package vp8enc

import (
	"math/rand"
	"testing"
)

// boolDecoder mirrors the arithmetic decoder at golang.org/x/image/vp8's
// partition type (BSD-3-Clause). The algorithm is specified in RFC 6386
// section 7.3. We duplicate it here as a test oracle so our round-trip
// tests don't depend on any unexported type from x/image/vp8.
type boolDecoder struct {
	buf     []byte
	r       int
	rangeM1 uint32
	bits    uint32
	nBits   uint8
	eof     bool
}

func newBoolDecoder(buf []byte) *boolDecoder {
	return &boolDecoder{buf: buf, rangeM1: 254}
}

func (d *boolDecoder) readBit(prob uint8) bool {
	if d.nBits < 8 {
		if d.r >= len(d.buf) {
			d.eof = true
			return false
		}
		x := uint32(d.buf[d.r])
		d.bits |= x << (8 - d.nBits)
		d.r++
		d.nBits += 8
	}
	split := (d.rangeM1*uint32(prob))>>8 + 1
	bit := d.bits >= split<<8
	if bit {
		d.rangeM1 -= split
		d.bits -= split << 8
	} else {
		d.rangeM1 = split - 1
	}
	if d.rangeM1 < 127 {
		// Fast renormalization using the decoder LUTs. For parity with
		// x/image/vp8 we use the same shift/new-range derivation, but
		// expressed via VP8Norm (which includes the 0..255 range — for
		// rangeM1 < 127, VP8Norm[rangeM1+1] matches x/image/vp8's
		// lutShift[rangeM1]).
		shift := uint8(VP8Norm[d.rangeM1+1])
		// Update rangeM1 by shifting left and setting low bits to 1
		// (equivalent to (rangeM1+1 << shift) - 1).
		newRange := (d.rangeM1 + 1) << shift
		d.rangeM1 = newRange - 1
		d.bits <<= shift
		d.nBits -= shift
	}
	return bit
}

// TestBoolCoderRoundtripDeterministic writes a fixed sequence of (bit, prob)
// pairs through the encoder, then verifies the matching decoder recovers
// the same bits in order.
func TestBoolCoderRoundtripDeterministic(t *testing.T) {
	type sample struct {
		bit  int
		prob int
	}
	// Hand-picked sequence exercising low/high probs, runs, and bit mixes.
	seq := []sample{
		{0, 128}, {1, 128}, {0, 128}, {1, 128},
		{0, 255}, {0, 255}, {0, 255},
		{1, 1}, {1, 1}, {1, 1},
		{0, 10}, {1, 200}, {0, 200}, {1, 10},
		{0, 64}, {1, 192}, {1, 64}, {0, 192},
	}
	enc := NewBoolEncoder()
	for _, s := range seq {
		enc.WriteBit(s.bit, s.prob)
	}
	payload := enc.Finish()

	dec := newBoolDecoder(payload)
	for i, s := range seq {
		got := dec.readBit(uint8(s.prob))
		want := s.bit == 1
		if got != want {
			t.Fatalf("bit %d: got %v, want %v (prob=%d)", i, got, want, s.prob)
		}
	}
	if dec.eof {
		t.Fatalf("unexpected EOF after reading valid bits")
	}
}

// TestBoolCoderRoundtripRandom pushes a large random stream of
// (bit, prob) pairs through the encoder and verifies every bit decodes
// correctly. This is the workhorse test that catches renorm and carry bugs.
func TestBoolCoderRoundtripRandom(t *testing.T) {
	r := rand.New(rand.NewSource(0xcafef00d))
	const N = 200000

	bits := make([]int, N)
	probs := make([]int, N)
	enc := NewBoolEncoder()
	for i := 0; i < N; i++ {
		// Exercise a mix of probs, including the extremes.
		p := r.Intn(255) + 1 // [1, 255]
		probs[i] = p
		b := 0
		if r.Intn(256) >= p {
			// "1" is less likely when prob is high.
			b = 1
		}
		bits[i] = b
		enc.WriteBit(b, p)
	}
	payload := enc.Finish()
	t.Logf("encoded %d bits into %d bytes (%.3f bits/bit)",
		N, len(payload), float64(len(payload)*8)/float64(N))

	dec := newBoolDecoder(payload)
	for i := 0; i < N; i++ {
		got := dec.readBit(uint8(probs[i]))
		want := bits[i] == 1
		if got != want {
			t.Fatalf("bit %d: got %v, want %v (prob=%d)", i, got, want, probs[i])
		}
		if dec.eof {
			t.Fatalf("unexpected EOF at bit %d", i)
		}
	}
}

// TestBoolCoderUniformRoundtrip verifies WriteUint/readUint equivalence.
func TestBoolCoderUniformRoundtrip(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	enc := NewBoolEncoder()
	type item struct {
		v uint32
		n int
	}
	var items []item
	for i := 0; i < 1000; i++ {
		n := r.Intn(16) + 1
		v := uint32(r.Intn(1 << uint(n)))
		items = append(items, item{v, n})
		enc.WriteUint(v, n, UniformProb)
	}
	payload := enc.Finish()

	dec := newBoolDecoder(payload)
	for i, it := range items {
		var got uint32
		for bit := it.n - 1; bit >= 0; bit-- {
			if dec.readBit(UniformProb) {
				got |= 1 << uint(bit)
			}
		}
		if got != it.v {
			t.Fatalf("item %d: got %d, want %d (n=%d)", i, got, it.v, it.n)
		}
	}
}

// TestBoolCoderExtremeProbs verifies stability at prob=1 and prob=255.
// These force aggressive renormalization and are where carry bugs lurk.
func TestBoolCoderExtremeProbs(t *testing.T) {
	cases := []struct {
		name string
		bits []int
		prob int
	}{
		{"low-prob-all-zero", repeat(0, 1000), 1},
		{"low-prob-all-one", repeat(1, 1000), 1},
		{"high-prob-all-zero", repeat(0, 1000), 255},
		{"high-prob-all-one", repeat(1, 1000), 255},
		{"mid-prob-alternating", alternating(1000), 128},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc := NewBoolEncoder()
			for _, b := range c.bits {
				enc.WriteBit(b, c.prob)
			}
			payload := enc.Finish()

			dec := newBoolDecoder(payload)
			for i, b := range c.bits {
				got := dec.readBit(uint8(c.prob))
				want := b == 1
				if got != want {
					t.Fatalf("bit %d: got %v want %v", i, got, want)
				}
			}
		})
	}
}

// TestBoolCoderReset confirms Reset leaves the encoder in a state equivalent
// to a fresh instance.
func TestBoolCoderReset(t *testing.T) {
	enc := NewBoolEncoder()
	for i := 0; i < 100; i++ {
		enc.WriteBit(i&1, 128)
	}
	enc.Finish()
	enc.Reset()

	for i := 0; i < 50; i++ {
		enc.WriteBit(1, 200)
	}
	payload := enc.Finish()

	dec := newBoolDecoder(payload)
	for i := 0; i < 50; i++ {
		got := dec.readBit(200)
		if !got {
			t.Fatalf("bit %d: expected 1", i)
		}
	}
}

func repeat(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func alternating(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i & 1
	}
	return out
}

// FuzzBoolCoderRoundtrip exercises the encoder/decoder pair with random
// inputs. Any decoder mismatch indicates a carry or renormalization bug.
func FuzzBoolCoderRoundtrip(f *testing.F) {
	f.Add([]byte{0x55, 0xaa, 0x00, 0xff, 0x01, 0x80, 0xc3})
	f.Add([]byte{0x00, 0x00, 0x00})
	f.Add([]byte{0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		enc := NewBoolEncoder()
		type pair struct{ b, p int }
		var pairs []pair
		// Interpret each byte as (bit = low bit, prob = remaining 7 bits mapped to [1,255]).
		for _, d := range data {
			prob := int(d>>1)*2 + 1 // odd in [1, 255]
			if prob < 1 {
				prob = 1
			}
			if prob > 255 {
				prob = 255
			}
			bit := int(d & 1)
			pairs = append(pairs, pair{bit, prob})
			enc.WriteBit(bit, prob)
		}
		payload := enc.Finish()
		dec := newBoolDecoder(payload)
		for i, p := range pairs {
			got := dec.readBit(uint8(p.p))
			want := p.b == 1
			if got != want {
				t.Fatalf("pair %d: got %v want %v (prob=%d)", i, got, want, p.p)
			}
		}
	})
}

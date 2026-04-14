package vp8enc

// BoolEncoder is the VP8 boolean arithmetic coder in write (encode) mode.
//
// The algorithm is specified in RFC 6386 section 7 and follows the libvpx
// reference implementation. The output byte sequence is designed to be
// decoded by the matching decoder described in section 7.3; we verify this
// with a round-trip test against a mirror decoder that is bit-compatible
// with golang.org/x/image/vp8's `partition` type.
//
// Invariants held between calls to WriteBit:
//   - rng (range) is in [128, 255]
//   - low holds up to 24 bits of "pending" output
//   - count tracks bits committed into low but not yet emitted;
//     initial value -24 means we need 24 shift-ins before the first emit.
//
// Carry propagation: when an emitted byte would overflow due to a late
// addition in low, we propagate a +1 back through any preceding 0xff bytes,
// turning them into 0x00 and incrementing the first non-0xff byte before
// the run.
type BoolEncoder struct {
	buf   []byte
	low   uint32
	rng   uint32
	count int32
}

// NewBoolEncoder returns a freshly initialized encoder writing to an
// internal buffer. Retrieve the output via Bytes after calling Finish.
func NewBoolEncoder() *BoolEncoder {
	return &BoolEncoder{
		rng:   255,
		count: -24,
	}
}

// Reset re-initializes the encoder to a fresh state, reusing the backing
// byte slice's capacity.
func (e *BoolEncoder) Reset() {
	e.buf = e.buf[:0]
	e.low = 0
	e.rng = 255
	e.count = -24
}

// WriteBit encodes a single bit against the given probability.
//
// prob is an 8-bit probability in [0, 255] where higher values bias toward
// the "0" branch being taken. 128 is the 50/50 (uniform) probability.
func (e *BoolEncoder) WriteBit(bit int, prob int) {
	split := uint32(1) + (((e.rng - 1) * uint32(prob)) >> 8)
	if bit != 0 {
		e.low += split
		e.rng -= split
	} else {
		e.rng = split
	}
	shift := int32(VP8Norm[e.rng])
	e.rng <<= shift
	e.count += shift
	if e.count >= 0 {
		offset := shift - e.count
		if offset >= 1 && (e.low<<uint32(offset-1))&0x80000000 != 0 {
			// Carry: propagate +1 back through the buffer, flipping 0xff
			// bytes to 0x00 and incrementing the first byte before them.
			x := len(e.buf) - 1
			for x >= 0 && e.buf[x] == 0xff {
				e.buf[x] = 0
				x--
			}
			if x >= 0 {
				e.buf[x]++
			}
			// If x < 0 we would propagate past the start of the buffer.
			// This cannot happen in a well-formed encoder because the
			// initial low is 0 and the first emitted byte is always < 0x80.
		}
		e.buf = append(e.buf, byte(e.low>>(24-uint32(offset))))
		e.low = (e.low << uint32(offset)) & 0xffffff
		shift = e.count
		e.count -= 8
	}
	e.low <<= uint32(shift)
}

// WriteUint encodes n bits of value v high-bit first, each against prob.
func (e *BoolEncoder) WriteUint(v uint32, n int, prob int) {
	for n > 0 {
		n--
		bit := int((v >> uint(n)) & 1)
		e.WriteBit(bit, prob)
	}
}

// WriteFlag encodes a 1-bit flag against uniform probability (128).
func (e *BoolEncoder) WriteFlag(v bool) {
	bit := 0
	if v {
		bit = 1
	}
	e.WriteBit(bit, UniformProb)
}

// WriteInt encodes a signed value as magnitude + sign.
// Signed values are stored as |v| in n bits followed by a sign bit.
// Each bit is coded against prob (typically UniformProb).
func (e *BoolEncoder) WriteInt(v int32, n int, prob int) {
	u := v
	sign := 0
	if v < 0 {
		u = -v
		sign = 1
	}
	e.WriteUint(uint32(u), n, prob)
	e.WriteBit(sign, prob)
}

// WriteOptionalInt encodes a possibly-zero signed value. When v == 0 a
// single "not present" bit is emitted; otherwise a "present" bit followed
// by the magnitude+sign encoding.
func (e *BoolEncoder) WriteOptionalInt(v int32, n int, prob int) {
	if v == 0 {
		e.WriteBit(0, prob)
		return
	}
	e.WriteBit(1, prob)
	e.WriteInt(v, n, prob)
}

// Finish flushes any pending bits and returns the encoded byte stream.
// It emits enough trailing zero bits at prob 128 to drain the internal
// state, so every previously written bit is recoverable by the decoder.
//
// The returned slice is the encoder's internal buffer; callers that want
// to retain it across subsequent encodes must copy it.
func (e *BoolEncoder) Finish() []byte {
	// Push out any committed bits still in `low` by writing 32 dummy 0
	// bits at uniform probability. This is the approach used by libvpx's
	// vp8_stop_encode.
	for i := 0; i < 32; i++ {
		e.WriteBit(0, UniformProb)
	}
	return e.buf
}

// Bytes returns the currently encoded bytes without flushing.
// Only meaningful for diagnostics; call Finish for a decodable stream.
func (e *BoolEncoder) Bytes() []byte {
	return e.buf
}

// Len returns the number of bytes emitted so far.
func (e *BoolEncoder) Len() int {
	return len(e.buf)
}

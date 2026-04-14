package vp8enc

import (
	"math/rand"
	"testing"
)

// TestDCTZeroInput verifies that an all-zero residual produces coefficients
// whose magnitude is below any realistic quantizer threshold. The libvpx
// reference FDCT isn't strictly zero-preserving because pass-1 rounding
// biases (14500, 7500 with >>12) leak through, but any leakage must be
// <= 1 in absolute value — smaller than any quantizer step.
func TestDCTZeroInput(t *testing.T) {
	var src, dst [16]int16
	FDCT4x4(src[:], dst[:])
	for i, v := range dst {
		if v < -1 || v > 1 {
			t.Errorf("dst[%d] = %d, want |v| <= 1 (bias leakage only)", i, v)
		}
	}
}

// TestDCTConstantInput verifies that a constant residual produces only a
// non-zero DC term, with AC coefficients all zero (within the rounding
// tolerance of integer arithmetic).
func TestDCTConstantInput(t *testing.T) {
	var src [16]int16
	for i := range src {
		src[i] = 100
	}
	var dst [16]int16
	FDCT4x4(src[:], dst[:])
	// DC should be approximately 100 * 16 (sum), scaled by the fixed-point
	// constants. We just assert it's the largest coefficient.
	if dst[0] == 0 {
		t.Fatal("expected non-zero DC")
	}
	for i := 1; i < 16; i++ {
		if abs16(dst[i]) > abs16(dst[0])/10 {
			t.Errorf("AC dst[%d] = %d is too large relative to DC %d", i, dst[i], dst[0])
		}
	}
}

// TestDCTRoundtrip confirms that forward → inverse recovers the original
// residual within a small error bound for random input.
func TestDCTRoundtrip(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	worst := int16(0)
	for trial := 0; trial < 200; trial++ {
		var src, coef, rec [16]int16
		for i := range src {
			src[i] = int16(r.Intn(511) - 255) // in [-255, 255]
		}
		FDCT4x4(src[:], coef[:])
		IDCT4x4(coef[:], rec[:])
		for i := 0; i < 16; i++ {
			d := src[i] - rec[i]
			if d < 0 {
				d = -d
			}
			if d > worst {
				worst = d
			}
			if d > 2 {
				t.Errorf("trial %d pos %d: src=%d rec=%d diff=%d",
					trial, i, src[i], rec[i], d)
			}
		}
	}
	t.Logf("worst per-pixel DCT roundtrip error: %d", worst)
}

// TestWHTRoundtrip confirms forward→inverse recovery for the Walsh-Hadamard.
func TestWHTRoundtrip(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	for trial := 0; trial < 200; trial++ {
		var src, coef, rec [16]int16
		for i := range src {
			src[i] = int16(r.Intn(2049) - 1024) // in [-1024, 1024]
		}
		FWHT4x4(src[:], coef[:])
		IWHT4x4(coef[:], rec[:])
		for i := 0; i < 16; i++ {
			d := src[i] - rec[i]
			if d < 0 {
				d = -d
			}
			if d > 2 {
				t.Errorf("trial %d pos %d: src=%d rec=%d diff=%d",
					trial, i, src[i], rec[i], d)
			}
		}
	}
}

// TestQuantizerFactors checks that NewQuantizer matches the known values
// spec'd for a few canonical QI values.
func TestQuantizerFactors(t *testing.T) {
	q := NewQuantizer(0)
	if q.Y1[0] != 4 || q.Y1[1] != 4 {
		t.Errorf("QI=0: Y1 = (%d, %d), want (4, 4)", q.Y1[0], q.Y1[1])
	}
	if q.Y2[0] != 8 {
		t.Errorf("QI=0: Y2 DC = %d, want 8", q.Y2[0])
	}
	if q.Y2[1] < 8 {
		t.Errorf("QI=0: Y2 AC = %d, want >= 8", q.Y2[1])
	}

	q = NewQuantizer(127)
	if q.Y1[0] != DequantTableDC[127] {
		t.Errorf("QI=127: Y1 DC = %d, want %d", q.Y1[0], DequantTableDC[127])
	}
	// UV DC must be clipped at index 117.
	if q.UV[0] != DequantTableDC[117] {
		t.Errorf("QI=127: UV DC = %d, want %d (clipped)", q.UV[0], DequantTableDC[117])
	}
}

// TestQuantizeRoundtrip verifies that quantize→dequantize loses at most
// ~half a dequant step per coefficient.
func TestQuantizeRoundtrip(t *testing.T) {
	r := rand.New(rand.NewSource(17))
	var src [16]int16
	for i := range src {
		src[i] = int16(r.Intn(2001) - 1000)
	}
	var q, dq [16]int16
	dcFactor := uint16(10)
	acFactor := uint16(20)
	eob := QuantizeBlock(src[:], q[:], dq[:], dcFactor, acFactor, 0)
	for i := 0; i < 16; i++ {
		factor := int32(acFactor)
		if i == 0 {
			factor = int32(dcFactor)
		}
		diff := int32(src[i]) - int32(dq[i])
		if diff < 0 {
			diff = -diff
		}
		if diff > factor {
			t.Errorf("pos %d: src=%d dq=%d diff=%d > factor=%d",
				i, src[i], dq[i], diff, factor)
		}
	}
	if eob < 0 || eob > 16 {
		t.Errorf("EOB out of range: %d", eob)
	}
}

// TestQuantizeDeadzone verifies that a large deadzone produces more zeros.
func TestQuantizeDeadzone(t *testing.T) {
	var src [16]int16
	for i := range src {
		src[i] = 10
	}
	var q0, q1, dq0, dq1 [16]int16
	QuantizeBlock(src[:], q0[:], dq0[:], 8, 8, 0)
	QuantizeBlock(src[:], q1[:], dq1[:], 8, 8, 20)
	zeros0 := 0
	zeros1 := 0
	for i := 0; i < 16; i++ {
		if q0[i] == 0 {
			zeros0++
		}
		if q1[i] == 0 {
			zeros1++
		}
	}
	if zeros1 <= zeros0 {
		t.Errorf("high deadzone should produce more zeros: got %d vs %d", zeros1, zeros0)
	}
}

// TestFullDCTQuantizeRoundtrip exercises the full pipeline: residual →
// FDCT → quantize → dequantize → IDCT → reconstructed residual. Error
// should be bounded by the quantizer step.
func TestFullDCTQuantizeRoundtrip(t *testing.T) {
	r := rand.New(rand.NewSource(23))
	qt := NewQuantizer(40) // moderate Q
	worstErr := 0
	for trial := 0; trial < 100; trial++ {
		var src, coef, q, dq, rec [16]int16
		for i := range src {
			src[i] = int16(r.Intn(201) - 100) // residual in [-100, 100]
		}
		FDCT4x4(src[:], coef[:])
		QuantizeBlock(coef[:], q[:], dq[:], qt.Y1[0], qt.Y1[1], 0)
		IDCT4x4(dq[:], rec[:])
		for i := 0; i < 16; i++ {
			d := int(src[i]) - int(rec[i])
			if d < 0 {
				d = -d
			}
			if d > worstErr {
				worstErr = d
			}
		}
	}
	t.Logf("worst per-pixel error after DCT+Q+IDCT at QI=40: %d", worstErr)
	// At QI=40, dequant factor ~40, so errors of up to ~40 are normal.
	if worstErr > 80 {
		t.Errorf("worst error %d exceeds reasonable bound", worstErr)
	}
}

func abs16(x int16) int16 {
	if x < 0 {
		return -x
	}
	return x
}

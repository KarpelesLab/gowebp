package vp8enc

// Forward intra prediction for VP8 encoding.
//
// Predictors are pure functions: given an input block's neighbors (top row,
// left column, top-left corner) they produce the predicted pixel values
// which the encoder subtracts from the source to get residuals. The
// neighbors come from already-reconstructed (i.e. lossy) MB data, matching
// exactly what the decoder will see.
//
// This file implements I16 (16×16 luma block) and UV8 (8×8 chroma block)
// modes: DC, VE (vertical), HE (horizontal), TM (true-motion/Paeth-ish).
// The four I4 modes and six diagonal I4 modes land in a later phase.
//
// Reference: RFC 6386 chapter 12; cross-checked against x/image/vp8/predfunc.go.

// PredictI16 fills dst[0..16][0..16] (row-major, stride 16) with the
// predicted pixels of a 16x16 block, given the reconstructed neighbors:
//   - topLeft: single pixel at (x-1, y-1)
//   - topRow: 16 pixels along the top edge at (x..x+15, y-1)
//   - leftCol: 16 pixels along the left edge at (x-1, y..y+15)
//   - hasTop, hasLeft indicate whether those neighbors are valid MBs
//     (i.e. mby > 0 for top, mbx > 0 for left). When false, the
//     corresponding side uses the default filler (0x7f for top, 0x81 for
//     left) and DC mode switches to the appropriate variant.
func PredictI16(dst *[256]byte, mode int, topRow, leftCol *[16]byte, topLeft byte, hasTop, hasLeft bool) {
	switch mode {
	case ModeDC:
		var sum uint32
		cnt := uint32(0)
		if hasTop {
			for i := 0; i < 16; i++ {
				sum += uint32(topRow[i])
			}
			cnt += 16
		}
		if hasLeft {
			for j := 0; j < 16; j++ {
				sum += uint32(leftCol[j])
			}
			cnt += 16
		}
		var avg byte
		switch {
		case cnt == 32:
			avg = byte((sum + 16) >> 5)
		case cnt == 16:
			avg = byte((sum + 8) >> 4)
		default:
			avg = 0x80
		}
		for j := 0; j < 16; j++ {
			for i := 0; i < 16; i++ {
				dst[j*16+i] = avg
			}
		}

	case ModeVE:
		// Copy top row to every row.
		for j := 0; j < 16; j++ {
			for i := 0; i < 16; i++ {
				dst[j*16+i] = topRow[i]
			}
		}

	case ModeHE:
		for j := 0; j < 16; j++ {
			v := leftCol[j]
			for i := 0; i < 16; i++ {
				dst[j*16+i] = v
			}
		}

	case ModeTM:
		tl := int32(topLeft)
		for j := 0; j < 16; j++ {
			l := int32(leftCol[j])
			for i := 0; i < 16; i++ {
				t := int32(topRow[i])
				dst[j*16+i] = byte(clampInt32(t+l-tl, 0, 255))
			}
		}
	}
}

// PredictUV8 fills an 8x8 block with predicted chroma values. Semantics
// match PredictI16 but sized to 8.
func PredictUV8(dst *[64]byte, mode int, topRow, leftCol *[8]byte, topLeft byte, hasTop, hasLeft bool) {
	switch mode {
	case ModeDC:
		var sum uint32
		cnt := uint32(0)
		if hasTop {
			for i := 0; i < 8; i++ {
				sum += uint32(topRow[i])
			}
			cnt += 8
		}
		if hasLeft {
			for j := 0; j < 8; j++ {
				sum += uint32(leftCol[j])
			}
			cnt += 8
		}
		var avg byte
		switch {
		case cnt == 16:
			avg = byte((sum + 8) >> 4)
		case cnt == 8:
			avg = byte((sum + 4) >> 3)
		default:
			avg = 0x80
		}
		for j := 0; j < 8; j++ {
			for i := 0; i < 8; i++ {
				dst[j*8+i] = avg
			}
		}

	case ModeVE:
		for j := 0; j < 8; j++ {
			for i := 0; i < 8; i++ {
				dst[j*8+i] = topRow[i]
			}
		}

	case ModeHE:
		for j := 0; j < 8; j++ {
			v := leftCol[j]
			for i := 0; i < 8; i++ {
				dst[j*8+i] = v
			}
		}

	case ModeTM:
		tl := int32(topLeft)
		for j := 0; j < 8; j++ {
			l := int32(leftCol[j])
			for i := 0; i < 8; i++ {
				t := int32(topRow[i])
				dst[j*8+i] = byte(clampInt32(t+l-tl, 0, 255))
			}
		}
	}
}

// SumSquaredError returns the sum of squared differences between two
// equal-length byte slices, reinterpreted as pixel values. Used for mode
// selection (min-SSE search).
func SumSquaredError(a, b []byte) int64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum int64
	for i := 0; i < n; i++ {
		d := int64(a[i]) - int64(b[i])
		sum += d * d
	}
	return sum
}

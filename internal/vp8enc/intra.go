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

// PredictI4 fills dst with the 16 predicted pixels of one 4x4 sub-block
// for the given mode. Neighbors:
//   - tl: top-left corner pixel (at y-1, x-1)
//   - top[0..3]: 4 pixels directly above (at y-1, x..x+3)
//   - top[4..7]: 4 pixels to the top-right (at y-1, x+4..x+7);
//     for right-column sub-blocks these are the MB-level overhang values
//     (RFC 6386 section 12.2 / x/image/vp8 prepareYBR's column-24..27 fill)
//   - left[0..3]: 4 pixels along the left column (at y..y+3, x-1)
func PredictI4(dst *[16]byte, mode int, tl byte, top *[8]byte, left *[4]byte) {
	a := int32(tl)
	b := int32(top[0])
	c := int32(top[1])
	d := int32(top[2])
	e := int32(top[3])
	f := int32(top[4])
	g := int32(top[5])
	h := int32(top[6])
	h2 := int32(top[7]) // eighth column pixel; used by LD for g+2h+h averaging
	_ = h2              // (h_extra not all modes need it; keep for parity)
	p := int32(left[0])
	q := int32(left[1])
	r := int32(left[2])
	s := int32(left[3])

	setRow := func(row int, v0, v1, v2, v3 byte) {
		dst[row*4+0] = v0
		dst[row*4+1] = v1
		dst[row*4+2] = v2
		dst[row*4+3] = v3
	}

	switch mode {
	case ModeI4DC:
		avg := byte((b + c + d + e + p + q + r + s + 4) >> 3)
		for i := 0; i < 16; i++ {
			dst[i] = avg
		}

	case ModeI4TM:
		for j := 0; j < 4; j++ {
			lj := int32(left[j])
			for i := 0; i < 4; i++ {
				ti := int32(top[i])
				dst[j*4+i] = byte(clampInt32(ti+lj-a, 0, 255))
			}
		}

	case ModeI4VE:
		abc := byte((a + 2*b + c + 2) >> 2)
		bcd := byte((b + 2*c + d + 2) >> 2)
		cde := byte((c + 2*d + e + 2) >> 2)
		def := byte((d + 2*e + f + 2) >> 2)
		for j := 0; j < 4; j++ {
			setRow(j, abc, bcd, cde, def)
		}

	case ModeI4HE:
		apq := byte((a + 2*p + q + 2) >> 2)
		pqr := byte((p + 2*q + r + 2) >> 2)
		qrs := byte((q + 2*r + s + 2) >> 2)
		rss := byte((r + 2*s + s + 2) >> 2)
		for i := 0; i < 4; i++ {
			dst[0*4+i] = apq
			dst[1*4+i] = pqr
			dst[2*4+i] = qrs
			dst[3*4+i] = rss
		}

	case ModeI4RD:
		srq := byte((s + 2*r + q + 2) >> 2)
		rqp := byte((r + 2*q + p + 2) >> 2)
		qpa := byte((q + 2*p + a + 2) >> 2)
		pab := byte((p + 2*a + b + 2) >> 2)
		abc := byte((a + 2*b + c + 2) >> 2)
		bcd := byte((b + 2*c + d + 2) >> 2)
		cde := byte((c + 2*d + e + 2) >> 2)
		setRow(0, pab, abc, bcd, cde)
		setRow(1, qpa, pab, abc, bcd)
		setRow(2, rqp, qpa, pab, abc)
		setRow(3, srq, rqp, qpa, pab)

	case ModeI4VR:
		ab := byte((a + b + 1) >> 1)
		bc := byte((b + c + 1) >> 1)
		cd := byte((c + d + 1) >> 1)
		de := byte((d + e + 1) >> 1)
		rqp := byte((r + 2*q + p + 2) >> 2)
		qpa := byte((q + 2*p + a + 2) >> 2)
		pab := byte((p + 2*a + b + 2) >> 2)
		abc := byte((a + 2*b + c + 2) >> 2)
		bcd := byte((b + 2*c + d + 2) >> 2)
		cde := byte((c + 2*d + e + 2) >> 2)
		setRow(0, ab, bc, cd, de)
		setRow(1, pab, abc, bcd, cde)
		setRow(2, qpa, ab, bc, cd)
		setRow(3, rqp, pab, abc, bcd)

	case ModeI4LD:
		abc := byte((b + 2*c + d + 2) >> 2)
		bcd := byte((c + 2*d + e + 2) >> 2)
		cde := byte((d + 2*e + f + 2) >> 2)
		def := byte((e + 2*f + g + 2) >> 2)
		efg := byte((f + 2*g + h + 2) >> 2)
		fgh := byte((g + 2*h + h2 + 2) >> 2)
		ghh := byte((h + 2*h2 + h2 + 2) >> 2)
		setRow(0, abc, bcd, cde, def)
		setRow(1, bcd, cde, def, efg)
		setRow(2, cde, def, efg, fgh)
		setRow(3, def, efg, fgh, ghh)

	case ModeI4VL:
		ab := byte((b + c + 1) >> 1)
		bc := byte((c + d + 1) >> 1)
		cd := byte((d + e + 1) >> 1)
		de := byte((e + f + 1) >> 1)
		abc := byte((b + 2*c + d + 2) >> 2)
		bcd := byte((c + 2*d + e + 2) >> 2)
		cde := byte((d + 2*e + f + 2) >> 2)
		def := byte((e + 2*f + g + 2) >> 2)
		efg := byte((f + 2*g + h + 2) >> 2)
		fgh := byte((g + 2*h + h2 + 2) >> 2)
		setRow(0, ab, bc, cd, de)
		setRow(1, abc, bcd, cde, def)
		setRow(2, bc, cd, de, efg)
		setRow(3, bcd, cde, def, fgh)

	case ModeI4HD:
		sr := byte((s + r + 1) >> 1)
		rq := byte((r + q + 1) >> 1)
		qp := byte((q + p + 1) >> 1)
		pa := byte((p + a + 1) >> 1)
		srq := byte((s + 2*r + q + 2) >> 2)
		rqp := byte((r + 2*q + p + 2) >> 2)
		qpa := byte((q + 2*p + a + 2) >> 2)
		pab := byte((p + 2*a + b + 2) >> 2)
		abc := byte((a + 2*b + c + 2) >> 2)
		bcd := byte((b + 2*c + d + 2) >> 2)
		setRow(0, pa, pab, abc, bcd)
		setRow(1, qp, qpa, pa, pab)
		setRow(2, rq, rqp, qp, qpa)
		setRow(3, sr, srq, rq, rqp)

	case ModeI4HU:
		pq := byte((p + q + 1) >> 1)
		qr := byte((q + r + 1) >> 1)
		rs := byte((r + s + 1) >> 1)
		pqr := byte((p + 2*q + r + 2) >> 2)
		qrs := byte((q + 2*r + s + 2) >> 2)
		rss := byte((r + 2*s + s + 2) >> 2)
		sss := byte(s)
		setRow(0, pq, pqr, qr, qrs)
		setRow(1, qr, qrs, rs, rss)
		setRow(2, rs, rss, sss, sss)
		setRow(3, sss, sss, sss, sss)
	}
}

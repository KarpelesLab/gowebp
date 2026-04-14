package vp8enc

// DCT/WHT coefficient token encoding (RFC 6386 chapter 13).
//
// Coefficients are emitted in VP8 zigzag order through the boolean coder
// against a context-adaptive probability model indexed by (plane, band,
// previous-coef-category). The model is the token-tree defined in section
// 13.2. This encoder is the exact inverse of parseResiduals4 in
// golang.org/x/image/vp8/reconstruct.go — if the encoder and decoder walk
// the same tree in the same order against the same probability row, the
// bitstream roundtrips.
//
// The bands table has 17 entries to handle n=16 during EOB tail-matching
// after position 15. The extra entry is unused for probability lookup in
// the inner loop but the decoder reads bands[16]=0 for the trailing check
// that our encoder mirrors.
var bandsExt = [17]uint8{0, 1, 2, 3, 6, 4, 5, 6, 6, 6, 6, 6, 6, 6, 6, 7, 0}

// Category-3/4/5/6 extra-bit probability tables (RFC 6386 section 13.2,
// cross-checked against x/image/vp8/reconstruct.go's cat3456).
var cat3Bits = [3]uint8{173, 148, 140}
var cat4Bits = [4]uint8{176, 155, 140, 135}
var cat5Bits = [5]uint8{180, 157, 141, 134, 130}
var cat6Bits = [11]uint8{254, 254, 243, 230, 196, 177, 153, 140, 133, 130, 129}

// WriteCoefBlock encodes 16 quantized coefficients (raster order) for one
// 4x4 block. Returns 1 if the block had any non-zero coefficient, 0
// otherwise — matches the decoder's return value, which the caller
// propagates as the "non-zero" context for subsequent blocks.
//
// coefs is indexed by raster (0..15); the zigzag mapping is applied
// internally. plane selects the probability plane; context is 0/1/2 from
// neighbor non-zero counts; tokenProb is the current probability table
// (we always use DefaultTokenProb, but passed in for future prob-update
// support). skipFirstCoeff == true for I16 Y1 blocks (DC is in the Y2
// block, not this one), starting from zigzag position 1.
func WriteCoefBlock(
	enc *BoolEncoder,
	coefs *[16]int16,
	plane, context int,
	tokenProb *[NumPlanes][NumBands][NumContexts][NumProbs]uint8,
	skipFirstCoeff bool,
) int {
	start := 0
	if skipFirstCoeff {
		start = 1
	}

	// Find the index (in zigzag order) of the last non-zero coefficient.
	lastNZ := -1
	for i := start; i < 16; i++ {
		z := Zigzag4x4[i]
		if coefs[z] != 0 {
			lastNZ = i
		}
	}

	p := &tokenProb[plane][bandsExt[start]][context]

	// Leading EOB check at the block's first zigzag position.
	if lastNZ < 0 {
		enc.WriteBit(0, int((*p)[0]))
		return 0
	}
	enc.WriteBit(1, int((*p)[0]))

	n := start
	for n < 16 {
		z := Zigzag4x4[n]
		v := int32(coefs[z])
		absV := v
		if absV < 0 {
			absV = -absV
		}

		if absV == 0 {
			enc.WriteBit(0, int((*p)[1]))
			p = &tokenProb[plane][bandsExt[n+1]][0]
			n++
			continue
		}

		// Non-zero.
		enc.WriteBit(1, int((*p)[1]))

		var nextCtx int
		if absV == 1 {
			enc.WriteBit(0, int((*p)[2]))
			nextCtx = 1
		} else {
			enc.WriteBit(1, int((*p)[2]))
			writeValueGE2(enc, int(absV), p)
			nextCtx = 2
		}

		// Sign bit.
		signBit := 0
		if v < 0 {
			signBit = 1
		}
		enc.WriteBit(signBit, UniformProb)

		// EOB marker after this coefficient, unless we're at the very end.
		if n == lastNZ {
			if n+1 < 16 {
				// Encode EOB against the NEXT position's prob row.
				pEOB := &tokenProb[plane][bandsExt[n+1]][nextCtx]
				enc.WriteBit(0, int((*pEOB)[0]))
			}
			return 1
		}
		// Not the last non-zero — emit "continue" bit and advance.
		pNext := &tokenProb[plane][bandsExt[n+1]][nextCtx]
		enc.WriteBit(1, int((*pNext)[0]))
		p = pNext
		n++
	}
	return 1
}

// writeValueGE2 emits the magnitude of a quantized coefficient with
// absolute value >= 2. It mirrors the decoder's value-tree traversal:
//
//	2             : 0 0
//	3, 4          : 0 1 X               (cat 0a/0b via p[5])
//	5, 6          : 1 0 0 X             (cat 1 via fixed 159)
//	7..10         : 1 0 1 X X           (cat 2 via 165, 145)
//	11..2114      : 1 1 <cat> <bits>    (cat 3..6)
//
// p is the current position's probability row.
func writeValueGE2(enc *BoolEncoder, v int, p *[NumProbs]uint8) {
	switch {
	case v == 2:
		enc.WriteBit(0, int(p[3]))
		enc.WriteBit(0, int(p[4]))

	case v == 3:
		enc.WriteBit(0, int(p[3]))
		enc.WriteBit(1, int(p[4]))
		enc.WriteBit(0, int(p[5]))
	case v == 4:
		enc.WriteBit(0, int(p[3]))
		enc.WriteBit(1, int(p[4]))
		enc.WriteBit(1, int(p[5]))

	case v == 5 || v == 6:
		enc.WriteBit(1, int(p[3]))
		enc.WriteBit(0, int(p[6]))
		enc.WriteBit(0, int(p[7]))
		enc.WriteBit(v-5, 159)

	case v >= 7 && v <= 10:
		enc.WriteBit(1, int(p[3]))
		enc.WriteBit(0, int(p[6]))
		enc.WriteBit(1, int(p[7]))
		r := v - 7
		enc.WriteBit((r>>1)&1, 165)
		enc.WriteBit(r&1, 145)

	case v >= 11 && v <= 18:
		writeCategoryHeader(enc, p, 0)
		writeExtraBits(enc, v-11, cat3Bits[:])
	case v >= 19 && v <= 34:
		writeCategoryHeader(enc, p, 1)
		writeExtraBits(enc, v-19, cat4Bits[:])
	case v >= 35 && v <= 66:
		writeCategoryHeader(enc, p, 2)
		writeExtraBits(enc, v-35, cat5Bits[:])
	case v >= 67:
		// Cat 6 nominally caps at 2048 per RFC; clamp for safety.
		if v > 2048 {
			v = 2048
		}
		writeCategoryHeader(enc, p, 3)
		writeExtraBits(enc, v-67, cat6Bits[:])
	}
}

// writeCategoryHeader emits the 4 tree-selector bits that identify
// categories 3..6. cat ∈ {0,1,2,3} corresponds to RFC categories 3..6.
func writeCategoryHeader(enc *BoolEncoder, p *[NumProbs]uint8, cat int) {
	enc.WriteBit(1, int(p[3]))
	enc.WriteBit(1, int(p[6]))
	b1 := (cat >> 1) & 1
	b0 := cat & 1
	enc.WriteBit(b1, int(p[8]))
	enc.WriteBit(b0, int(p[9+b1]))
}

// writeExtraBits emits the magnitude bits of a category (3..6) in
// MSB-first order against the per-category fixed probabilities.
func writeExtraBits(enc *BoolEncoder, remainder int, probs []uint8) {
	n := len(probs)
	for i := 0; i < n; i++ {
		bit := (remainder >> (n - 1 - i)) & 1
		enc.WriteBit(bit, int(probs[i]))
	}
}

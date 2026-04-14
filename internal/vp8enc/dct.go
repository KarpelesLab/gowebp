package vp8enc

// This file implements the forward and inverse transforms used by VP8:
//   - 4x4 integer DCT on 4x4 spatial blocks of residuals
//   - 4x4 Walsh-Hadamard (WHT) on the 16 Y DC coefficients of an I16 MB
//
// Both transforms are bit-exact to the libvpx reference (RFC 6386 sections
// 14.3 and 14.4). Output coefficients are stored in row-major 4x4 order
// (index 0 = top-left DC, index 1 = top-right, etc.).

// FDCT constants: approximations of 2*cos(pi/8)*sqrt(2) and related terms
// as rational multipliers, chosen so all intermediate products fit in
// int32 and the inverse matches.
const (
	fdctC1 = 2217
	fdctC2 = 5352
)

// FDCT4x4 performs a forward 4x4 DCT on 16 residual values in src and
// writes 16 coefficients to dst. Both slices are row-major 4x4 blocks.
//
// This is the libvpx vp8_short_fdct4x4_c form, which pairs exactly with the
// libvpx IDCT implemented below (and with the decoder at
// golang.org/x/image/vp8/idct.go).
//
// Known property: because the pass-1 rounding biases (14500, 7500) exceed
// their >>12 divisor, an all-zero residual produces tiny non-zero outputs
// in some odd-harmonic positions. These values are smaller than any
// realistic quantizer and quantize to 0 in practice; encoders rely on the
// "skip" flag to avoid emitting empty blocks.
//
// Input residual range is [-510, 510] (signed 10-bit). Output fits in int16.
func FDCT4x4(src []int16, dst []int16) {
	_ = src[15]
	_ = dst[15]
	var tmp [16]int32

	// Pass 1: rows.
	for i := 0; i < 4; i++ {
		r := i * 4
		d0 := int32(src[r+0])
		d1 := int32(src[r+1])
		d2 := int32(src[r+2])
		d3 := int32(src[r+3])

		a1 := (d0 + d3) << 3
		b1 := (d1 + d2) << 3
		c1 := (d1 - d2) << 3
		d1d := (d0 - d3) << 3

		tmp[r+0] = a1 + b1
		tmp[r+2] = a1 - b1
		tmp[r+1] = (c1*fdctC1 + d1d*fdctC2 + 14500) >> 12
		tmp[r+3] = (d1d*fdctC1 - c1*fdctC2 + 7500) >> 12
	}

	// Pass 2: columns.
	for i := 0; i < 4; i++ {
		a1 := tmp[i+0] + tmp[i+12]
		b1 := tmp[i+4] + tmp[i+8]
		c1 := tmp[i+4] - tmp[i+8]
		d1 := tmp[i+0] - tmp[i+12]

		dst[i+0] = int16((a1 + b1 + 7) >> 4)
		dst[i+8] = int16((a1 - b1 + 7) >> 4)

		col1 := (c1*fdctC1 + d1*fdctC2 + 12000) >> 16
		if d1 != 0 {
			col1++
		}
		dst[i+4] = int16(col1)
		dst[i+12] = int16((d1*fdctC1 - c1*fdctC2 + 51000) >> 16)
	}
}

// iDCT constants.
const (
	sinpi8sqrt2       = 35468
	cospi8sqrt2minus1 = 20091
)

// IDCT4x4 performs the inverse 4x4 DCT on 16 coefficients in src, writing
// 16 reconstructed residuals to dst. Output residuals may overshoot the
// input range by small amounts and must be clipped to [0, 255] after
// adding prediction.
func IDCT4x4(src []int16, dst []int16) {
	_ = src[15]
	_ = dst[15]
	var tmp [16]int32

	// Pass 1: rows.
	for i := 0; i < 4; i++ {
		r := i * 4
		i0 := int32(src[r+0])
		i1 := int32(src[r+1])
		i2 := int32(src[r+2])
		i3 := int32(src[r+3])

		a := i0 + i2
		b := i0 - i2
		c := (i1 * sinpi8sqrt2) >> 16
		c -= i3 + ((i3 * cospi8sqrt2minus1) >> 16)
		d := i1 + ((i1 * cospi8sqrt2minus1) >> 16)
		d += (i3 * sinpi8sqrt2) >> 16

		tmp[r+0] = a + d
		tmp[r+3] = a - d
		tmp[r+1] = b + c
		tmp[r+2] = b - c
	}

	// Pass 2: columns.
	for i := 0; i < 4; i++ {
		i0 := tmp[i+0]
		i1 := tmp[i+4]
		i2 := tmp[i+8]
		i3 := tmp[i+12]

		a := i0 + i2
		b := i0 - i2
		c := (i1 * sinpi8sqrt2) >> 16
		c -= i3 + ((i3 * cospi8sqrt2minus1) >> 16)
		d := i1 + ((i1 * cospi8sqrt2minus1) >> 16)
		d += (i3 * sinpi8sqrt2) >> 16

		dst[i+0] = int16((a + d + 4) >> 3)
		dst[i+12] = int16((a - d + 4) >> 3)
		dst[i+4] = int16((b + c + 4) >> 3)
		dst[i+8] = int16((b - c + 4) >> 3)
	}
}

// FWHT4x4 performs the forward Walsh-Hadamard transform on the 16 DC
// coefficients of an I16 macroblock's Y plane. Input is the 16 DC values
// (one per 4x4 Y sub-block, in raster order); output is the Y2 block.
func FWHT4x4(src []int16, dst []int16) {
	_ = src[15]
	_ = dst[15]
	var tmp [16]int32

	// Pass 1: rows.
	for i := 0; i < 4; i++ {
		r := i * 4
		d0 := int32(src[r+0])
		d1 := int32(src[r+1])
		d2 := int32(src[r+2])
		d3 := int32(src[r+3])

		a1 := (d0 + d2) << 2
		d1d := (d1 + d3) << 2
		c1 := (d1 - d3) << 2
		b1 := (d0 - d2) << 2

		op0 := a1 + d1d
		if a1 != 0 {
			op0++
		}
		tmp[r+0] = op0
		tmp[r+1] = b1 + c1
		tmp[r+2] = b1 - c1
		tmp[r+3] = a1 - d1d
	}

	// Pass 2: columns.
	for i := 0; i < 4; i++ {
		a1 := tmp[i+0] + tmp[i+8]
		d1 := tmp[i+4] + tmp[i+12]
		c1 := tmp[i+4] - tmp[i+12]
		b1 := tmp[i+0] - tmp[i+8]

		a2 := a1 + d1
		b2 := b1 + c1
		c2 := b1 - c1
		d2 := a1 - d1

		if a2 < 0 {
			a2++
		}
		if b2 < 0 {
			b2++
		}
		if c2 < 0 {
			c2++
		}
		if d2 < 0 {
			d2++
		}

		dst[i+0] = int16((a2 + 3) >> 3)
		dst[i+4] = int16((b2 + 3) >> 3)
		dst[i+8] = int16((c2 + 3) >> 3)
		dst[i+12] = int16((d2 + 3) >> 3)
	}
}

// IWHT4x4 performs the inverse WHT on the 16 Y2 coefficients, producing
// 16 reconstructed Y-DC values in dst.
func IWHT4x4(src []int16, dst []int16) {
	_ = src[15]
	_ = dst[15]
	var tmp [16]int32

	// Pass 1: rows.
	for i := 0; i < 4; i++ {
		r := i * 4
		d0 := int32(src[r+0])
		d1 := int32(src[r+1])
		d2 := int32(src[r+2])
		d3 := int32(src[r+3])

		a := d0 + d3
		b := d1 + d2
		c := d1 - d2
		d := d0 - d3

		tmp[r+0] = a + b
		tmp[r+1] = d + c
		tmp[r+2] = a - b
		tmp[r+3] = d - c
	}

	// Pass 2: columns.
	for i := 0; i < 4; i++ {
		a := tmp[i+0] + tmp[i+12]
		b := tmp[i+4] + tmp[i+8]
		c := tmp[i+4] - tmp[i+8]
		d := tmp[i+0] - tmp[i+12]

		dst[i+0] = int16((a + b + 3) >> 3)
		dst[i+4] = int16((d + c + 3) >> 3)
		dst[i+8] = int16((a - b + 3) >> 3)
		dst[i+12] = int16((d - c + 3) >> 3)
	}
}

// Quantizer bundles the six VP8 dequant factors needed to (de)quantize
// Y, Y2, and UV blocks. Indices: [0] = DC factor, [1] = AC factor.
type Quantizer struct {
	Y1 [2]uint16 // Y AC blocks
	Y2 [2]uint16 // Y DC (Walsh) block
	UV [2]uint16 // Chroma AC blocks
}

// NewQuantizer derives dequant factors from a base quantizer index in
// [0, 127] using the exact lookup + adjustments specified in RFC 6386
// section 9.6 (mirrored from x/image/vp8 quant.go).
func NewQuantizer(qi int) Quantizer {
	qi = clampInt(qi, 0, 127)
	q := Quantizer{}
	q.Y1[0] = DequantTableDC[qi]
	q.Y1[1] = DequantTableAC[qi]
	q.Y2[0] = DequantTableDC[qi] * 2
	q.Y2[1] = DequantTableAC[qi] * 155 / 100
	if q.Y2[1] < 8 {
		q.Y2[1] = 8
	}
	// UV DC is clipped to dequantTableDC[117] = 132 by the spec's
	// reference decoder.
	uvDCIdx := qi
	if uvDCIdx > 117 {
		uvDCIdx = 117
	}
	q.UV[0] = DequantTableDC[uvDCIdx]
	q.UV[1] = DequantTableAC[qi]
	return q
}

// QuantizeBlock performs deadzone quantization of 16 coefficients given
// (dcFactor, acFactor) dequant values and a deadzone bias. It writes the
// quantized integer coefficients to qOut and the reconstructed (dequantized)
// coefficients to dqOut. Returns the end-of-block index (1 + position of
// last non-zero coefficient in zigzag order) or 0 for an all-zero block.
//
// deadzone is a bias subtracted from the absolute coefficient value before
// division. A typical choice is factor/4 for "lossy" mode; larger values
// increase zeros (smaller files, lower quality).
func QuantizeBlock(
	coef []int16, qOut []int16, dqOut []int16,
	dcFactor, acFactor uint16, deadzone int32,
) int {
	_ = coef[15]
	_ = qOut[15]
	_ = dqOut[15]
	eob := 0
	for i := 0; i < 16; i++ {
		rc := Zigzag4x4[i]
		f := int32(acFactor)
		if rc == 0 {
			f = int32(dcFactor)
		}
		c := int32(coef[rc])
		sign := int32(1)
		abs := c
		if c < 0 {
			sign = -1
			abs = -c
		}
		abs -= deadzone
		if abs < 0 {
			qOut[rc] = 0
			dqOut[rc] = 0
			continue
		}
		q := (abs + f/2) / f
		if q == 0 {
			qOut[rc] = 0
			dqOut[rc] = 0
			continue
		}
		qs := int16(sign * q)
		qOut[rc] = qs
		dqOut[rc] = int16(int32(qs) * f)
		eob = i + 1
	}
	return eob
}

// DequantizeBlock reconstructs coefficients from a quantized block in the
// zigzag-ordered input `q`. Writes to `coef` in raster 4x4 order.
// (This is the operation the decoder performs implicitly via the coefficient
// stream; exposed here for encoder-side reconstruction / RDO.)
func DequantizeBlock(q []int16, coef []int16, dcFactor, acFactor uint16) {
	_ = q[15]
	_ = coef[15]
	for i := 0; i < 16; i++ {
		rc := Zigzag4x4[i]
		f := int32(acFactor)
		if rc == 0 {
			f = int32(dcFactor)
		}
		coef[rc] = int16(int32(q[rc]) * f)
	}
}

package vp8enc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
)

// EncodeOptions carries VP8 frame-level tuning that the package-level
// encoder wrapper translates from nativewebp.Options.
type EncodeOptions struct {
	// Quality maps 0..100 to the VP8 base quantizer index; higher Quality
	// → lower QI → finer quantization. Default 75.
	Quality float32
	// Method is the speed/quality tradeoff; 0 = fastest, 6 = slowest.
	// Currently only toggles whether mode search explores all 4 I16/UV8
	// modes (>=1) or hard-selects DC (0).
	Method int
}

const (
	// VP8 image dimension is stored in 14 bits, so the cap is one less than
	// VP8L's 16384-pixel cap.
	MaxDimension = (1 << 14) - 1
)

// EncodeFrame writes a single VP8 keyframe (no RIFF/VP8 chunk wrapper) to w.
// Callers are responsible for wrapping the output in a VP8 chunk header
// and a RIFF container when building a full .webp file.
//
// The current encoder implements I16 + UV8 prediction (DC/V/H/TM) with
// forward DCT + Walsh-Hadamard, deadzone quantization, and token-tree
// coefficient coding. Mode selection is SSE-based. I4 modes (10 per MB)
// and rate-distortion optimization are later-phase work.
func EncodeFrame(w io.Writer, img image.Image, opts EncodeOptions) error {
	if img == nil {
		return errors.New("vp8enc: nil image")
	}
	b := img.Bounds()
	wd, ht := b.Dx(), b.Dy()
	if wd < 1 || ht < 1 {
		return errors.New("vp8enc: empty image bounds")
	}
	if wd > MaxDimension || ht > MaxDimension {
		return errors.New("vp8enc: image exceeds VP8 max dimension 16383")
	}

	frame := RGBAToFrame(img)

	baseQ := qualityToQI(opts.Quality)
	quant := NewQuantizer(baseQ)

	enc := newEncState(frame, quant, opts)
	enc.encodeAllMBs()

	// Partition 0: frame-level header + per-MB modes.
	p0 := NewBoolEncoder()
	WriteHeaderInit(p0)
	WriteSegmentHeaderOff(p0)
	WriteFilterHeaderOff(p0)
	WriteLog2NumParts(p0, 0)
	WriteQuantHeader(p0, baseQ)
	WriteRefreshEntropyProbs(p0)
	WriteTokenProbUpdates(p0)
	// No skip probability (every MB emits residuals, even if zero).
	WriteSkipProb(p0, false, 0)

	for i, mb := range enc.mbs {
		_ = i
		WriteMBModes(p0, mb.yMode, mb.uvMode)
	}

	p0Bytes := p0.Finish()
	p1Bytes := enc.p1.Finish()

	fps := uint32(len(p0Bytes))
	if fps >= 1<<19 {
		return errors.New("vp8enc: first partition exceeds 19-bit size field")
	}
	tag := uint32(0) | (0 << 1) | (1 << 4) | (fps << 5)

	var header [10]byte
	header[0] = byte(tag)
	header[1] = byte(tag >> 8)
	header[2] = byte(tag >> 16)
	header[3] = 0x9d
	header[4] = 0x01
	header[5] = 0x2a
	header[6] = byte(wd)
	header[7] = byte(wd >> 8)
	header[8] = byte(ht)
	header[9] = byte(ht >> 8)

	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(p0Bytes); err != nil {
		return err
	}
	if _, err := w.Write(p1Bytes); err != nil {
		return err
	}
	return nil
}

// EncodeWebP wraps EncodeFrame in the RIFF/WEBP/VP8 container format.
func EncodeWebP(w io.Writer, img image.Image, opts EncodeOptions) error {
	var vp8 bytes.Buffer
	if err := EncodeFrame(&vp8, img, opts); err != nil {
		return err
	}
	chunkLen := vp8.Len()
	padded := chunkLen
	if padded&1 == 1 {
		padded++
	}
	total := 4 + 4 + 4 + padded

	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(total)); err != nil {
		return err
	}
	if _, err := w.Write([]byte("WEBP")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("VP8 ")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(chunkLen)); err != nil {
		return err
	}
	if _, err := w.Write(vp8.Bytes()); err != nil {
		return err
	}
	if padded != chunkLen {
		if _, err := w.Write([]byte{0}); err != nil {
			return err
		}
	}
	return nil
}

// mbDecision stores the mode and per-MB neighbor flags needed by the
// partition-0 emitter after the residual pass. We encode residuals during
// the MB walk (to keep reconstruction local) but the mode bits live in
// partition 0 and must be emitted at the end.
type mbDecision struct {
	yMode, uvMode int
}

// encState orchestrates frame encoding. It maintains source and
// reconstructed YCbCr planes, the partition-1 boolean coder for residual
// tokens, per-MB mode decisions, and the non-zero neighbor masks needed
// to pick the right token-probability context.
type encState struct {
	opts  EncodeOptions
	frame *Frame
	quant Quantizer

	reconY  []byte
	reconCb []byte
	reconCr []byte

	p1  *BoolEncoder
	mbs []mbDecision

	// Non-zero tracking for token contexts (RFC 6386 section 13.3).
	// Each byte holds 4 luma + 2 Cb + 2 Cr nz bits in an 8-bit mask.
	leftNZ   uint8
	upNZ     []uint8
	leftNZY2 uint8
	upNZY2   []uint8
}

func newEncState(frame *Frame, quant Quantizer, opts EncodeOptions) *encState {
	return &encState{
		opts:    opts,
		frame:   frame,
		quant:   quant,
		reconY:  make([]byte, len(frame.Y)),
		reconCb: make([]byte, len(frame.Cb)),
		reconCr: make([]byte, len(frame.Cr)),
		p1:      NewBoolEncoder(),
		mbs:     make([]mbDecision, frame.MBWidth*frame.MBHeight),
		upNZ:    make([]uint8, frame.MBWidth),
		upNZY2:  make([]uint8, frame.MBWidth),
	}
}

// encodeAllMBs walks every macroblock in raster order, picks intra modes
// by SSE, performs the full predict→residual→transform→quantize→recon
// pipeline, and emits residual tokens to the partition-1 coder.
func (s *encState) encodeAllMBs() {
	for mby := 0; mby < s.frame.MBHeight; mby++ {
		s.leftNZ = 0
		s.leftNZY2 = 0
		for mbx := 0; mbx < s.frame.MBWidth; mbx++ {
			s.encodeOneMB(mbx, mby)
		}
	}
}

// --- Neighbor accessors -----------------------------------------------
//
// The decoder fills neighbor defaults of 0x7f (top) / 0x81 (left) / 0x7f
// (top-left) for frame-edge MBs (see prepareYBR in x/image/vp8). The
// encoder must produce the same prediction inputs, so we mirror those
// defaults exactly here and read from the reconstruction planes otherwise.

func (s *encState) getYTopRow(mbx, mby int, out *[16]byte) bool {
	if mby == 0 {
		for i := 0; i < 16; i++ {
			out[i] = 0x7f
		}
		return false
	}
	base := (mby*16-1)*s.frame.YStride + mbx*16
	for i := 0; i < 16; i++ {
		out[i] = s.reconY[base+i]
	}
	return true
}

func (s *encState) getYLeftCol(mbx, mby int, out *[16]byte) bool {
	if mbx == 0 {
		for j := 0; j < 16; j++ {
			out[j] = 0x81
		}
		return false
	}
	for j := 0; j < 16; j++ {
		out[j] = s.reconY[(mby*16+j)*s.frame.YStride+mbx*16-1]
	}
	return true
}

func (s *encState) getYTopLeft(mbx, mby int) byte {
	switch {
	case mbx == 0 && mby == 0:
		return 0x7f
	case mbx == 0:
		return 0x81
	case mby == 0:
		return 0x7f
	default:
		return s.reconY[(mby*16-1)*s.frame.YStride+mbx*16-1]
	}
}

func (s *encState) getUVTopRow(plane byte, mbx, mby int, out *[8]byte) bool {
	if mby == 0 {
		for i := 0; i < 8; i++ {
			out[i] = 0x7f
		}
		return false
	}
	var src []byte
	if plane == 'U' {
		src = s.reconCb
	} else {
		src = s.reconCr
	}
	base := (mby*8-1)*s.frame.UVStride + mbx*8
	for i := 0; i < 8; i++ {
		out[i] = src[base+i]
	}
	return true
}

func (s *encState) getUVLeftCol(plane byte, mbx, mby int, out *[8]byte) bool {
	if mbx == 0 {
		for j := 0; j < 8; j++ {
			out[j] = 0x81
		}
		return false
	}
	var src []byte
	if plane == 'U' {
		src = s.reconCb
	} else {
		src = s.reconCr
	}
	for j := 0; j < 8; j++ {
		out[j] = src[(mby*8+j)*s.frame.UVStride+mbx*8-1]
	}
	return true
}

func (s *encState) getUVTopLeft(plane byte, mbx, mby int) byte {
	switch {
	case mbx == 0 && mby == 0:
		return 0x7f
	case mbx == 0:
		return 0x81
	case mby == 0:
		return 0x7f
	}
	var src []byte
	if plane == 'U' {
		src = s.reconCb
	} else {
		src = s.reconCr
	}
	return src[(mby*8-1)*s.frame.UVStride+mbx*8-1]
}

// --- Per-MB encode ----------------------------------------------------

func (s *encState) encodeOneMB(mbx, mby int) {
	// 1. Pick Y mode by SSE.
	yMode := s.pickYMode(mbx, mby)

	// 2. Pick UV mode by SSE.
	uvMode := s.pickUVMode(mbx, mby)

	s.mbs[mby*s.frame.MBWidth+mbx] = mbDecision{yMode: yMode, uvMode: uvMode}

	// 3. Build the Y predictor and residuals.
	var yTop, yLeft [16]byte
	hasTop := s.getYTopRow(mbx, mby, &yTop)
	hasLeft := s.getYLeftCol(mbx, mby, &yLeft)
	yTL := s.getYTopLeft(mbx, mby)

	var yPred [256]byte
	PredictI16(&yPred, yMode, &yTop, &yLeft, yTL, hasTop, hasLeft)

	// 4. Compute residuals per 4x4 sub-block (16 sub-blocks in Y).
	var yRes [16][16]int16
	for sby := 0; sby < 4; sby++ {
		for sbx := 0; sbx < 4; sbx++ {
			subIdx := sby*4 + sbx
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					px := mbx*16 + sbx*4 + i
					py := mby*16 + sby*4 + j
					pred := int16(yPred[(sby*4+j)*16+sbx*4+i])
					src := int16(s.frame.Y[py*s.frame.YStride+px])
					yRes[subIdx][j*4+i] = src - pred
				}
			}
		}
	}

	// 5. Forward 4x4 DCT on each Y sub-block.
	var yCoef [16][16]int16
	for s_ := 0; s_ < 16; s_++ {
		FDCT4x4(yRes[s_][:], yCoef[s_][:])
	}

	// 6. Extract the 16 DC coefficients and WHT them.
	var y2In, y2Coef [16]int16
	for s_ := 0; s_ < 16; s_++ {
		y2In[s_] = yCoef[s_][0]
	}
	FWHT4x4(y2In[:], y2Coef[:])

	// 7. Quantize Y2 block.
	var y2Q, y2DQ [16]int16
	dz := int32(s.quant.Y2[1]) / 4
	QuantizeBlock(y2Coef[:], y2Q[:], y2DQ[:], s.quant.Y2[0], s.quant.Y2[1], dz)

	// 8. Quantize Y1 AC (skipping DC — it's in Y2).
	var y1Q, y1DQ [16][16]int16
	for s_ := 0; s_ < 16; s_++ {
		// DC coefficient is not emitted via the Y1 block path, so zero
		// it in the input to the quantizer to avoid polluting the AC
		// coeffs. However the dequantized DC for reconstruction comes
		// from the Y2 block (below).
		yCoef[s_][0] = 0
		dzAC := int32(s.quant.Y1[1]) / 4
		QuantizeBlock(yCoef[s_][:], y1Q[s_][:], y1DQ[s_][:],
			s.quant.Y1[0], s.quant.Y1[1], dzAC)
	}

	// 9. Emit Y2 tokens to partition 1.
	ctxY2 := int(s.leftNZY2 + s.upNZY2[mbx])
	y2NZ := WriteCoefBlock(s.p1, &y2Q, PlaneY2, ctxY2, &DefaultTokenProb, false)
	s.leftNZY2 = uint8(y2NZ)
	s.upNZY2[mbx] = uint8(y2NZ)

	// 10. Emit 16 Y1 blocks (plane = Y1WithY2, skipFirstCoeff=true).
	lnzY := unpackNibble(s.leftNZ & 0x0f)
	unzY := unpackNibble(s.upNZ[mbx] & 0x0f)
	for sby := 0; sby < 4; sby++ {
		nzLeft := lnzY[sby]
		for sbx := 0; sbx < 4; sbx++ {
			ctx := int(nzLeft + unzY[sbx])
			idx := sby*4 + sbx
			nz := WriteCoefBlock(s.p1, &y1Q[idx], PlaneY1WithY2, ctx,
				&DefaultTokenProb, true)
			nzLeft = uint8(nz)
			unzY[sbx] = uint8(nz)
		}
		lnzY[sby] = nzLeft
	}
	newLeftY := packNibble(lnzY)
	newUpY := packNibble(unzY)

	// 11. Chroma: Cb then Cr (4 blocks each).
	var cbTop, cbLeft [8]byte
	cbHasTop := s.getUVTopRow('U', mbx, mby, &cbTop)
	cbHasLeft := s.getUVLeftCol('U', mbx, mby, &cbLeft)
	cbTL := s.getUVTopLeft('U', mbx, mby)
	var cbPred [64]byte
	PredictUV8(&cbPred, uvMode, &cbTop, &cbLeft, cbTL, cbHasTop, cbHasLeft)

	var crTop, crLeft [8]byte
	crHasTop := s.getUVTopRow('V', mbx, mby, &crTop)
	crHasLeft := s.getUVLeftCol('V', mbx, mby, &crLeft)
	crTL := s.getUVTopLeft('V', mbx, mby)
	var crPred [64]byte
	PredictUV8(&crPred, uvMode, &crTop, &crLeft, crTL, crHasTop, crHasLeft)

	// Cb residuals + transform + quantize + emit.
	var cbRes, cbCoef, cbQ, cbDQ [4][16]int16
	for sby := 0; sby < 2; sby++ {
		for sbx := 0; sbx < 2; sbx++ {
			subIdx := sby*2 + sbx
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					px := mbx*8 + sbx*4 + i
					py := mby*8 + sby*4 + j
					pred := int16(cbPred[(sby*4+j)*8+sbx*4+i])
					src := int16(s.frame.Cb[py*s.frame.UVStride+px])
					cbRes[subIdx][j*4+i] = src - pred
				}
			}
			FDCT4x4(cbRes[subIdx][:], cbCoef[subIdx][:])
			dzUV := int32(s.quant.UV[1]) / 4
			QuantizeBlock(cbCoef[subIdx][:], cbQ[subIdx][:], cbDQ[subIdx][:],
				s.quant.UV[0], s.quant.UV[1], dzUV)
		}
	}

	var crRes, crCoef, crQ, crDQ [4][16]int16
	for sby := 0; sby < 2; sby++ {
		for sbx := 0; sbx < 2; sbx++ {
			subIdx := sby*2 + sbx
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					px := mbx*8 + sbx*4 + i
					py := mby*8 + sby*4 + j
					pred := int16(crPred[(sby*4+j)*8+sbx*4+i])
					src := int16(s.frame.Cr[py*s.frame.UVStride+px])
					crRes[subIdx][j*4+i] = src - pred
				}
			}
			FDCT4x4(crRes[subIdx][:], crCoef[subIdx][:])
			dzUV := int32(s.quant.UV[1]) / 4
			QuantizeBlock(crCoef[subIdx][:], crQ[subIdx][:], crDQ[subIdx][:],
				s.quant.UV[0], s.quant.UV[1], dzUV)
		}
	}

	// Emit Cb tokens, then Cr tokens.
	// The decoder's chroma nz masks are in bits 4..7 of nzMask (Cb=4-5, Cr=6-7 in 2x2 layout).
	lnzUV := unpackNibble(s.leftNZ >> 4)
	unzUV := unpackNibble(s.upNZ[mbx] >> 4)

	// Cb: 2x2 blocks at positions (y,x)=(0,0),(0,1),(1,0),(1,1).
	// nz mask layout in the mbNzMask: Cb at bits 4-5 (and corresponding rows),
	// Cr at bits 6-7. Follow parseResiduals layout.
	for sby := 0; sby < 2; sby++ {
		nzLeft := lnzUV[sby]
		for sbx := 0; sbx < 2; sbx++ {
			ctx := int(nzLeft + unzUV[sbx])
			idx := sby*2 + sbx
			nz := WriteCoefBlock(s.p1, &cbQ[idx], PlaneUV, ctx, &DefaultTokenProb, false)
			nzLeft = uint8(nz)
			unzUV[sbx] = uint8(nz)
		}
		lnzUV[sby] = nzLeft
	}
	// Cr uses the upper 2 bits of each 4-bit nibble.
	for sby := 0; sby < 2; sby++ {
		nzLeft := lnzUV[sby+2]
		for sbx := 0; sbx < 2; sbx++ {
			ctx := int(nzLeft + unzUV[sbx+2])
			idx := sby*2 + sbx
			nz := WriteCoefBlock(s.p1, &crQ[idx], PlaneUV, ctx, &DefaultTokenProb, false)
			nzLeft = uint8(nz)
			unzUV[sbx+2] = uint8(nz)
		}
		lnzUV[sby+2] = nzLeft
	}
	newLeftUV := packNibble(lnzUV)
	newUpUV := packNibble(unzUV)

	s.leftNZ = (newLeftUV << 4) | newLeftY
	s.upNZ[mbx] = (newUpUV << 4) | newUpY

	// 12. Reconstruct Y: IWHT → add DC back to each Y1 → IDCT → + pred → clip.
	var y2Rec [16]int16
	IWHT4x4(y2DQ[:], y2Rec[:])
	for s_ := 0; s_ < 16; s_++ {
		y1DQ[s_][0] = y2Rec[s_]
	}
	var yResRec [16][16]int16
	for s_ := 0; s_ < 16; s_++ {
		IDCT4x4(y1DQ[s_][:], yResRec[s_][:])
	}
	for sby := 0; sby < 4; sby++ {
		for sbx := 0; sbx < 4; sbx++ {
			subIdx := sby*4 + sbx
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					px := mbx*16 + sbx*4 + i
					py := mby*16 + sby*4 + j
					pred := int32(yPred[(sby*4+j)*16+sbx*4+i])
					res := int32(yResRec[subIdx][j*4+i])
					v := pred + res
					s.reconY[py*s.frame.YStride+px] = byte(clampInt32(v, 0, 255))
				}
			}
		}
	}

	// 13. Reconstruct chroma.
	for sby := 0; sby < 2; sby++ {
		for sbx := 0; sbx < 2; sbx++ {
			subIdx := sby*2 + sbx
			var cbResRec, crResRec [16]int16
			IDCT4x4(cbDQ[subIdx][:], cbResRec[:])
			IDCT4x4(crDQ[subIdx][:], crResRec[:])
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					px := mbx*8 + sbx*4 + i
					py := mby*8 + sby*4 + j
					cbP := int32(cbPred[(sby*4+j)*8+sbx*4+i])
					crP := int32(crPred[(sby*4+j)*8+sbx*4+i])
					cbV := cbP + int32(cbResRec[j*4+i])
					crV := crP + int32(crResRec[j*4+i])
					s.reconCb[py*s.frame.UVStride+px] = byte(clampInt32(cbV, 0, 255))
					s.reconCr[py*s.frame.UVStride+px] = byte(clampInt32(crV, 0, 255))
				}
			}
		}
	}
}

// pickYMode returns the I16 mode (DC/VE/HE/TM) with lowest SSE against the
// source luma block. Method=0 shortcuts to DC.
func (s *encState) pickYMode(mbx, mby int) int {
	if s.opts.Method <= 0 {
		return ModeDC
	}
	var top, left [16]byte
	hasTop := s.getYTopRow(mbx, mby, &top)
	hasLeft := s.getYLeftCol(mbx, mby, &left)
	tl := s.getYTopLeft(mbx, mby)

	var src [256]byte
	for j := 0; j < 16; j++ {
		for i := 0; i < 16; i++ {
			src[j*16+i] = s.frame.Y[(mby*16+j)*s.frame.YStride+mbx*16+i]
		}
	}

	best := ModeDC
	bestSSE := int64(-1)
	modes := []int{ModeDC, ModeVE, ModeHE, ModeTM}
	for _, m := range modes {
		// VE and HE require the respective neighbor; fall back gracefully.
		if m == ModeVE && !hasTop {
			continue
		}
		if m == ModeHE && !hasLeft {
			continue
		}
		if m == ModeTM && (!hasTop || !hasLeft) {
			continue
		}
		var pred [256]byte
		PredictI16(&pred, m, &top, &left, tl, hasTop, hasLeft)
		sse := SumSquaredError(src[:], pred[:])
		if bestSSE < 0 || sse < bestSSE {
			bestSSE = sse
			best = m
		}
	}
	return best
}

func (s *encState) pickUVMode(mbx, mby int) int {
	if s.opts.Method <= 0 {
		return ModeDC
	}
	var cbTop, cbLeft, crTop, crLeft [8]byte
	cbHasTop := s.getUVTopRow('U', mbx, mby, &cbTop)
	cbHasLeft := s.getUVLeftCol('U', mbx, mby, &cbLeft)
	cbTL := s.getUVTopLeft('U', mbx, mby)
	crHasTop := s.getUVTopRow('V', mbx, mby, &crTop)
	crHasLeft := s.getUVLeftCol('V', mbx, mby, &crLeft)
	crTL := s.getUVTopLeft('V', mbx, mby)

	var cbSrc, crSrc [64]byte
	for j := 0; j < 8; j++ {
		for i := 0; i < 8; i++ {
			cbSrc[j*8+i] = s.frame.Cb[(mby*8+j)*s.frame.UVStride+mbx*8+i]
			crSrc[j*8+i] = s.frame.Cr[(mby*8+j)*s.frame.UVStride+mbx*8+i]
		}
	}

	best := ModeDC
	bestSSE := int64(-1)
	modes := []int{ModeDC, ModeVE, ModeHE, ModeTM}
	for _, m := range modes {
		if m == ModeVE && (!cbHasTop || !crHasTop) {
			continue
		}
		if m == ModeHE && (!cbHasLeft || !crHasLeft) {
			continue
		}
		if m == ModeTM && (!cbHasTop || !cbHasLeft || !crHasTop || !crHasLeft) {
			continue
		}
		var cbPred, crPred [64]byte
		PredictUV8(&cbPred, m, &cbTop, &cbLeft, cbTL, cbHasTop, cbHasLeft)
		PredictUV8(&crPred, m, &crTop, &crLeft, crTL, crHasTop, crHasLeft)
		sse := SumSquaredError(cbSrc[:], cbPred[:]) + SumSquaredError(crSrc[:], crPred[:])
		if bestSSE < 0 || sse < bestSSE {
			bestSSE = sse
			best = m
		}
	}
	return best
}

// unpackNibble returns the 4 bits of mask as 0/1 uint8 values, LSB first.
func unpackNibble(mask uint8) [4]uint8 {
	return [4]uint8{
		mask & 1,
		(mask >> 1) & 1,
		(mask >> 2) & 1,
		(mask >> 3) & 1,
	}
}

// packNibble packs 4 0/1 values back into a single nibble, LSB first.
func packNibble(v [4]uint8) uint8 {
	return v[0] | (v[1] << 1) | (v[2] << 2) | (v[3] << 3)
}

// qualityToQI maps a 0..100 quality setting to a 0..127 base quantizer
// index using a monotonically decreasing curve: Q=100 → QI=0 (finest),
// Q=0 → QI=127 (coarsest). Default Q=75 → QI≈32, a reasonable midpoint.
func qualityToQI(quality float32) int {
	if quality <= 0 {
		return 127
	}
	if quality >= 100 {
		return 0
	}
	qi := int((100.0 - quality) * 127.0 / 100.0)
	if qi < 0 {
		qi = 0
	}
	if qi > 127 {
		qi = 127
	}
	return qi
}

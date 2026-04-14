package vp8enc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"math"
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
	// Enable the normal loop filter with a level derived from the
	// quantizer. Larger QI (coarser quant) needs a stronger filter to
	// hide block boundaries. libwebp uses a similar linear mapping.
	filterLevel := baseQ/8 + 2
	if filterLevel > 63 {
		filterLevel = 63
	}
	WriteFilterHeader(p0, false, filterLevel, 0)
	WriteLog2NumParts(p0, 0)
	WriteQuantHeader(p0, baseQ)
	WriteRefreshEntropyProbs(p0)
	WriteTokenProbUpdates(p0)
	// Enable per-MB skip bit. Calibrate skipProb ≈ P(skip=0) × 255
	// against the actual skip rate of this frame so non-skipping MBs
	// (the common case) cost fewer bits. Clamped to [8, 247] to
	// avoid degenerate extreme probabilities.
	skipped := 0
	for _, mb := range enc.mbs {
		if mb.skip {
			skipped++
		}
	}
	total := len(enc.mbs)
	skipProb := uint8(160)
	if total > 0 {
		p := int(float64(total-skipped) / float64(total) * 255)
		if p < 8 {
			p = 8
		}
		if p > 247 {
			p = 247
		}
		skipProb = uint8(p)
	}
	WriteSkipProb(p0, true, skipProb)

	// Partition-0 mode coding context. leftPredMode[j] is the last (right)
	// column of per-sub-block I4 modes in the MB immediately to the left
	// of the current MB (reset at the start of each MB row).
	// upPredMode[mbx*4+i] is the bottom-row of I4 modes for the MB at
	// column mbx, position i. Used as "above" context for subsequent MBs.
	upPredMode := make([]int, enc.frame.MBWidth*4)
	for mby := 0; mby < enc.frame.MBHeight; mby++ {
		leftPredMode := [4]int{ModeI4DC, ModeI4DC, ModeI4DC, ModeI4DC}
		for mbx := 0; mbx < enc.frame.MBWidth; mbx++ {
			mb := enc.mbs[mby*enc.frame.MBWidth+mbx]
			// Emit skip bit first, as the decoder expects. Must use
			// the same skipProb we wrote to the header.
			if mb.skip {
				p0.WriteBit(1, int(skipProb))
			} else {
				p0.WriteBit(0, int(skipProb))
			}
			if mb.isI16 {
				WriteMBModes(p0, mb.yMode, mb.uvMode)
				// I16 MBs propagate their Y mode as the context for all
				// 4 edge positions for neighbor MBs.
				fill := i16ToI4Context(mb.yMode)
				for i := 0; i < 4; i++ {
					upPredMode[mbx*4+i] = fill
					leftPredMode[i] = fill
				}
			} else {
				var above [4]int
				for i := 0; i < 4; i++ {
					above[i] = upPredMode[mbx*4+i]
				}
				left := leftPredMode
				WriteMBModesBPred(p0, &mb.i4Modes, &above, &left, mb.uvMode)
				for i := 0; i < 4; i++ {
					upPredMode[mbx*4+i] = above[i]
				}
				leftPredMode = left
			}
		}
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
	isI16   bool
	skip    bool      // true if all quantized coefs are zero — token emission skipped
	yMode   int       // I16 mode when isI16; ignored when B_PRED
	i4Modes [4][4]int // I4 modes per sub-block when !isI16
	uvMode  int
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
	// Pick UV mode (same for both I16 and B_PRED paths).
	uvMode := s.pickUVMode(mbx, mby)

	switch {
	case s.opts.Method >= 3:
		// Per-MB arbitration between I16 and B_PRED: pick lower SSE
		// with a rate penalty that covers the extra mode-coding bits
		// plus the loss of the compact Y2-WHT DC path on B_PRED MBs.
		yMode, i16SSE := s.bestI16(mbx, mby)
		bSSE := s.estimateBPredSSE(mbx, mby)
		qf := int64(s.quant.Y1[1])
		bPenalty := qf * qf * 2
		if i16SSE <= bSSE+bPenalty {
			s.encodeI16MB(mbx, mby, yMode, uvMode)
		} else {
			s.encodeBPredMB(mbx, mby, uvMode)
		}
		return
	case s.opts.Method == 2:
		s.encodeBPredMB(mbx, mby, uvMode)
		return
	}

	yMode := s.pickYMode(mbx, mby)
	s.encodeI16MB(mbx, mby, yMode, uvMode)
}

// encodeI16MB performs the full I16 macroblock encode: predict,
// residual, FDCT + WHT, quantize, token emission, reconstruct.
// Extracted from the original encodeOneMB body to support per-MB
// arbitration (the caller has already chosen yMode and uvMode).
func (s *encState) encodeI16MB(mbx, mby int, yMode, uvMode int) {
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
	// Y2 holds only DC values (the WHT of Y sub-block DCs), so all 16
	// entries are "DC-like" and should use the DC deadzone.
	QuantizeBlockSplit(y2Coef[:], y2Q[:], y2DQ[:], s.quant.Y2[0], s.quant.Y2[1], 0, 0)

	// 8. Quantize Y1 AC (skipping DC — it's in Y2).
	var y1Q, y1DQ [16][16]int16
	for s_ := 0; s_ < 16; s_++ {
		// DC coefficient is not emitted via the Y1 block path, so zero
		// it in the input to the quantizer to avoid polluting the AC
		// coeffs. However the dequantized DC for reconstruction comes
		// from the Y2 block (below).
		yCoef[s_][0] = 0
		// DC was zeroed; AC gets a moderate deadzone to save entropy.
		QuantizeBlockSplit(yCoef[s_][:], y1Q[s_][:], y1DQ[s_][:],
			s.quant.Y1[0], s.quant.Y1[1], 0, int32(s.quant.Y1[1])/4)
	}

	// 9. Chroma: Cb then Cr (4 blocks each).
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

	// Chroma residuals + transform + quantize.
	var cbRes, cbCoef, cbQ, cbDQ [4][16]int16
	var crRes, crCoef, crQ, crDQ [4][16]int16
	for sby := 0; sby < 2; sby++ {
		for sbx := 0; sbx < 2; sbx++ {
			subIdx := sby*2 + sbx
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					px := mbx*8 + sbx*4 + i
					py := mby*8 + sby*4 + j
					cbRes[subIdx][j*4+i] = int16(s.frame.Cb[py*s.frame.UVStride+px]) -
						int16(cbPred[(sby*4+j)*8+sbx*4+i])
					crRes[subIdx][j*4+i] = int16(s.frame.Cr[py*s.frame.UVStride+px]) -
						int16(crPred[(sby*4+j)*8+sbx*4+i])
				}
			}
			FDCT4x4(cbRes[subIdx][:], cbCoef[subIdx][:])
			FDCT4x4(crRes[subIdx][:], crCoef[subIdx][:])
			dzUV := int32(0)
			QuantizeBlock(cbCoef[subIdx][:], cbQ[subIdx][:], cbDQ[subIdx][:],
				s.quant.UV[0], s.quant.UV[1], dzUV)
			QuantizeBlock(crCoef[subIdx][:], crQ[subIdx][:], crDQ[subIdx][:],
				s.quant.UV[0], s.quant.UV[1], dzUV)
		}
	}

	// 10. Decide skip: if every quantized coefficient is zero across
	// Y2, all 16 Y1, all 4 Cb, all 4 Cr blocks, we can emit skip=1 and
	// omit all residual tokens.
	skip := allZero16(&y2Q) && blocksAllZero16(&y1Q) &&
		blocksAllZero4(&cbQ) && blocksAllZero4(&crQ)

	s.mbs[mby*s.frame.MBWidth+mbx] = mbDecision{
		isI16: true, skip: skip, yMode: yMode, uvMode: uvMode,
	}

	if skip {
		// All-zero coefs: no tokens, and neighbor nz masks reset for
		// subsequent MBs (matches decoder's else-branch in reconstruct).
		s.leftNZY2 = 0
		s.upNZY2[mbx] = 0
		s.leftNZ = 0
		s.upNZ[mbx] = 0
	} else {
		// Emit Y2 tokens to partition 1.
		ctxY2 := int(s.leftNZY2 + s.upNZY2[mbx])
		y2NZ := WriteCoefBlock(s.p1, &y2Q, PlaneY2, ctxY2, &DefaultTokenProb, false)
		s.leftNZY2 = uint8(y2NZ)
		s.upNZY2[mbx] = uint8(y2NZ)

		// Emit 16 Y1 blocks (plane = Y1WithY2, skipFirstCoeff=true).
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

		// Chroma token emission.
		lnzUV := unpackNibble(s.leftNZ >> 4)
		unzUV := unpackNibble(s.upNZ[mbx] >> 4)
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
	}

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

// encodeBPredMB encodes a macroblock using the 16 independent 4x4 I4
// predictors. Sub-blocks are processed in raster order; each sub-block's
// prediction reads from the reconstruction of already-processed
// sub-blocks (within this MB) or from the reconY plane (outside this MB).
func (s *encState) encodeBPredMB(mbx, mby int, uvMode int) {
	// Build the MB's top row + 4-pixel overhang (20 pixels total) and
	// the 16-pixel left column. These are the pixels visible at the
	// "edges" of the MB; within-MB sub-block prediction uses inMB.
	var topRow [20]byte
	var leftCol [16]byte
	var corner byte = s.getYTopLeft(mbx, mby)

	if mby == 0 {
		for i := 0; i < 20; i++ {
			topRow[i] = 0x7f
		}
	} else {
		ybase := (mby*16 - 1) * s.frame.YStride
		for i := 0; i < 16; i++ {
			topRow[i] = s.reconY[ybase+mbx*16+i]
		}
		// Overhang (pixels 16..19) comes from the MB above-right if that
		// MB exists, else replicated from the last top pixel.
		if mbx == s.frame.MBWidth-1 {
			r := s.reconY[ybase+mbx*16+15]
			for i := 16; i < 20; i++ {
				topRow[i] = r
			}
		} else {
			for i := 0; i < 4; i++ {
				topRow[16+i] = s.reconY[ybase+mbx*16+16+i]
			}
		}
	}

	if mbx == 0 {
		for j := 0; j < 16; j++ {
			leftCol[j] = 0x81
		}
	} else {
		for j := 0; j < 16; j++ {
			leftCol[j] = s.reconY[(mby*16+j)*s.frame.YStride+mbx*16-1]
		}
	}

	// Reconstructed MB pixels (built up as we process sub-blocks).
	var inMB [16][16]byte

	// Per-sub-block mode and non-zero tracking. Token emission is
	// deferred until after the UV pass so we can decide skip over the
	// entire MB's quantized coefficient set.
	var modes [4][4]int
	var yQ [16][16]int16

	for sj := 0; sj < 4; sj++ {
		for si := 0; si < 4; si++ {
			// Build sub-block neighbors.
			var tl byte
			var top [8]byte
			var left [4]byte
			s.buildI4Neighbors(&tl, &top, &left, &inMB, &topRow, &leftCol, corner, si, sj)

			// Gather source pixels for this sub-block.
			var src [16]byte
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					src[j*4+i] = s.frame.Y[(mby*16+sj*4+j)*s.frame.YStride+mbx*16+si*4+i]
				}
			}

			// Min-SSE mode search over all 10 I4 modes. Edge
			// sub-blocks see the same defaults (0x7f top, 0x81 left)
			// that the decoder fills via prepareYBR, so every mode is
			// legally applicable. Any previous quality regression from
			// allowing all modes at edges was from the nzY16 bug
			// (fixed in 8fe2243) corrupting subsequent MB contexts.
			bestMode := ModeI4DC
			bestSSE := int64(-1)
			var bestPred [16]byte
			for m := 0; m < NumPredModes; m++ {
				var pred [16]byte
				PredictI4(&pred, m, tl, &top, &left)
				sse := SumSquaredError(src[:], pred[:])
				if bestSSE < 0 || sse < bestSSE {
					bestSSE = sse
					bestMode = m
					bestPred = pred
				}
			}
			modes[sj][si] = bestMode

			// Compute residual and run transform/quantize.
			var res, coef, dq [16]int16
			for k := 0; k < 16; k++ {
				res[k] = int16(src[k]) - int16(bestPred[k])
			}
			FDCT4x4(res[:], coef[:])
			// B_PRED has no Y2, so the DC of each 4x4 block IS emitted
			// through Y1SansY2. DC gets no deadzone; AC gets moderate.
			QuantizeBlockSplit(coef[:], yQ[sj*4+si][:], dq[:],
				s.quant.Y1[0], s.quant.Y1[1], 0, int32(s.quant.Y1[1])/4)

			// Reconstruct (needed for subsequent sub-blocks' prediction).
			var resRec [16]int16
			IDCT4x4(dq[:], resRec[:])
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					v := int32(bestPred[j*4+i]) + int32(resRec[j*4+i])
					inMB[sj*4+j][si*4+i] = byte(clampInt32(v, 0, 255))
				}
			}
		}
	}

	// Copy reconstructed Y into the recon plane.
	for j := 0; j < 16; j++ {
		for i := 0; i < 16; i++ {
			s.reconY[(mby*16+j)*s.frame.YStride+mbx*16+i] = inMB[j][i]
		}
	}

	// UV: predict + transform + quantize, still deferred emission.
	cbQ, crQ := s.quantizeMBChroma(mbx, mby, uvMode)

	// Decide skip over the full MB.
	skip := blocksAllZero16(&yQ) && blocksAllZero4(cbQ) && blocksAllZero4(crQ)

	s.mbs[mby*s.frame.MBWidth+mbx] = mbDecision{
		isI16: false, skip: skip, i4Modes: modes, uvMode: uvMode,
	}

	// B_PRED MBs do NOT touch the Y2 non-zero context (nzY16). The
	// decoder's parseResiduals only updates nzY16 for I16 MBs (inside
	// the `if d.usePredY16` branch in parseResiduals), and its skip
	// path only clears nzY16 when `d.usePredY16` is true. So nzY16
	// must persist across B_PRED MBs to stay in sync. A previous
	// unconditional reset here was the root cause of the quality
	// regression seen when arbitration (Method=3) mixed I16 and
	// B_PRED MBs within a frame.

	if skip {
		s.leftNZ = 0
		s.upNZ[mbx] = 0
		return
	}

	// Emit Y1SansY2 tokens for all 16 sub-blocks in raster order.
	lnzY := unpackNibble(s.leftNZ & 0x0f)
	unzY := unpackNibble(s.upNZ[mbx] & 0x0f)
	for sj := 0; sj < 4; sj++ {
		for si := 0; si < 4; si++ {
			ctx := int(lnzY[sj]) + int(unzY[si])
			nz := WriteCoefBlock(s.p1, &yQ[sj*4+si], PlaneY1SansY2, ctx,
				&DefaultTokenProb, false)
			lnzY[sj] = uint8(nz)
			unzY[si] = uint8(nz)
		}
	}
	newLeftY := packNibble(lnzY)
	newUpY := packNibble(unzY)
	s.leftNZ = (s.leftNZ & 0xf0) | newLeftY
	s.upNZ[mbx] = (s.upNZ[mbx] & 0xf0) | newUpY

	// UV token emission from the already-quantized blocks.
	s.emitMBChromaTokens(mbx, cbQ, crQ)
}

// buildI4Neighbors fills (tl, top, left) for the sub-block at position
// (si, sj) within the current MB, pulling from either the in-MB
// reconstruction buffer (for sub-blocks already processed in this MB) or
// from the MB-edge neighbor arrays (for pixels outside this MB).
func (s *encState) buildI4Neighbors(tl *byte, top *[8]byte, left *[4]byte,
	inMB *[16][16]byte, topRow *[20]byte, leftCol *[16]byte, corner byte,
	si, sj int) {
	x0 := si * 4
	y0 := sj * 4

	// Top-left corner.
	switch {
	case si == 0 && sj == 0:
		*tl = corner
	case sj == 0:
		*tl = topRow[x0-1]
	case si == 0:
		*tl = leftCol[y0-1]
	default:
		*tl = inMB[y0-1][x0-1]
	}

	// Direct top (4 pixels above).
	if sj == 0 {
		for i := 0; i < 4; i++ {
			top[i] = topRow[x0+i]
		}
	} else {
		for i := 0; i < 4; i++ {
			top[i] = inMB[y0-1][x0+i]
		}
	}

	// Top-right overhang (4 pixels to the top-right). For non-right-col
	// sub-blocks in row 0, these are the next 4 top-row pixels; for
	// row 0 right-column (si=3), they're the MB's overhang (topRow[16..19]).
	// For sj>=1 non-right-col, they're the bottom row of the sub-block
	// at (si+1, sj-1). For sj>=1 right-col, they're replicated from the
	// MB-level overhang (same as sj=0 si=3) — matches decoder's prepareYBR
	// duplication of ybr[0][24..27] to ybr[4/8/12][24..27].
	if si == 3 {
		for i := 0; i < 4; i++ {
			top[4+i] = topRow[16+i]
		}
	} else if sj == 0 {
		for i := 0; i < 4; i++ {
			top[4+i] = topRow[x0+4+i]
		}
	} else {
		// si < 3, sj >= 1: top-right is the bottom row of (si+1, sj-1),
		// which has already been reconstructed.
		for i := 0; i < 4; i++ {
			top[4+i] = inMB[y0-1][x0+4+i]
		}
	}

	// Left column.
	if si == 0 {
		for j := 0; j < 4; j++ {
			left[j] = leftCol[y0+j]
		}
	} else {
		for j := 0; j < 4; j++ {
			left[j] = inMB[y0+j][x0-1]
		}
	}
}

// quantizeMBChroma handles chroma prediction, residual, transform,
// quantization, and reconstruction for one MB — but does NOT emit
// tokens. Returns the quantized Cb and Cr blocks so the caller can
// decide whether to emit tokens (skip=0) or suppress them (skip=1).
func (s *encState) quantizeMBChroma(mbx, mby int, uvMode int) (*[4][16]int16, *[4][16]int16) {
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

	cbQ := new([4][16]int16)
	crQ := new([4][16]int16)
	var cbDQ, crDQ [4][16]int16
	for sby := 0; sby < 2; sby++ {
		for sbx := 0; sbx < 2; sbx++ {
			subIdx := sby*2 + sbx
			var cbRes, crRes, cbCoef, crCoef [16]int16
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					px := mbx*8 + sbx*4 + i
					py := mby*8 + sby*4 + j
					cbRes[j*4+i] = int16(s.frame.Cb[py*s.frame.UVStride+px]) -
						int16(cbPred[(sby*4+j)*8+sbx*4+i])
					crRes[j*4+i] = int16(s.frame.Cr[py*s.frame.UVStride+px]) -
						int16(crPred[(sby*4+j)*8+sbx*4+i])
				}
			}
			FDCT4x4(cbRes[:], cbCoef[:])
			FDCT4x4(crRes[:], crCoef[:])
			QuantizeBlockSplit(cbCoef[:], cbQ[subIdx][:], cbDQ[subIdx][:],
				s.quant.UV[0], s.quant.UV[1], 0, int32(s.quant.UV[1])/4)
			QuantizeBlockSplit(crCoef[:], crQ[subIdx][:], crDQ[subIdx][:],
				s.quant.UV[0], s.quant.UV[1], 0, int32(s.quant.UV[1])/4)
		}
	}

	// Reconstruct chroma immediately (doesn't depend on token emission).
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
					cbV := int32(cbPred[(sby*4+j)*8+sbx*4+i]) + int32(cbResRec[j*4+i])
					crV := int32(crPred[(sby*4+j)*8+sbx*4+i]) + int32(crResRec[j*4+i])
					s.reconCb[py*s.frame.UVStride+px] = byte(clampInt32(cbV, 0, 255))
					s.reconCr[py*s.frame.UVStride+px] = byte(clampInt32(crV, 0, 255))
				}
			}
		}
	}
	return cbQ, crQ
}

// emitMBChromaTokens writes chroma coefficient tokens to partition 1 and
// updates the chroma non-zero mask portion of s.leftNZ and s.upNZ[mbx].
// Paired with quantizeMBChroma when the caller decides skip=0.
func (s *encState) emitMBChromaTokens(mbx int, cbQ, crQ *[4][16]int16) {
	lnzUV := unpackNibble(s.leftNZ >> 4)
	unzUV := unpackNibble(s.upNZ[mbx] >> 4)
	for sby := 0; sby < 2; sby++ {
		nzLeft := lnzUV[sby]
		for sbx := 0; sbx < 2; sbx++ {
			ctx := int(nzLeft + unzUV[sbx])
			nz := WriteCoefBlock(s.p1, &cbQ[sby*2+sbx], PlaneUV, ctx,
				&DefaultTokenProb, false)
			nzLeft = uint8(nz)
			unzUV[sbx] = uint8(nz)
		}
		lnzUV[sby] = nzLeft
	}
	for sby := 0; sby < 2; sby++ {
		nzLeft := lnzUV[sby+2]
		for sbx := 0; sbx < 2; sbx++ {
			ctx := int(nzLeft + unzUV[sbx+2])
			nz := WriteCoefBlock(s.p1, &crQ[sby*2+sbx], PlaneUV, ctx,
				&DefaultTokenProb, false)
			nzLeft = uint8(nz)
			unzUV[sbx+2] = uint8(nz)
		}
		lnzUV[sby+2] = nzLeft
	}
	s.leftNZ = (s.leftNZ & 0x0f) | (packNibble(lnzUV) << 4)
	s.upNZ[mbx] = (s.upNZ[mbx] & 0x0f) | (packNibble(unzUV) << 4)
}

// bestI16 is pickYMode that also returns the winning SSE, used by
// per-MB arbitration (Method >= 3).
func (s *encState) bestI16(mbx, mby int) (int, int64) {
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
	for _, m := range []int{ModeDC, ModeVE, ModeHE, ModeTM} {
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
	return best, bestSSE
}

// estimateBPredSSE approximates the total SSE of the best per-sub-block
// I4 modes without doing full sub-block reconstruction. Within-MB
// neighbors come from SOURCE pixels instead of reconstructed ones —
// this overestimates B_PRED's quality slightly (real reconstruction has
// quant error that propagates), so the returned SSE is a lower bound.
// That's the right direction for a conservative arbitration that
// prefers I16 unless B_PRED is clearly better.
func (s *encState) estimateBPredSSE(mbx, mby int) int64 {
	// Use source pixels as neighbors approximation.
	var total int64
	for sby := 0; sby < 4; sby++ {
		for sbx := 0; sbx < 4; sbx++ {
			// Read this sub-block's source and its (source-based) neighbors.
			var src [16]byte
			for j := 0; j < 4; j++ {
				for i := 0; i < 4; i++ {
					src[j*4+i] = s.frame.Y[(mby*16+sby*4+j)*s.frame.YStride+mbx*16+sbx*4+i]
				}
			}
			tl, top, left, hasTop, hasLeft := s.i4NeighborsFromSource(mbx, mby, sbx, sby)

			bestSSE := int64(-1)
			for m := 0; m < NumPredModes; m++ {
				if !hasTop && (m == ModeI4VE || m == ModeI4RD || m == ModeI4VR ||
					m == ModeI4LD || m == ModeI4VL) {
					continue
				}
				if !hasLeft && (m == ModeI4HE || m == ModeI4RD || m == ModeI4VR ||
					m == ModeI4HD || m == ModeI4HU) {
					continue
				}
				if m == ModeI4TM && (!hasTop || !hasLeft) {
					continue
				}
				var pred [16]byte
				PredictI4(&pred, m, tl, &top, &left)
				sse := SumSquaredError(src[:], pred[:])
				if bestSSE < 0 || sse < bestSSE {
					bestSSE = sse
				}
			}
			if bestSSE > 0 {
				total += bestSSE
			}
		}
	}
	return total
}

// i4NeighborsFromSource builds an I4 neighbor set from source pixels
// (not reconstructed) — a shortcut used only for estimateBPredSSE.
// For within-MB neighbors it reads frame.Y; for outside-MB neighbors
// it uses reconY (which is the same as what a real encode would see).
func (s *encState) i4NeighborsFromSource(mbx, mby, sbx, sby int) (tl byte, top [8]byte, left [4]byte, hasTop, hasLeft bool) {
	x0 := mbx*16 + sbx*4
	y0 := mby*16 + sby*4
	hasTop = y0 > 0
	hasLeft = x0 > 0

	// top-left
	if !hasTop && !hasLeft {
		tl = 0x7f
	} else if !hasTop {
		tl = 0x7f
	} else if !hasLeft {
		tl = 0x81
	} else {
		if sby > 0 || sbx > 0 {
			tl = s.frame.Y[(y0-1)*s.frame.YStride+x0-1]
		} else {
			tl = s.reconY[(y0-1)*s.frame.YStride+x0-1]
		}
	}

	// top row (4 direct + 4 right overhang)
	if !hasTop {
		for i := 0; i < 8; i++ {
			top[i] = 0x7f
		}
	} else {
		for i := 0; i < 4; i++ {
			if sby > 0 {
				top[i] = s.frame.Y[(y0-1)*s.frame.YStride+x0+i]
			} else {
				top[i] = s.reconY[(y0-1)*s.frame.YStride+x0+i]
			}
		}
		// Top-right 4 pixels. For right-edge MBs and right-column
		// sub-blocks, replicate.
		for i := 0; i < 4; i++ {
			xi := x0 + 4 + i
			if xi >= s.frame.YStride || xi >= mbx*16+16 {
				// overhang: use the MB-level top-right area
				if mbx*16+16+i < (mbx+1)*16+16 && y0 > 0 && mbx < s.frame.MBWidth-1 {
					top[4+i] = s.reconY[(y0-1)*s.frame.YStride+mbx*16+16+i]
				} else {
					// replicate last top pixel
					top[4+i] = top[3]
				}
			} else {
				if sby > 0 {
					top[4+i] = s.frame.Y[(y0-1)*s.frame.YStride+xi]
				} else {
					top[4+i] = s.reconY[(y0-1)*s.frame.YStride+xi]
				}
			}
		}
	}

	// left column
	if !hasLeft {
		for j := 0; j < 4; j++ {
			left[j] = 0x81
		}
	} else {
		for j := 0; j < 4; j++ {
			if sbx > 0 {
				left[j] = s.frame.Y[(y0+j)*s.frame.YStride+x0-1]
			} else {
				left[j] = s.reconY[(y0+j)*s.frame.YStride+x0-1]
			}
		}
	}
	return
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

// allZero16 returns true if every element of a 16-entry int16 array is zero.
func allZero16(a *[16]int16) bool {
	for _, v := range a {
		if v != 0 {
			return false
		}
	}
	return true
}

// blocksAllZero16 checks 16 sub-blocks × 16 coefficients.
func blocksAllZero16(a *[16][16]int16) bool {
	for i := 0; i < 16; i++ {
		for _, v := range a[i] {
			if v != 0 {
				return false
			}
		}
	}
	return true
}

// blocksAllZero4 checks 4 sub-blocks × 16 coefficients (used for Cb/Cr).
func blocksAllZero4(a *[4][16]int16) bool {
	for i := 0; i < 4; i++ {
		for _, v := range a[i] {
			if v != 0 {
				return false
			}
		}
	}
	return true
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

// i16ToI4Context maps an I16 mode to the equivalent I4 mode used for
// propagating prediction context to neighbor MBs. DC↔DC, V↔V, H↔H, TM↔TM.
func i16ToI4Context(i16 int) int {
	switch i16 {
	case ModeDC:
		return ModeI4DC
	case ModeVE:
		return ModeI4VE
	case ModeHE:
		return ModeI4HE
	case ModeTM:
		return ModeI4TM
	}
	return ModeI4DC
}

// qualityToQI maps a 0..100 quality setting to a 0..127 base quantizer
// index using a monotonically decreasing curve: Q=100 → QI=0 (finest),
// Q=0 → QI=127 (coarsest).
//
// Uses a non-linear curve QI = 127 * (1 - Q/100)^1.2 to bias QI toward
// finer quantization at high Q (matching user expectations from cwebp
// where Q=75 is "good default" with ample headroom below). Compared to
// a linear mapping:
//
//	Q=95 → QI=6   (linear would give 6 also)
//	Q=90 → QI=10  (linear: 12)
//	Q=75 → QI=22  (linear: 31)
//	Q=50 → QI=55  (linear: 63)
//	Q=25 → QI=93  (linear: 95)
//
// Net effect: visibly finer quality at Q=75-95 for a modest size cost.
func qualityToQI(quality float32) int {
	if quality <= 0 {
		return 127
	}
	if quality >= 100 {
		return 0
	}
	t := float64(100-quality) / 100
	qi := int(127 * math.Pow(t, 1.2))
	if qi < 0 {
		qi = 0
	}
	if qi > 127 {
		qi = 127
	}
	return qi
}

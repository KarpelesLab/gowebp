package vp8enc

// Partition 0 header emission (RFC 6386 sections 9.2–9.7, 13.4–13.5).
//
// All bits here are encoded via the boolean arithmetic coder against
// UniformProb unless otherwise noted. Ordering is strict — the decoder at
// golang.org/x/image/vp8 parses in this exact sequence, and any drift
// makes every subsequent field misalign.

// WriteHeaderInit writes the keyframe-only fields at the top of partition 0:
//   - color_space (1 bit, must be 0)
//   - clamp_type (1 bit, typically 0 meaning "bicubic clamping")
func WriteHeaderInit(e *BoolEncoder) {
	e.WriteBit(0, UniformProb) // color_space
	e.WriteBit(0, UniformProb) // clamp_type
}

// WriteSegmentHeaderOff emits a "no segmentation" segment header: a single
// zero bit indicating use_segment is false.
func WriteSegmentHeaderOff(e *BoolEncoder) {
	e.WriteBit(0, UniformProb)
}

// WriteFilterHeaderOff emits a filter header with level=0 (disables the
// loop filter entirely per RFC 6386 section 15):
//
//	simple=0, level=0, sharpness=0, useLFDelta=0
func WriteFilterHeaderOff(e *BoolEncoder) {
	e.WriteBit(0, UniformProb)     // simple
	e.WriteUint(0, 6, UniformProb) // level
	e.WriteUint(0, 3, UniformProb) // sharpness
	e.WriteBit(0, UniformProb)     // useLFDelta
}

// WriteLog2NumParts writes the log2 of the number of coefficient partitions
// (0..3 → 1, 2, 4, 8 partitions).
func WriteLog2NumParts(e *BoolEncoder, log2 int) {
	e.WriteUint(uint32(log2), 2, UniformProb)
}

// WriteQuantHeader emits the quantizer indexer: base Y AC index in 7 bits
// followed by five optional deltas (Y1 DC, Y2 DC, Y2 AC, UV DC, UV AC).
// Set all deltas to 0 for simple mode.
func WriteQuantHeader(e *BoolEncoder, baseQ int) {
	e.WriteUint(uint32(baseQ&0x7f), 7, UniformProb)
	e.WriteOptionalInt(0, 4, UniformProb) // y1dc_delta_q
	e.WriteOptionalInt(0, 4, UniformProb) // y2dc_delta_q
	e.WriteOptionalInt(0, 4, UniformProb) // y2ac_delta_q
	e.WriteOptionalInt(0, 4, UniformProb) // uvdc_delta_q
	e.WriteOptionalInt(0, 4, UniformProb) // uvac_delta_q
}

// WriteRefreshEntropyProbs emits the single "refresh entropy probs" flag.
// Keyframe-only. 0 means the next frame does not inherit updated probs
// (we're still a static image encoder so this is always 0).
func WriteRefreshEntropyProbs(e *BoolEncoder) {
	e.WriteBit(0, UniformProb)
}

// WriteTokenProbUpdates emits the 4×8×3×11 = 1056 "should we update this
// prob?" bits, each coded against TokenProbUpdateProb. We never update, so
// every emitted bit is 0. The decoder MUST consume exactly this many bits
// in this exact order.
func WriteTokenProbUpdates(e *BoolEncoder) {
	for i := 0; i < NumPlanes; i++ {
		for j := 0; j < NumBands; j++ {
			for k := 0; k < NumContexts; k++ {
				for l := 0; l < NumProbs; l++ {
					e.WriteBit(0, int(TokenProbUpdateProb[i][j][k][l]))
				}
			}
		}
	}
}

// WriteSkipProb emits mb_no_skip_coeff plus, if true, an 8-bit skip
// probability. If skipProbOn is true, per-MB skip bits follow; if false,
// every MB carries full residuals.
func WriteSkipProb(e *BoolEncoder, skipProbOn bool, prob uint8) {
	if !skipProbOn {
		e.WriteBit(0, UniformProb)
		return
	}
	e.WriteBit(1, UniformProb)
	e.WriteUint(uint32(prob), 8, UniformProb)
}

// Y16 mode enum used by WriteMBModes. The decoder tree is:
//
//	readBit(156) -> if 0: readBit(163) -> 0:DC, 1:VE
//	              -> if 1: readBit(128) -> 0:HE, 1:TM
const (
	ModeDC = 0
	ModeVE = 1
	ModeHE = 2
	ModeTM = 3
)

// WriteMBModes writes the per-macroblock mode fields for an I16 MB:
//
//	usePredY16 (1 bit at prob 145), predY16, predC8.
// Must be called once per MB, after any skip bit, in MB raster order.
func WriteMBModes(e *BoolEncoder, predY16, predC8 int) {
	e.WriteBit(1, 145) // usePredY16 = true (I16 mode)

	// I16 Y mode tree.
	switch predY16 {
	case ModeDC:
		e.WriteBit(0, 156)
		e.WriteBit(0, 163)
	case ModeVE:
		e.WriteBit(0, 156)
		e.WriteBit(1, 163)
	case ModeHE:
		e.WriteBit(1, 156)
		e.WriteBit(0, 128)
	case ModeTM:
		e.WriteBit(1, 156)
		e.WriteBit(1, 128)
	}

	writeUVMode(e, predC8)
}

// WriteMBModesBPred writes the per-macroblock mode fields for a B_PRED
// MB: usePredY16=0, 16 I4 modes (tree-coded against PredProb[above][left]),
// then the UV mode.
//
// modes[j][i] is the I4 mode for sub-block at (i, j) in the MB.
// aboveModes[i] is the mode of the sub-block above sub-block column i
//   (bottom-row modes of the MB above, or ModeI4DC if on the frame edge).
// leftModes is updated in place: on entry, leftModes[j] is the mode of
//   the sub-block to the left of sub-block row j; on return, leftModes[j]
//   holds the mode of this MB's rightmost sub-block in row j (i.e. the
//   input context for the NEXT MB to the right).
// aboveModes is also updated in place to reflect the bottom-row modes
// of this MB.
func WriteMBModesBPred(e *BoolEncoder, modes *[4][4]int, aboveModes *[4]int, leftModes *[4]int, predC8 int) {
	e.WriteBit(0, 145) // usePredY16 = false (B_PRED)

	for j := 0; j < 4; j++ {
		l := leftModes[j]
		for i := 0; i < 4; i++ {
			mode := modes[j][i]
			writeI4Mode(e, mode, aboveModes[i], l)
			l = mode
			aboveModes[i] = mode
		}
		leftModes[j] = l
	}

	writeUVMode(e, predC8)
}

// writeI4Mode emits the tree-coded I4 mode given the modes of the
// sub-blocks above and to the left. Tree structure from RFC 6386 section
// 11.5, mirroring parsePredModeY4 in x/image/vp8/pred.go.
func writeI4Mode(e *BoolEncoder, mode, above, left int) {
	prob := &PredProb[above][left]
	switch mode {
	case ModeI4DC:
		e.WriteBit(0, int(prob[0]))
	case ModeI4TM:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(0, int(prob[1]))
	case ModeI4VE:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(0, int(prob[2]))
	case ModeI4HE:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(1, int(prob[2]))
		e.WriteBit(0, int(prob[3]))
		e.WriteBit(0, int(prob[4]))
	case ModeI4RD:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(1, int(prob[2]))
		e.WriteBit(0, int(prob[3]))
		e.WriteBit(1, int(prob[4]))
		e.WriteBit(0, int(prob[5]))
	case ModeI4VR:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(1, int(prob[2]))
		e.WriteBit(0, int(prob[3]))
		e.WriteBit(1, int(prob[4]))
		e.WriteBit(1, int(prob[5]))
	case ModeI4LD:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(1, int(prob[2]))
		e.WriteBit(1, int(prob[3]))
		e.WriteBit(0, int(prob[6]))
	case ModeI4VL:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(1, int(prob[2]))
		e.WriteBit(1, int(prob[3]))
		e.WriteBit(1, int(prob[6]))
		e.WriteBit(0, int(prob[7]))
	case ModeI4HD:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(1, int(prob[2]))
		e.WriteBit(1, int(prob[3]))
		e.WriteBit(1, int(prob[6]))
		e.WriteBit(1, int(prob[7]))
		e.WriteBit(0, int(prob[8]))
	case ModeI4HU:
		e.WriteBit(1, int(prob[0]))
		e.WriteBit(1, int(prob[1]))
		e.WriteBit(1, int(prob[2]))
		e.WriteBit(1, int(prob[3]))
		e.WriteBit(1, int(prob[6]))
		e.WriteBit(1, int(prob[7]))
		e.WriteBit(1, int(prob[8]))
	}
}

func writeUVMode(e *BoolEncoder, predC8 int) {
	switch predC8 {
	case ModeDC:
		e.WriteBit(0, 142)
	case ModeVE:
		e.WriteBit(1, 142)
		e.WriteBit(0, 114)
	case ModeHE:
		e.WriteBit(1, 142)
		e.WriteBit(1, 114)
		e.WriteBit(0, 183)
	case ModeTM:
		e.WriteBit(1, 142)
		e.WriteBit(1, 114)
		e.WriteBit(1, 183)
	}
}

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
	// Currently ignored (stub for Phase C tuning hooks).
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
// At the current stage this is a minimum-viable keyframe: every macroblock
// is encoded as I16 DC_PRED with all coefficients skipped. The resulting
// image decodes through golang.org/x/image/vp8 as a solid mid-gray frame
// (for a solid-color input) and exercises the full partition-0 header
// including the mandatory 1056 token-prob-update bits, the skip machinery,
// and the mode bits.
//
// Full intra-prediction and residual coding arrive in Phase B proper.
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

	// Partition 0: frame-level header + per-MB modes.
	p0 := NewBoolEncoder()
	WriteHeaderInit(p0)
	WriteSegmentHeaderOff(p0)
	WriteFilterHeaderOff(p0)
	WriteLog2NumParts(p0, 0) // single residual partition
	WriteQuantHeader(p0, baseQ)
	WriteRefreshEntropyProbs(p0)
	WriteTokenProbUpdates(p0)
	WriteSkipProb(p0, true, 255) // enable skip, always-skip probability 255

	for i := 0; i < frame.MBWidth*frame.MBHeight; i++ {
		p0.WriteBit(1, 255) // skip = true (always, per skipProb=255)
		WriteMBModes(p0, ModeDC, ModeDC)
	}

	// Partition 1: residual coefficient data. Because every MB has
	// skip=1, no coefficients are emitted; the partition exists but is
	// empty (modulo the boolean-coder flush).
	p1 := NewBoolEncoder()

	p0Bytes := p0.Finish()
	p1Bytes := p1.Finish()

	// Uncompressed 10-byte keyframe header.
	//
	//   bits 0    : key_frame (0 = keyframe)
	//   bits 1-3  : version
	//   bit  4    : show_frame (1)
	//   bits 5-23 : first_partition_size (19 bits)
	//   bytes 3-5 : start code 0x9d 0x01 0x2a
	//   bytes 6-7 : scale(2) | width(14), little-endian
	//   bytes 8-9 : scale(2) | height(14), little-endian
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
	header[7] = byte(wd >> 8) // top 2 bits = horizontal scale, set to 0
	header[8] = byte(ht)
	header[9] = byte(ht >> 8) // top 2 bits = vertical scale, set to 0

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

// EncodeWebP wraps an EncodeFrame call in the RIFF/VP8 container format,
// producing a complete .webp file.
func EncodeWebP(w io.Writer, img image.Image, opts EncodeOptions) error {
	var vp8 bytes.Buffer
	if err := EncodeFrame(&vp8, img, opts); err != nil {
		return err
	}
	chunkLen := vp8.Len()
	// VP8 chunks must be padded to an even length.
	padded := chunkLen
	if padded&1 == 1 {
		padded++
	}

	total := 4 + 4 + 4 + padded // "WEBP" + "VP8 " + size(4) + data

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
	// Linear for now; Phase C will calibrate against cwebp.
	qi := int((100.0 - quality) * 127.0 / 100.0)
	if qi < 0 {
		qi = 0
	}
	if qi > 127 {
		qi = 127
	}
	return qi
}

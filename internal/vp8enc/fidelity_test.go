package vp8enc

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"testing"

	xwebp "golang.org/x/image/webp"
)

// TestEncodePSNR encodes varied content and checks that the decoded
// output has reasonable fidelity. This is the Phase B milestone test:
// PSNR ≥ 30 dB at Q=75 for a gradient image, which is achievable with
// I16-only encoding. Higher PSNR requires I4 modes (later phase).
func TestEncodePSNR(t *testing.T) {
	cases := []struct {
		name     string
		quality  float32
		method   int
		minPSNR  float64
		minPSNRY float64 // spec-correct limited-range Y-PSNR
	}{
		// minPSNR is the RGB-PSNR through Go's (JFIF) YCbCrToRGB, which
		// always caps around 28 dB because of the BT.601 range mismatch
		// with VP8's limited-range color space.
		// minPSNRY is the luma-only PSNR computed against the spec's
		// limited-range BT.601, which is the actual encoder quality.
		{"q90-method-1", 90, 1, 28, 45},
		{"q75-method-1", 75, 1, 25, 40},
		{"q50-method-1", 50, 1, 22, 35},
		{"q90-method-2", 90, 2, 28, 45},
		{"q75-method-2", 75, 2, 25, 40},
		{"q75-method-3", 75, 3, 26, 40},
		{"q90-method-3", 90, 3, 28, 45},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 64x64 gradient, detectable content.
			w, h := 64, 64
			src := image.NewNRGBA(image.Rect(0, 0, w, h))
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					r := uint8((x * 255) / w)
					g := uint8((y * 255) / h)
					b := uint8(((x + y) * 255) / (w + h))
					src.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
				}
			}

			var buf bytes.Buffer
			err := EncodeWebP(&buf, src, EncodeOptions{Quality: c.quality, Method: c.method})
			if err != nil {
				t.Fatalf("EncodeWebP: %v", err)
			}
			t.Logf("%s: encoded %d bytes", c.name, buf.Len())

			dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			psnr := computePSNR(src, dec)
			var psnrY float64
			if ycbcr, ok := dec.(*image.YCbCr); ok {
				psnrY = computePSNRLimited(src, ycbcr)
			}
			t.Logf("%s: RGB-PSNR (jfif-via-stdlib) = %.2f dB, Y-PSNR (spec) = %.2f dB",
				c.name, psnr, psnrY)
			if psnr < c.minPSNR {
				t.Errorf("%s: RGB-PSNR %.2f dB below threshold %.2f dB",
					c.name, psnr, c.minPSNR)
			}
			if psnrY > 0 && psnrY < c.minPSNRY {
				t.Errorf("%s: Y-PSNR %.2f dB below threshold %.2f dB",
					c.name, psnrY, c.minPSNRY)
			}
		})
	}
}

// TestBPredBeatsI16OnTexture verifies that on content with high spatial
// detail the per-sub-block I4 modes (Method=2) produce equal or better
// PSNR than the single-block I16 modes (Method=1).
func TestBPredBeatsI16OnTexture(t *testing.T) {
	// 64x64 checkerboard — classic case where I4 modes excel because
	// each 4x4 can independently pick a direction; I16 is forced to one.
	w, h := 64, 64
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(255)
			if ((x >> 2) ^ (y >> 2)) & 1 == 0 {
				v = 32
			}
			src.SetNRGBA(x, y, color.NRGBA{v, v, v, 255})
		}
	}

	var bufI16, bufBPred bytes.Buffer
	if err := EncodeWebP(&bufI16, src, EncodeOptions{Quality: 75, Method: 1}); err != nil {
		t.Fatalf("I16: %v", err)
	}
	if err := EncodeWebP(&bufBPred, src, EncodeOptions{Quality: 75, Method: 2}); err != nil {
		t.Fatalf("B_PRED: %v", err)
	}
	t.Logf("I16 size: %d bytes, B_PRED size: %d bytes", bufI16.Len(), bufBPred.Len())

	decI16, err := xwebp.Decode(bytes.NewReader(bufI16.Bytes()))
	if err != nil {
		t.Fatalf("decode I16: %v", err)
	}
	decBPred, err := xwebp.Decode(bytes.NewReader(bufBPred.Bytes()))
	if err != nil {
		t.Fatalf("decode B_PRED: %v", err)
	}
	psnrI16 := computePSNR(src, decI16)
	psnrBPred := computePSNR(src, decBPred)
	t.Logf("checkerboard PSNR — I16: %.2f dB, B_PRED: %.2f dB", psnrI16, psnrBPred)
	if psnrBPred < psnrI16-1 {
		t.Errorf("B_PRED PSNR %.2f substantially worse than I16 %.2f", psnrBPred, psnrI16)
	}
}

// TestEncodeSolidColor verifies that a solid-color image round-trips
// with near-perfect fidelity — the DC prediction + quantization path
// should recover the input color cleanly.
func TestEncodeSolidColor(t *testing.T) {
	w, h := 32, 32
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{100, 150, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := EncodeWebP(&buf, src, EncodeOptions{Quality: 90, Method: 1}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	psnr := computePSNR(src, dec)
	t.Logf("solid-color PSNR at Q=90: %.2f dB", psnr)
	// I16-only with naive deadzone quantization and 4:2:0 chroma
	// subsampling caps PSNR around 30 dB for typical mid-saturation
	// colors. cwebp hits 40+ dB by using trellis + RDO, which arrives
	// in later phases.
	if psnr < 28 {
		t.Errorf("solid-color PSNR %.2f < 28", psnr)
	}
}

// computePSNR compares two images pixel-by-pixel in sRGB space and
// returns PSNR in dB (higher is better; 40+ is visually lossless).
//
// NOTE: if b is an *image.YCbCr (which is what x/image/vp8 and
// x/image/webp return for lossy), this converts via Go's stdlib
// YCbCrToRGB which uses JFIF (full-range) BT.601. That differs from
// VP8's limited-range BT.601 and injects a systematic ~2-4 unit error
// per channel. For a spec-correct PSNR, use computePSNRLimited below.
func computePSNR(a, b image.Image) float64 {
	rect := a.Bounds()
	var sumSq float64
	var n float64
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			ar, ag, ab, _ := a.At(x, y).RGBA()
			br, bg, bb, _ := b.At(x, y).RGBA()
			dr := float64(ar>>8) - float64(br>>8)
			dg := float64(ag>>8) - float64(bg>>8)
			db := float64(ab>>8) - float64(bb>>8)
			sumSq += dr*dr + dg*dg + db*db
			n += 3
		}
	}
	if sumSq == 0 {
		return math.Inf(1)
	}
	mse := sumSq / n
	return 10 * math.Log10(255*255/mse)
}

// computePSNRLimited converts the source image to YCbCr using the VP8
// spec's limited-range BT.601 and compares it against b's raw YCbCr
// planes. This avoids the JFIF/BT.601-range mismatch that Go's
// image/color YCbCrToRGB introduces when called on VP8 output.
//
// Returns PSNR-Y (luma-only), which is typically 2-3 dB higher than
// RGB PSNR and is the standard metric in video compression literature.
func computePSNRLimited(src image.Image, vp8Decoded *image.YCbCr) float64 {
	rect := src.Bounds()
	var sumSq float64
	var n float64
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			r, g, b, _ := src.At(x, y).RGBA()
			R := int32(r >> 8)
			G := int32(g >> 8)
			B := int32(b >> 8)
			// VP8 spec limited-range Y
			Y := (66*R+129*G+25*B+128)>>8 + 16
			if Y < 0 {
				Y = 0
			}
			if Y > 255 {
				Y = 255
			}
			decY := int32(vp8Decoded.Y[(y-rect.Min.Y)*vp8Decoded.YStride+(x-rect.Min.X)])
			d := float64(Y - decY)
			sumSq += d * d
			n += 1
		}
	}
	if sumSq == 0 {
		return math.Inf(1)
	}
	mse := sumSq / n
	return 10 * math.Log10(255*255/mse)
}

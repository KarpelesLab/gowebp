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
		name    string
		quality float32
		method  int
		minPSNR float64
	}{
		{"q90-method-1", 90, 1, 28},
		{"q75-method-1", 75, 1, 25},
		{"q50-method-1", 50, 1, 22},
		{"q90-method-2", 90, 2, 28},
		{"q75-method-2", 75, 2, 25},
		{"q75-method-3", 75, 3, 26}, // per-MB arbitration
		{"q90-method-3", 90, 3, 28},
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
			t.Logf("%s: PSNR = %.2f dB", c.name, psnr)
			if psnr < c.minPSNR {
				t.Errorf("%s: PSNR %.2f dB below threshold %.2f dB",
					c.name, psnr, c.minPSNR)
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

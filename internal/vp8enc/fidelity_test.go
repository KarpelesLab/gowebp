package vp8enc

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"sync"
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
		{"q75-method-1", 75, 1, 25, 39},
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
			var psnrY, psnrRGBSpec float64
			if ycbcr, ok := dec.(*image.YCbCr); ok {
				psnrY = computePSNRLimited(src, ycbcr)
				psnrRGBSpec = computePSNRSpec(src, ycbcr)
			}
			t.Logf("%s: RGB(spec)=%.2f dB, Y=%.2f dB, RGB(jfif)=%.2f dB",
				c.name, psnrRGBSpec, psnrY, psnr)
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

// TestEncodeNaturalContent exercises the encoder on synthetic but
// natural-ish content (spatial frequency + smooth gradients + color
// variation) to validate it doesn't fall apart on realistic input.
// Uses spec-correct Y-PSNR.
func TestEncodeNaturalContent(t *testing.T) {
	w, h := 128, 128
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Ramp + low-frequency wave + mild high-freq texture.
			r := uint8((x*200/w + ((x^y)&7)*4) & 0xff)
			g := uint8((y*180/h + ((x>>3)^(y>>3))&0x1f) & 0xff)
			b := uint8(((x+y)*220/(w+h) + ((x*y)>>6)&0xf) & 0xff)
			src.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
		}
	}
	cases := []struct {
		q        float32
		m        int
		minPSNRY float64
	}{
		{90, 2, 38},
		{75, 2, 33},
		{50, 2, 28},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := EncodeWebP(&buf, src, EncodeOptions{Quality: c.q, Method: c.m}); err != nil {
			t.Fatalf("Q=%.0f M=%d: %v", c.q, c.m, err)
		}
		dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("Q=%.0f: decode %v", c.q, err)
		}
		ycbcr, ok := dec.(*image.YCbCr)
		if !ok {
			t.Fatalf("Q=%.0f: decoded is not YCbCr", c.q)
		}
		psnrY := computePSNRLimited(src, ycbcr)
		t.Logf("natural 128x128 Q=%.0f M=%d: %d bytes, Y-PSNR=%.2f dB",
			c.q, c.m, buf.Len(), psnrY)
		if psnrY < c.minPSNRY {
			t.Errorf("Q=%.0f: Y-PSNR %.2f dB < threshold %.2f dB",
				c.q, psnrY, c.minPSNRY)
		}
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

// TestQuality100NearLossless verifies that Q=100 (QI=0, finest
// quantization) preserves the source image at near-lossless quality.
// At QI=0 the dequant factors are their minimum (4 for both DC and AC),
// so quantization error should be bounded tightly.
func TestQuality100NearLossless(t *testing.T) {
	w, h := 64, 64
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	// Smooth gradient — avoids aliasing artifacts from 4:2:0 chroma
	// subsampling that would dominate at high Q.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				uint8(x * 255 / w),
				uint8(y * 255 / h),
				uint8((x + y) * 255 / (w + h)),
				255,
			})
		}
	}
	var buf bytes.Buffer
	if err := EncodeWebP(&buf, src, EncodeOptions{Quality: 100, Method: 3}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	ycbcr := dec.(*image.YCbCr)
	psnrY := computePSNRLimited(src, ycbcr)
	psnrRGB := computePSNRSpec(src, ycbcr)
	t.Logf("Q=100 near-lossless: %d bytes, Y-PSNR=%.2f dB, RGB-PSNR=%.2f dB",
		buf.Len(), psnrY, psnrRGB)
	if psnrY < 45 {
		t.Errorf("Q=100 Y-PSNR %.2f below expected 45+", psnrY)
	}
}

// TestEncodeConcurrent verifies the encoder has no shared mutable
// state: N goroutines encoding the same image concurrently must all
// produce byte-identical output.
func TestEncodeConcurrent(t *testing.T) {
	w, h := 64, 64
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				uint8(x * 255 / w),
				uint8(y * 255 / h),
				uint8((x + y) * 255 / (w + h)),
				255,
			})
		}
	}
	// Reference encode (single-threaded).
	var ref bytes.Buffer
	if err := EncodeWebP(&ref, src, EncodeOptions{Quality: 75, Method: 2}); err != nil {
		t.Fatalf("ref: %v", err)
	}
	refBytes := ref.Bytes()

	const N = 8
	results := make([][]byte, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			var buf bytes.Buffer
			errs[idx] = EncodeWebP(&buf, src, EncodeOptions{Quality: 75, Method: 2})
			results[idx] = buf.Bytes()
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
			continue
		}
		if !bytes.Equal(results[i], refBytes) {
			t.Errorf("goroutine %d output differs from reference", i)
		}
	}
}

// TestEncodeTinyImages verifies the encoder handles the smallest
// possible inputs correctly: 1x1 up to the first full-MB size (16x16)
// and sizes that aren't MB-aligned.
func TestEncodeTinyImages(t *testing.T) {
	sizes := []struct{ w, h int }{
		{1, 1}, {2, 2}, {3, 5}, {7, 7}, {8, 16},
		{15, 15}, {16, 16}, {17, 17}, {31, 9}, {33, 33},
	}
	for _, sz := range sizes {
		src := image.NewNRGBA(image.Rect(0, 0, sz.w, sz.h))
		for y := 0; y < sz.h; y++ {
			for x := 0; x < sz.w; x++ {
				src.SetNRGBA(x, y, color.NRGBA{
					uint8((x * 255) / (sz.w + 1)),
					uint8((y * 255) / (sz.h + 1)),
					uint8(((x + y) * 255) / (sz.w + sz.h + 1)),
					255,
				})
			}
		}
		var buf bytes.Buffer
		for _, m := range []int{0, 1, 2, 3} {
			buf.Reset()
			if err := EncodeWebP(&buf, src, EncodeOptions{Quality: 75, Method: m}); err != nil {
				t.Errorf("%dx%d M=%d: encode error %v", sz.w, sz.h, m, err)
				continue
			}
			dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Errorf("%dx%d M=%d: decode error %v", sz.w, sz.h, m, err)
				continue
			}
			b := dec.Bounds()
			if b.Dx() != sz.w || b.Dy() != sz.h {
				t.Errorf("%dx%d M=%d: decoded %v", sz.w, sz.h, m, b)
			}
		}
	}
}

// TestSkipSavesBytesOnFlat checks that MBs with all-zero quantized
// coefficients trigger the skip bit and omit token emission. Flat
// content after prediction produces near-zero residuals that quantize
// to zero; file size should drop noticeably.
func TestSkipSavesBytesOnFlat(t *testing.T) {
	w, h := 128, 128
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{128, 128, 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := EncodeWebP(&buf, src, EncodeOptions{Quality: 75, Method: 1}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	ycbcr := dec.(*image.YCbCr)
	psnr := computePSNRSpec(src, ycbcr)
	t.Logf("flat gray 128x128 Q=75 M=1: %d bytes, PSNR=%.2f dB", buf.Len(), psnr)
	// A 128x128 flat image should compress to very few bytes with
	// skip enabled (most MBs skip after prediction).
	if buf.Len() > 300 {
		t.Errorf("flat-content file size %d bytes — skip bit not effective", buf.Len())
	}
	if psnr < 35 {
		t.Errorf("flat-content PSNR %.2f below expected 35+", psnr)
	}
}

// TestArbitrationBugDiagnostic narrows down the Method=3 quality
// regression that shows up on larger images at high Q.
func TestArbitrationBugDiagnostic(t *testing.T) {
	for _, sz := range []int{32, 48, 64, 96, 128} {
		w, h := sz, sz
		src := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				src.SetNRGBA(x, y, color.NRGBA{
					uint8(x * 255 / w),
					uint8(y * 255 / h),
					uint8((x + y) * 255 / (w + h)),
					255,
				})
			}
		}
		for _, m := range []int{1, 2, 3} {
			var buf bytes.Buffer
			if err := EncodeWebP(&buf, src, EncodeOptions{Quality: 90, Method: m}); err != nil {
				t.Fatalf("%dx%d M=%d: %v", sz, sz, m, err)
			}
			dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Logf("%dx%d M=%d: decode err %v", sz, sz, m, err)
				continue
			}
			ycbcr := dec.(*image.YCbCr)
			psnrY := computePSNRLimited(src, ycbcr)
			t.Logf("%dx%d M=%d Q=90: %d bytes, Y-PSNR=%.2f dB",
				sz, sz, m, buf.Len(), psnrY)
		}
	}
}

// TestQualityCurve encodes the same image at a range of quality
// settings and reports the size/quality curve. Intended as a human-
// readable diagnostic, not a hard assertion (no pass/fail beyond
// encoding succeeding at each Q).
func TestQualityCurve(t *testing.T) {
	if testing.Short() {
		t.Skip("quality curve diagnostic")
	}
	w, h := 128, 128
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				uint8(x * 255 / w),
				uint8(y * 255 / h),
				uint8((x + y) * 255 / (w + h)),
				255,
			})
		}
	}
	t.Logf("128x128 gradient")
	t.Logf("%5s %6s %8s %10s %10s", "Q", "Method", "bytes", "Y-PSNR", "RGB-PSNR")
	t.Logf("%5s %6s %8s %10s %10s", "-----", "------", "--------", "----------", "----------")
	for _, m := range []int{1, 2, 3} {
		for _, q := range []float32{25, 50, 75, 90, 100} {
			var buf bytes.Buffer
			if err := EncodeWebP(&buf, src, EncodeOptions{Quality: q, Method: m}); err != nil {
				t.Fatalf("Q=%.0f M=%d: %v", q, m, err)
			}
			dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("Q=%.0f M=%d decode: %v", q, m, err)
			}
			ycbcr := dec.(*image.YCbCr)
			psnrY := computePSNRLimited(src, ycbcr)
			psnrRGB := computePSNRSpec(src, ycbcr)
			t.Logf("%5.0f %6d %8d %9.2f dB %9.2f dB", q, m, buf.Len(), psnrY, psnrRGB)
		}
	}
}

// BenchmarkEncode256x256 measures encode throughput on a 256x256
// natural-ish image across the three main method levels.
func BenchmarkEncode256x256(b *testing.B) {
	w, h := 256, 256
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := uint8((x*200/w + ((x^y)&7)*4) & 0xff)
			g := uint8((y*180/h + ((x>>3)^(y>>3))&0x1f) & 0xff)
			bl := uint8(((x+y)*220/(w+h) + ((x*y)>>6)&0xf) & 0xff)
			src.SetNRGBA(x, y, color.NRGBA{r, g, bl, 255})
		}
	}
	cases := []struct {
		name   string
		method int
	}{
		{"method-0-I16-DC-only", 0},
		{"method-1-I16-4modes", 1},
		{"method-2-BPRED", 2},
		{"method-3-arbitration", 3},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var buf bytes.Buffer
			for i := 0; i < b.N; i++ {
				buf.Reset()
				if err := EncodeWebP(&buf, src, EncodeOptions{
					Quality: 75,
					Method:  c.method,
				}); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(buf.Len()), "bytes/op")
		})
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

// computePSNRSpec converts the decoded YCbCr back to RGB using the
// VP8 spec's limited-range BT.601 inverse (matching what a
// spec-compliant decoder like libwebp / browsers produce) and
// compares against source RGB. This is the fair "what will users
// actually see when viewing the encoded file" quality metric.
func computePSNRSpec(src image.Image, dec *image.YCbCr) float64 {
	rect := src.Bounds()
	var sumSq float64
	var n float64

	// Limited-range BT.601 inverse (VP8 spec / libwebp). Integer form:
	//   R = clip((298*(Y-16)             + 409*(Cr-128) + 128) >> 8)
	//   G = clip((298*(Y-16) - 100*(Cb-128) - 208*(Cr-128) + 128) >> 8)
	//   B = clip((298*(Y-16) + 516*(Cb-128)               + 128) >> 8)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			sr, sg, sb, _ := src.At(x, y).RGBA()
			Rs := int32(sr >> 8)
			Gs := int32(sg >> 8)
			Bs := int32(sb >> 8)

			lx := x - rect.Min.X
			ly := y - rect.Min.Y
			Y := int32(dec.Y[ly*dec.YStride+lx]) - 16
			Cb := int32(dec.Cb[(ly/2)*dec.CStride+(lx/2)]) - 128
			Cr := int32(dec.Cr[(ly/2)*dec.CStride+(lx/2)]) - 128
			R := clampInt32((298*Y+409*Cr+128)>>8, 0, 255)
			G := clampInt32((298*Y-100*Cb-208*Cr+128)>>8, 0, 255)
			B := clampInt32((298*Y+516*Cb+128)>>8, 0, 255)

			dr := float64(Rs - R)
			dg := float64(Gs - G)
			db := float64(Bs - B)
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

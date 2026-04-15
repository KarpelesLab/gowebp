package gowebp

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestEncodeLossyDwebpSmoke is a cross-decoder smoke test: it encodes
// an image with the lossy encoder and decodes it back using libwebp's
// reference `dwebp` binary (instead of golang.org/x/image/webp). This
// catches the failure mode where our encoder and Go's pure-Go decoder
// share a spec misreading — the Go-only tests would pass while real
// browsers and libwebp would reject or misrender the output.
//
// The test skips when dwebp is not on PATH, so CI without libwebp
// installed remains green.
func TestEncodeLossyDwebpSmoke(t *testing.T) {
	dwebp, err := exec.LookPath("dwebp")
	if err != nil {
		t.Skip("dwebp not in PATH; install libwebp to enable cross-decoder smoke test")
	}
	t.Logf("using %s for cross-decoder validation", dwebp)

	// Gradient input — exercises all chroma/luma ranges.
	w, h := 64, 64
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 4),
				G: uint8(y * 4),
				B: uint8((x + y) * 2),
				A: 255,
			})
		}
	}

	cases := []struct {
		q       float32
		m       int
		minPSNR float64
	}{
		{75, 3, 30},
		{90, 3, 32},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := Encode(&buf, src, &Options{Lossy: true, Quality: c.q, Method: c.m}); err != nil {
			t.Fatalf("Q=%.0f M=%d: Encode: %v", c.q, c.m, err)
		}
		decoded, err := dwebpDecode(t, dwebp, buf.Bytes())
		if err != nil {
			t.Fatalf("Q=%.0f M=%d: dwebp decode: %v", c.q, c.m, err)
		}
		b := decoded.Bounds()
		if b.Dx() != w || b.Dy() != h {
			t.Errorf("Q=%.0f M=%d: dwebp decoded size %v, want %dx%d",
				c.q, c.m, b, w, h)
			continue
		}
		psnr := rgbPSNR(src, decoded)
		t.Logf("Q=%3.0f M=%d: %d bytes, dwebp RGB-PSNR %.2f dB",
			c.q, c.m, buf.Len(), psnr)
		if psnr < c.minPSNR {
			t.Errorf("Q=%.0f M=%d: dwebp RGB-PSNR %.2f dB below threshold %.2f dB — "+
				"libwebp disagrees with our encoder more than expected",
				c.q, c.m, psnr, c.minPSNR)
		}
	}
}

// TestEncodeLossyDwebpAlpha verifies that libwebp preserves the alpha
// channel exactly (ALPH chunk is uncompressed / VP8L-lossless) — a
// stricter check than the Go-decoder test because libwebp is the
// reference implementation.
func TestEncodeLossyDwebpAlpha(t *testing.T) {
	dwebp, err := exec.LookPath("dwebp")
	if err != nil {
		t.Skip("dwebp not in PATH")
	}
	w, h := 32, 32
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{200, 100, 50, uint8((x * 255) / w)})
		}
	}
	var buf bytes.Buffer
	if err := Encode(&buf, src, &Options{Lossy: true, Quality: 90}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := dwebpDecode(t, dwebp, buf.Bytes())
	if err != nil {
		t.Fatalf("dwebp decode: %v", err)
	}
	maxAErr := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			_, _, _, a := decoded.At(x, y).RGBA()
			got := uint8(a >> 8)
			want := uint8((x * 255) / w)
			d := int(got) - int(want)
			if d < 0 {
				d = -d
			}
			if d > maxAErr {
				maxAErr = d
			}
		}
	}
	if maxAErr > 1 {
		t.Errorf("dwebp alpha roundtrip error up to %d; want exact (ALPH is lossless)", maxAErr)
	}
}

// dwebpDecode runs `dwebp` on webpBytes and returns the decoded image.
// It uses a temp file + PNG output (dwebp always writes a file).
func dwebpDecode(t *testing.T, dwebpPath string, webpBytes []byte) (image.Image, error) {
	t.Helper()
	tmp := t.TempDir()
	inPath := filepath.Join(tmp, "in.webp")
	outPath := filepath.Join(tmp, "out.png")
	if err := os.WriteFile(inPath, webpBytes, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.Command(dwebpPath, inPath, "-o", outPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("dwebp stderr: %s", out)
		return nil, err
	}
	f, err := os.Open(outPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// rgbPSNR computes mean RGB-PSNR (not luma-only) between a source
// image and a decoded image, both interpreted as NRGBA. Alpha is
// ignored. Used to validate that libwebp's decoded output is close
// enough to the source — catches encoder bugs that a Go-only roundtrip
// might miss.
func rgbPSNR(src, dec image.Image) float64 {
	sn := toNRGBA(src)
	dn := toNRGBA(dec)
	b := sn.Bounds()
	var sumSq, n float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			si := sn.PixOffset(x, y)
			di := dn.PixOffset(x, y)
			for c := 0; c < 3; c++ {
				d := float64(sn.Pix[si+c]) - float64(dn.Pix[di+c])
				sumSq += d * d
				n++
			}
		}
	}
	if sumSq == 0 {
		return 99
	}
	mse := sumSq / n
	return 10 * math.Log10(255*255/mse)
}

package gowebp

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	xwebp "golang.org/x/image/webp"
)

// TestGalleryPSNR runs the lossy encoder on real-world photographic
// content (the 5 images referenced in the README benchmark table)
// across the full quality range, and reports PSNR + size so
// regressions show up in CI logs.
//
// The images are not committed. If testdata/1.png..5.png are missing,
// the test skips. Set NATIVEWEBP_FETCH=1 to auto-download them:
//
//	NATIVEWEBP_FETCH=1 go test -run TestGalleryPSNR -v
func TestGalleryPSNR(t *testing.T) {
	// Minimum Y-PSNR thresholds at Q=75 Method=3 per image. Values
	// are measured-floor minus 1 dB; if a future change regresses the
	// encoder by more than 1 dB on any of these photos, the test
	// fails.
	thresholds := map[string]float64{
		"1.png": 38, // measured ~39.2 dB (natural photo)
		"2.png": 41, // ~42.2 dB (high-color photo)
		"3.png": 42, // ~43.5 dB (portrait)
		"4.png": 37, // ~38.7 dB (logo/graphic)
		"5.png": 35, // ~36.9 dB (high-detail)
	}
	names := []string{"1.png", "2.png", "3.png", "4.png", "5.png"}
	dir := "testdata"

	missing := 0
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			missing++
		}
	}
	if missing > 0 {
		if os.Getenv("NATIVEWEBP_FETCH") == "" {
			t.Skipf("testdata/*.png not present (%d of %d missing); "+
				"re-run with NATIVEWEBP_FETCH=1 to auto-fetch, or populate manually",
				missing, len(names))
		}
		if err := fetchGallery(dir, names); err != nil {
			t.Fatalf("download: %v", err)
		}
	}

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			src, err := loadPNG(filepath.Join(dir, n))
			if err != nil {
				t.Fatalf("load %s: %v", n, err)
			}
			b := src.Bounds()
			pngBytes, _ := fileSize(filepath.Join(dir, n))
			t.Logf("%s: %dx%d source PNG %d bytes", n, b.Dx(), b.Dy(), pngBytes)

			for _, q := range []float32{25, 50, 75, 90} {
				for _, m := range []int{1, 3, 4} {
					var buf bytes.Buffer
					start := time.Now()
					err := Encode(&buf, src, &Options{Lossy: true, Quality: q, Method: m})
					dur := time.Since(start)
					if err != nil {
						t.Fatalf("Q=%.0f M=%d: %v", q, m, err)
					}
					dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
					if err != nil {
						t.Fatalf("Q=%.0f M=%d decode: %v", q, m, err)
					}
					// Alpha-bearing images decode to *image.NYCbCrA; use
					// its embedded YCbCr for the luma-plane comparison.
					var ycbcr *image.YCbCr
					switch d := dec.(type) {
					case *image.YCbCr:
						ycbcr = d
					case *image.NYCbCrA:
						ycbcr = &d.YCbCr
					default:
						t.Fatalf("Q=%.0f M=%d: unexpected decoded type %T", q, m, dec)
					}
					psnrY := photoPSNRLimited(src, ycbcr)
					t.Logf("  Q=%3.0f M=%d: %6d B (%.2fx PNG), Y-PSNR %.2f dB, %v",
						q, m, buf.Len(),
						float64(buf.Len())/float64(pngBytes),
						psnrY, dur.Round(time.Millisecond))
					// Regression threshold at the recommended settings.
					if q == 75 && m == 3 {
						if want, ok := thresholds[n]; ok && psnrY < want {
							t.Errorf("%s @ Q=75 M=3: Y-PSNR %.2f dB below threshold %.2f dB",
								n, psnrY, want)
						}
					}
				}
			}
		})
	}
}

func fetchGallery(dir string, names []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, n := range names {
		url := "https://www.gstatic.com/webp/gallery3/" + n
		path := filepath.Join(dir, n)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		resp, err := client.Get(url)
		if err != nil {
			return fmt.Errorf("GET %s: %w", url, err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
		}
		f, err := os.Create(path)
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(f, resp.Body)
		resp.Body.Close()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// photoPSNRLimited compares source RGB to decoded YCbCr under VP8's
// limited-range BT.601. Uses non-premultiplied RGB (NRGBA) to match
// what the encoder sees — src.At().RGBA() returns premultiplied,
// which would disagree with the encoder's NRGBA-based conversion on
// images with non-opaque alpha.
func photoPSNRLimited(src image.Image, dec *image.YCbCr) float64 {
	rect := src.Bounds()
	nrgba := toNRGBA(src)
	var sumSq float64
	var n float64
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			idx := nrgba.PixOffset(x, y)
			R := int32(nrgba.Pix[idx+0])
			G := int32(nrgba.Pix[idx+1])
			B := int32(nrgba.Pix[idx+2])
			Y := (66*R+129*G+25*B+128)>>8 + 16
			if Y < 0 {
				Y = 0
			}
			if Y > 255 {
				Y = 255
			}
			decY := int32(dec.Y[(y-rect.Min.Y)*dec.YStride+(x-rect.Min.X)])
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

func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok {
		return n
	}
	b := src.Bounds()
	out := image.NewNRGBA(b)
	// Draw preserves the NRGBA (non-premultiplied) convention when
	// copying from any source type.
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(x, y, src.At(x, y))
		}
	}
	return out
}

package gowebp

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkEncodeGallery measures encode time and output size against
// the 5 real-photo gallery images. Compares the lossless (VP8L) path
// against three lossy-method tiers (1, 3, 4) at Q=75, plus Q=50 and
// Q=90 with the recommended Method=4.
//
// Skipped if testdata/*.png aren't populated. Run with:
//
//	NATIVEWEBP_FETCH=1 go test -run TestGalleryPSNR -v   # populate
//	go test -bench BenchmarkEncodeGallery -benchtime=3x  # benchmark
func BenchmarkEncodeGallery(b *testing.B) {
	names := []string{"1.png", "2.png", "3.png", "4.png", "5.png"}
	for _, n := range names {
		path := filepath.Join("testdata", n)
		if _, err := os.Stat(path); err != nil {
			b.Skipf("testdata/%s missing; populate with NATIVEWEBP_FETCH=1 go test -run TestGalleryPSNR", n)
			return
		}
	}

	type scenario struct {
		name string
		run  func(w *bytes.Buffer, img image.Image) error
	}
	scenarios := []scenario{
		{"lossless", func(w *bytes.Buffer, img image.Image) error {
			return Encode(w, img, nil)
		}},
		{"lossy-q50-m4", func(w *bytes.Buffer, img image.Image) error {
			return Encode(w, img, &Options{Lossy: true, Quality: 50, Method: 4})
		}},
		{"lossy-q75-m1", func(w *bytes.Buffer, img image.Image) error {
			return Encode(w, img, &Options{Lossy: true, Quality: 75, Method: 1})
		}},
		{"lossy-q75-m3", func(w *bytes.Buffer, img image.Image) error {
			return Encode(w, img, &Options{Lossy: true, Quality: 75, Method: 3})
		}},
		{"lossy-q75-m4", func(w *bytes.Buffer, img image.Image) error {
			return Encode(w, img, &Options{Lossy: true, Quality: 75, Method: 4})
		}},
		{"lossy-q90-m4", func(w *bytes.Buffer, img image.Image) error {
			return Encode(w, img, &Options{Lossy: true, Quality: 90, Method: 4})
		}},
	}

	for _, n := range names {
		src, err := loadBenchPNG(filepath.Join("testdata", n))
		if err != nil {
			b.Fatal(err)
		}
		for _, sc := range scenarios {
			b.Run(fmt.Sprintf("%s/%s", n, sc.name), func(b *testing.B) {
				var buf bytes.Buffer
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					buf.Reset()
					if err := sc.run(&buf, src); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(buf.Len()), "bytes/frame")
			})
		}
	}
}

// BenchmarkEncodePNGBaseline is the reference `image/png` encoder using
// png.BestCompression (matching the upstream README's PNG row).
func BenchmarkEncodePNGBaseline(b *testing.B) {
	names := []string{"1.png", "2.png", "3.png", "4.png", "5.png"}
	for _, n := range names {
		path := filepath.Join("testdata", n)
		if _, err := os.Stat(path); err != nil {
			b.Skipf("testdata/%s missing", n)
			return
		}
	}

	enc := &png.Encoder{CompressionLevel: png.BestCompression}
	for _, n := range names {
		src, err := loadBenchPNG(filepath.Join("testdata", n))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(n, func(b *testing.B) {
			var buf bytes.Buffer
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf.Reset()
				if err := enc.Encode(&buf, src); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(buf.Len()), "bytes/frame")
		})
	}
}

func loadBenchPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

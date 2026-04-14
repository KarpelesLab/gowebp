package vp8enc

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	xwebp "golang.org/x/image/webp"
)

// TestBPredSolidColor verifies that both I16 and B_PRED paths
// produce near-perfect roundtrip on a solid-color input — defaults
// for top/left neighbors at frame edges give constant predictions,
// so the residual is zero and reconstruction exactly matches
// (modulo the YUV-colorspace discretization).
func TestBPredSolidColor(t *testing.T) {
	w, h := 16, 16
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{128, 128, 128, 255})
		}
	}

	for _, method := range []int{1, 2, 3} {
		var buf bytes.Buffer
		if err := EncodeWebP(&buf, src, EncodeOptions{Quality: 90, Method: method}); err != nil {
			t.Fatalf("M=%d: %v", method, err)
		}
		dec, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("M=%d: decode %v", method, err)
		}
		ycbcr := dec.(*image.YCbCr)
		// Solid gray 128 → limited-range Y ≈ 126.
		// Every pixel must be within ±2 of 126 for the roundtrip to be
		// considered near-perfect.
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := ycbcr.Y[y*ycbcr.YStride+x]
				if v < 124 || v > 128 {
					t.Errorf("M=%d pixel(%d,%d)=%d, want ~126", method, x, y, v)
					break
				}
			}
		}
		t.Logf("M=%d: %d bytes", method, buf.Len())
	}
}

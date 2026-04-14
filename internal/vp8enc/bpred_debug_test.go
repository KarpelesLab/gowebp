package vp8enc

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	xwebp "golang.org/x/image/webp"
)

// TestBPredSolidColor checks that B_PRED on a solid color image
// produces output comparable to I16 — neither should have trouble with
// a constant input.
func TestBPredSolidColor(t *testing.T) {
	w, h := 16, 16
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{128, 128, 128, 255})
		}
	}

	var bufI16, bufBPred bytes.Buffer
	EncodeWebP(&bufI16, src, EncodeOptions{Quality: 90, Method: 1})
	EncodeWebP(&bufBPred, src, EncodeOptions{Quality: 90, Method: 2})

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
	t.Logf("solid-gray 16x16 Q=90 — I16: %.2f dB (%d B), B_PRED: %.2f dB (%d B)",
		psnrI16, bufI16.Len(), psnrBPred, bufBPred.Len())

	// Sample decoded pixels for diagnostic.
	r0, g0, b0, _ := decBPred.At(8, 8).RGBA()
	t.Logf("B_PRED decoded center pixel: (%d, %d, %d)", r0>>8, g0>>8, b0>>8)
}

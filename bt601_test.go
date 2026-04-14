package gowebp

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

// TestBT601YCbCrColorMidGray verifies a concrete limited-range
// YCbCr→RGB conversion. Mid-gray in limited-range is Y=126, Cb=Cr=128
// (from the encoder: (66+129+25)*128/256 + 16 ≈ 126). The spec-correct
// inverse should produce RGB ≈ 128.
func TestBT601YCbCrColorMidGray(t *testing.T) {
	c := BT601YCbCrColor{Y: 126, Cb: 128, Cr: 128}
	r, g, b, a := c.RGBA()
	// RGBA returns 16-bit values; shift down to 8-bit for comparison.
	R := r >> 8
	G := g >> 8
	B := b >> 8
	if a != 0xffff {
		t.Errorf("alpha=%d, want 0xffff", a)
	}
	// Y=126 → 298*(126-16)+128 = 32908 >> 8 = 128. R=G=B=128.
	for name, v := range map[string]uint32{"R": R, "G": G, "B": B} {
		if v < 127 || v > 129 {
			t.Errorf("mid-gray %s=%d, want ~128", name, v)
		}
	}
}

// TestBT601YCbCrColorBoundaries verifies clipping at extreme values.
func TestBT601YCbCrColorBoundaries(t *testing.T) {
	// Pure black: Y=16, Cb=Cr=128 → should give RGB ≈ (0, 0, 0).
	r, g, b, _ := BT601YCbCrColor{Y: 16, Cb: 128, Cr: 128}.RGBA()
	for name, v := range map[string]uint32{"R": r, "G": g, "B": b} {
		if v > 0x0101 {
			t.Errorf("black %s=%d, want ~0", name, v>>8)
		}
	}
	// Pure white: Y=235, Cb=Cr=128 → should give RGB ≈ (255, 255, 255).
	r, g, b, _ = BT601YCbCrColor{Y: 235, Cb: 128, Cr: 128}.RGBA()
	for name, v := range map[string]uint32{"R": r, "G": g, "B": b} {
		if v>>8 < 254 {
			t.Errorf("white %s=%d, want ~255", name, v>>8)
		}
	}
	// Out-of-range input should clip, not overflow.
	r, g, b, _ = BT601YCbCrColor{Y: 255, Cb: 0, Cr: 255}.RGBA()
	// R should clip to 255.
	if r>>8 != 255 {
		t.Errorf("over-bright Y/Cr clip: R=%d, want 255", r>>8)
	}
	_ = g
	_ = b
}

// TestBT601vsStdlibYCbCr verifies that our type gives MATERIALLY
// different RGB output than Go's stdlib YCbCrToRGB, confirming the
// fix actually matters.
func TestBT601vsStdlibYCbCr(t *testing.T) {
	// Y=137 Cb=157 Cr=103: this is roughly what YUV(100, 150, 200)
	// encodes to in limited range.
	ourR, ourG, ourB, _ := BT601YCbCrColor{Y: 137, Cb: 157, Cr: 103}.RGBA()
	stdR, stdG, stdB := color.YCbCrToRGB(137, 157, 103)

	// Bit-shift our 16-bit back to 8.
	ourR8 := int(ourR >> 8)
	ourG8 := int(ourG >> 8)
	ourB8 := int(ourB >> 8)

	t.Logf("source YUV(137,157,103) → BT.601(limited): RGB(%d,%d,%d)",
		ourR8, ourG8, ourB8)
	t.Logf("source YUV(137,157,103) → JFIF(full-range): RGB(%d,%d,%d)",
		stdR, stdG, stdB)

	// Our conversion should hit approximately (100, 150, 200) — the
	// original RGB that produced this YUV. Stdlib's JFIF will be
	// several units off.
	if ourR8 < 98 || ourR8 > 102 {
		t.Errorf("BT.601 R=%d, want 98..102 (expected near 100)", ourR8)
	}
	if ourG8 < 148 || ourG8 > 152 {
		t.Errorf("BT.601 G=%d, want 148..152 (expected near 150)", ourG8)
	}
	if ourB8 < 198 || ourB8 > 202 {
		t.Errorf("BT.601 B=%d, want 198..202 (expected near 200)", ourB8)
	}

	// Sanity: stdlib DOESN'T produce the right answer for VP8 pixels.
	// This confirms we're not fixing a non-problem.
	diff := abs(ourR8-int(stdR)) + abs(ourG8-int(stdG)) + abs(ourB8-int(stdB))
	if diff < 5 {
		t.Errorf("BT.601 and JFIF inverse agreed too closely (diff=%d); expected ~15+ unit offset",
			diff)
	}
}

// TestDecodeWrapsBT601 verifies that gowebp.Decode returns a
// *BT601YCbCr wrapper for lossy (VP8) sources — so users get correct
// colors without doing anything special.
func TestDecodeWrapsBT601(t *testing.T) {
	w, h := 32, 32
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{100, 150, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := Encode(&buf, src, &Options{Lossy: true, Quality: 95, Method: 4}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	wrapped, ok := dec.(*BT601YCbCr)
	if !ok {
		t.Fatalf("Decode returned %T, want *BT601YCbCr for VP8 source", dec)
	}
	// Sample center pixel and confirm RGB is close to source.
	r, g, b, _ := wrapped.At(16, 16).RGBA()
	R := int(r >> 8)
	G := int(g >> 8)
	B := int(b >> 8)
	t.Logf("decoded center pixel via BT601YCbCr: R=%d G=%d B=%d", R, G, B)
	// Q=95 on a solid color should land very close to source (100,150,200).
	if abs(R-100) > 4 || abs(G-150) > 4 || abs(B-200) > 4 {
		t.Errorf("Q=95 solid color roundtrip: got (%d,%d,%d), want near (100,150,200)", R, G, B)
	}
}

// TestDecodeLosslessPassesThrough confirms that VP8L decodes don't
// get wrapped (they're NRGBA from x/image/webp — no BT.601 conversion
// needed).
func TestDecodeLosslessPassesThrough(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			src.SetNRGBA(x, y, color.NRGBA{10, 20, 30, 255})
		}
	}
	var buf bytes.Buffer
	if err := Encode(&buf, src, nil); err != nil {
		t.Fatalf("Encode lossless: %v", err)
	}
	dec, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, ok := dec.(*BT601YCbCr); ok {
		t.Errorf("Decode wrapped a VP8L image in BT601YCbCr; shouldn't happen")
	}
	// Should be exact pixel roundtrip.
	r, g, b, _ := dec.At(4, 4).RGBA()
	if r>>8 != 10 || g>>8 != 20 || b>>8 != 30 {
		t.Errorf("lossless roundtrip: got (%d,%d,%d), want (10,20,30)",
			r>>8, g>>8, b>>8)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

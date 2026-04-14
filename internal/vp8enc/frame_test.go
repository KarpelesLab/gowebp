package vp8enc

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	xvp8 "golang.org/x/image/vp8"
	xwebp "golang.org/x/image/webp"
)

// TestEncodeFrameDecodesThroughXImage is the Phase-A/B integration
// milestone: emit a minimum-viable keyframe (all-skip, all DC_PRED) for a
// solid-color image and verify it parses cleanly through the reference
// pure-Go decoder. A solid-color input reconstructs as mid-gray since our
// skip-everything encoder discards all residuals, but the bitstream
// structure is what's under test — not pixel fidelity.
func TestEncodeFrameDecodesThroughXImage(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{128, 128, 128, 255})
		}
	}

	var buf bytes.Buffer
	if err := EncodeFrame(&buf, img, EncodeOptions{Quality: 75}); err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	raw := buf.Bytes()
	t.Logf("emitted %d VP8 bytes for 16x16 input", len(raw))

	d := xvp8.NewDecoder()
	d.Init(bytes.NewReader(raw), len(raw))

	fh, err := d.DecodeFrameHeader()
	if err != nil {
		t.Fatalf("DecodeFrameHeader: %v", err)
	}
	if !fh.KeyFrame {
		t.Errorf("frame not marked as keyframe")
	}
	if !fh.ShowFrame {
		t.Errorf("show_frame bit not set")
	}
	if fh.Width != 16 || fh.Height != 16 {
		t.Errorf("decoded size %dx%d, want 16x16", fh.Width, fh.Height)
	}

	decoded, err := d.DecodeFrame()
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if decoded == nil {
		t.Fatal("DecodeFrame returned nil image")
	}
	if decoded.Rect.Dx() != 16 || decoded.Rect.Dy() != 16 {
		t.Errorf("decoded rect %v, want 16x16", decoded.Rect)
	}
}

// TestEncodeWebPDecodesThroughXWebp exercises the RIFF/VP8 container path
// and verifies the high-level x/image/webp decoder can consume it.
func TestEncodeWebPDecodesThroughXWebp(t *testing.T) {
	cases := []struct{ w, h int }{
		{16, 16},
		{32, 16},
		{48, 48},
		{17, 17}, // non-MB-aligned
	}
	for _, c := range cases {
		img := image.NewNRGBA(image.Rect(0, 0, c.w, c.h))
		for y := 0; y < c.h; y++ {
			for x := 0; x < c.w; x++ {
				img.SetNRGBA(x, y, color.NRGBA{200, 100, 50, 255})
			}
		}

		var buf bytes.Buffer
		if err := EncodeWebP(&buf, img, EncodeOptions{Quality: 75}); err != nil {
			t.Fatalf("%dx%d: EncodeWebP: %v", c.w, c.h, err)
		}

		decoded, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("%dx%d: xwebp.Decode: %v", c.w, c.h, err)
		}
		got := decoded.Bounds()
		if got.Dx() != c.w || got.Dy() != c.h {
			t.Errorf("%dx%d: decoded size %v", c.w, c.h, got)
		}
	}
}

// TestEncodeFrameTagGolden pins the 10-byte uncompressed frame tag for a
// fixed input. The first_partition_size field is derived from the
// partition length so it will shift if partition-0 contents change; the
// other 7 bytes (start code + dims) are structurally fixed.
func TestEncodeFrameTagGolden(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 24))
	var buf bytes.Buffer
	if err := EncodeFrame(&buf, img, EncodeOptions{Quality: 75}); err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	raw := buf.Bytes()
	if len(raw) < 10 {
		t.Fatalf("output too short: %d bytes", len(raw))
	}

	// Start code is fixed.
	if raw[3] != 0x9d || raw[4] != 0x01 || raw[5] != 0x2a {
		t.Errorf("start code mismatch: got %x %x %x", raw[3], raw[4], raw[5])
	}
	// Width = 32 → bytes 6-7 = 0x20 0x00 (scale bits = 0).
	if raw[6] != 0x20 || raw[7]&0x3f != 0x00 {
		t.Errorf("width bytes = %x %x, want 20 00", raw[6], raw[7])
	}
	// Height = 24 → bytes 8-9 = 0x18 0x00.
	if raw[8] != 0x18 || raw[9]&0x3f != 0x00 {
		t.Errorf("height bytes = %x %x, want 18 00", raw[8], raw[9])
	}
	// Key-frame bit (byte 0 bit 0) = 0.
	if raw[0]&1 != 0 {
		t.Errorf("key_frame bit set; should be 0 for keyframe")
	}
	// show_frame bit (byte 0 bit 4) = 1.
	if raw[0]&(1<<4) == 0 {
		t.Errorf("show_frame bit not set")
	}
}

func TestEncodeFrameRejectsInvalid(t *testing.T) {
	// Zero-size image.
	img := image.NewNRGBA(image.Rect(0, 0, 0, 0))
	if err := EncodeFrame(new(bytes.Buffer), img, EncodeOptions{}); err == nil {
		t.Error("expected error for empty image")
	}
	// Oversized image (> 16383).
	big := image.NewNRGBA(image.Rect(0, 0, 20000, 10))
	if err := EncodeFrame(new(bytes.Buffer), big, EncodeOptions{}); err == nil {
		t.Error("expected error for oversized image")
	}
	// Nil image.
	if err := EncodeFrame(new(bytes.Buffer), nil, EncodeOptions{}); err == nil {
		t.Error("expected error for nil image")
	}
}

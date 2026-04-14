package vp8enc

import (
	"image"
	"image/color"
	"testing"
)

// TestFrameAlignment verifies MB alignment padding for odd-sized images.
func TestFrameAlignment(t *testing.T) {
	cases := []struct{ w, h, mbW, mbH int }{
		{16, 16, 1, 1},
		{17, 17, 2, 2},
		{32, 32, 2, 2},
		{1, 1, 1, 1},
		{100, 50, 7, 4},
	}
	for _, c := range cases {
		img := image.NewNRGBA(image.Rect(0, 0, c.w, c.h))
		f := RGBAToFrame(img)
		if f.MBWidth != c.mbW || f.MBHeight != c.mbH {
			t.Errorf("%dx%d: got MB (%d,%d), want (%d,%d)",
				c.w, c.h, f.MBWidth, f.MBHeight, c.mbW, c.mbH)
		}
		if len(f.Y) != f.YStride*c.mbH*16 {
			t.Errorf("%dx%d: Y size %d, want %d", c.w, c.h, len(f.Y), f.YStride*c.mbH*16)
		}
		if len(f.Cb) != f.UVStride*c.mbH*8 {
			t.Errorf("%dx%d: Cb size %d, want %d", c.w, c.h, len(f.Cb), f.UVStride*c.mbH*8)
		}
	}
}

// TestYUVSolidColors encodes known-color blocks and checks the resulting
// YCbCr values match BT.601 fixed-point expectations.
func TestYUVSolidColors(t *testing.T) {
	cases := []struct {
		name       string
		r, g, b    uint8
		yLo, yHi   int
		cbLo, cbHi int
		crLo, crHi int
	}{
		// BT.601 (limited-range studio): pure white -> Y≈235, Cb/Cr≈128
		// Pure black -> Y≈16
		{"white", 255, 255, 255, 234, 236, 127, 129, 127, 129},
		{"black", 0, 0, 0, 15, 17, 127, 129, 127, 129},
		{"red", 255, 0, 0, 80, 84, 89, 93, 239, 241},
		{"green", 0, 255, 0, 144, 146, 53, 55, 33, 37},
		{"blue", 0, 0, 255, 40, 42, 239, 241, 109, 111},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
			col := color.NRGBA{c.r, c.g, c.b, 0xff}
			for y := 0; y < 16; y++ {
				for x := 0; x < 16; x++ {
					img.SetNRGBA(x, y, col)
				}
			}
			f := RGBAToFrame(img)
			y := int(f.Y[0])
			cb := int(f.Cb[0])
			cr := int(f.Cr[0])
			if y < c.yLo || y > c.yHi {
				t.Errorf("Y=%d not in [%d,%d]", y, c.yLo, c.yHi)
			}
			if cb < c.cbLo || cb > c.cbHi {
				t.Errorf("Cb=%d not in [%d,%d]", cb, c.cbLo, c.cbHi)
			}
			if cr < c.crLo || cr > c.crHi {
				t.Errorf("Cr=%d not in [%d,%d]", cr, c.crLo, c.crHi)
			}
		})
	}
}

// TestYUVEdgePadding confirms that pixels beyond the display size replicate
// the nearest edge pixel, not a random color.
func TestYUVEdgePadding(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 17, 17))
	// Top-left quadrant red, rest white.
	for y := 0; y < 17; y++ {
		for x := 0; x < 17; x++ {
			var c color.NRGBA
			if x < 10 && y < 10 {
				c = color.NRGBA{255, 0, 0, 255}
			} else {
				c = color.NRGBA{255, 255, 255, 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	f := RGBAToFrame(img)
	// Pixel (17, 17) padded; must be replicated from (16, 16) which is white.
	if v := f.Y[17*f.YStride+17]; v < 230 {
		t.Errorf("padded pixel should be white-ish, got Y=%d", v)
	}
	// Pixel (0, 0) is red area; Y should be ~82.
	if v := f.Y[0]; v < 70 || v > 95 {
		t.Errorf("pixel (0,0) expected red Y~82, got %d", v)
	}
}

func TestExtractAlphaOpaque(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 0xff})
		}
	}
	if a := ExtractAlpha(img); a != nil {
		t.Errorf("fully opaque image should return nil alpha, got %d bytes", len(a))
	}
}

func TestExtractAlphaGradient(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, uint8(x * 32)})
		}
	}
	a := ExtractAlpha(img)
	if len(a) != 64 {
		t.Fatalf("alpha size=%d want 64", len(a))
	}
	if a[0] != 0 {
		t.Errorf("alpha[0]=%d want 0", a[0])
	}
	if a[7] != 7*32 {
		t.Errorf("alpha[7]=%d want %d", a[7], 7*32)
	}
}

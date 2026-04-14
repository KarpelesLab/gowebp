package vp8enc

import (
	"image"
	"image/draw"
)

// Frame is a macroblock-aligned YCbCr 4:2:0 image in planar layout.
//
// The display size is (Width, Height) in pixels. The allocated planes are
// padded up to the next multiple of 16 (luma) or 8 (chroma) so macroblock
// loops can read uniformly. Edge rows/columns are filled by replicating
// the last valid pixel, matching libwebp's padding policy.
type Frame struct {
	Width, Height int
	MBWidth       int // ceil(Width/16)
	MBHeight      int // ceil(Height/16)

	Y  []byte // YStride * MBHeight*16
	Cb []byte // UVStride * MBHeight*8
	Cr []byte // UVStride * MBHeight*8

	YStride  int // == MBWidth*16
	UVStride int // == MBWidth*8
}

// NewFrame allocates a Frame with zeroed planes sized for (w, h) display
// dimensions. The planes are MB-aligned.
func NewFrame(w, h int) *Frame {
	mbW := (w + 15) >> 4
	mbH := (h + 15) >> 4
	f := &Frame{
		Width:    w,
		Height:   h,
		MBWidth:  mbW,
		MBHeight: mbH,
		YStride:  mbW * 16,
		UVStride: mbW * 8,
	}
	f.Y = make([]byte, f.YStride*mbH*16)
	f.Cb = make([]byte, f.UVStride*mbH*8)
	f.Cr = make([]byte, f.UVStride*mbH*8)
	return f
}

// RGBAToFrame converts an image.Image to a macroblock-aligned 4:2:0 YCbCr
// Frame using BT.601 fixed-point coefficients (matching RFC 6386 and
// libwebp's picture_csp_enc.c). Padding pixels beyond (Width, Height)
// replicate the nearest edge pixel.
//
// Alpha is ignored here; ALPH chunk handling is the caller's responsibility.
func RGBAToFrame(img image.Image) *Frame {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	frame := NewFrame(w, h)

	// Materialize to NRGBA so we read RGB in a predictable layout.
	nrgba, ok := img.(*image.NRGBA)
	if !ok || !nrgba.Rect.Eq(b) {
		nrgba = image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.Draw(nrgba, nrgba.Bounds(), img, b.Min, draw.Src)
		b = nrgba.Bounds()
	}
	srcMinX := b.Min.X
	srcMinY := b.Min.Y

	// Luma: every pixel.
	for y := 0; y < frame.MBHeight*16; y++ {
		sy := clampInt(y, 0, h-1)
		srcRow := srcMinY + sy
		rowOff := y * frame.YStride
		base := nrgba.PixOffset(srcMinX, srcRow)
		pix := nrgba.Pix
		for x := 0; x < frame.MBWidth*16; x++ {
			sx := clampInt(x, 0, w-1)
			p := base + sx*4
			r := int32(pix[p])
			g := int32(pix[p+1])
			bl := int32(pix[p+2])
			// Y = (66R + 129G + 25B + 128) >> 8 + 16
			Y := (66*r+129*g+25*bl+128)>>8 + 16
			frame.Y[rowOff+x] = byte(clampInt32(Y, 0, 255))
		}
	}

	// Chroma: 2x2 averaged.
	for cy := 0; cy < frame.MBHeight*8; cy++ {
		y0 := cy * 2
		rowOff := cy * frame.UVStride
		for cx := 0; cx < frame.MBWidth*8; cx++ {
			x0 := cx * 2
			var sumR, sumG, sumB int32
			for dy := 0; dy < 2; dy++ {
				sy := clampInt(y0+dy, 0, h-1)
				srcRow := srcMinY + sy
				base := nrgba.PixOffset(srcMinX, srcRow)
				for dx := 0; dx < 2; dx++ {
					sx := clampInt(x0+dx, 0, w-1)
					p := base + sx*4
					sumR += int32(nrgba.Pix[p])
					sumG += int32(nrgba.Pix[p+1])
					sumB += int32(nrgba.Pix[p+2])
				}
			}
			r := (sumR + 2) >> 2
			g := (sumG + 2) >> 2
			bl := (sumB + 2) >> 2
			// Cb = (-38R - 74G + 112B + 128) >> 8 + 128
			// Cr = (112R - 94G - 18B + 128) >> 8 + 128
			Cb := (-38*r-74*g+112*bl+128)>>8 + 128
			Cr := (112*r-94*g-18*bl+128)>>8 + 128
			frame.Cb[rowOff+cx] = byte(clampInt32(Cb, 0, 255))
			frame.Cr[rowOff+cx] = byte(clampInt32(Cr, 0, 255))
		}
	}

	return frame
}

// ExtractAlpha returns the 8-bit alpha plane for img, sized exactly (w, h)
// — no MB padding. Returns nil if the image has no alpha channel or is
// fully opaque. Used by the ALPH chunk encoder.
func ExtractAlpha(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	nrgba, ok := img.(*image.NRGBA)
	if !ok || !nrgba.Rect.Eq(b) {
		nrgba = image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.Draw(nrgba, nrgba.Bounds(), img, b.Min, draw.Src)
		b = nrgba.Bounds()
	}
	out := make([]byte, w*h)
	fullyOpaque := true
	for y := 0; y < h; y++ {
		base := nrgba.PixOffset(b.Min.X, b.Min.Y+y)
		for x := 0; x < w; x++ {
			a := nrgba.Pix[base+x*4+3]
			if a != 0xff {
				fullyOpaque = false
			}
			out[y*w+x] = a
		}
	}
	if fullyOpaque {
		return nil
	}
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampInt32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

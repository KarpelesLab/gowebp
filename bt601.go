package gowebp

import (
	"image"
	"image/color"
)

// BT601YCbCrColor is a single pixel in VP8 limited-range BT.601 color
// space (luma 16–235, chroma 16–240), the color space WebP files
// actually store per RFC 6386.
//
// It exists because Go's stdlib `image/color.YCbCrToRGB` applies the
// JFIF (full-range) inverse even to values that were produced under
// limited-range encoding, which shifts the resulting RGB by 2–5 units
// per channel. BT601YCbCrColor.RGBA() uses the spec-correct inverse:
//
//	R = clip((298·(Y-16) + 409·(Cr-128) + 128) >> 8)
//	G = clip((298·(Y-16) -  100·(Cb-128) - 208·(Cr-128) + 128) >> 8)
//	B = clip((298·(Y-16) + 516·(Cb-128) + 128) >> 8)
//
// Decode returns images whose pixels are BT601YCbCrColor (wrapped in
// BT601YCbCr or BT601NYCbCrA) for VP8 sources, so calling
// img.At(x, y).RGBA() produces the colors a real VP8 decoder (libwebp,
// browsers) would show.
type BT601YCbCrColor struct {
	Y, Cb, Cr uint8
}

// RGBA implements color.Color using the VP8 spec's limited-range
// BT.601 inverse. Returns premultiplied 16-bit components (α is always
// 0xffff for this type).
func (c BT601YCbCrColor) RGBA() (r, g, b, a uint32) {
	Y := int32(c.Y) - 16
	Cb := int32(c.Cb) - 128
	Cr := int32(c.Cr) - 128
	R := (298*Y + 409*Cr + 128) >> 8
	G := (298*Y - 100*Cb - 208*Cr + 128) >> 8
	B := (298*Y + 516*Cb + 128) >> 8
	if R < 0 {
		R = 0
	} else if R > 255 {
		R = 255
	}
	if G < 0 {
		G = 0
	} else if G > 255 {
		G = 255
	}
	if B < 0 {
		B = 0
	} else if B > 255 {
		B = 255
	}
	// color.Color expects 16-bit values; 0x101 == 257 scales 0..255
	// to 0..65535 the same way as stdlib's color types.
	return uint32(R) * 0x101, uint32(G) * 0x101, uint32(B) * 0x101, 0xffff
}

// BT601NYCbCrAColor is BT601YCbCrColor with a non-premultiplied alpha
// channel. Used by the WebP decoder output when the source has an
// ALPH chunk.
type BT601NYCbCrAColor struct {
	BT601YCbCrColor
	A uint8
}

// RGBA returns premultiplied 16-bit components.
func (c BT601NYCbCrAColor) RGBA() (r, g, b, a uint32) {
	r, g, b, _ = c.BT601YCbCrColor.RGBA()
	a = uint32(c.A) * 0x101
	// Premultiply.
	r = r * a / 0xffff
	g = g * a / 0xffff
	b = b * a / 0xffff
	return
}

// BT601YCbCrColorModel and BT601NYCbCrAColorModel convert an arbitrary
// color into the limited-range BT.601 representation.
var (
	BT601YCbCrColorModel  = color.ModelFunc(bt601YCbCrModel)
	BT601NYCbCrAColorModel = color.ModelFunc(bt601NYCbCrAModel)
)

func bt601YCbCrModel(c color.Color) color.Color {
	if _, ok := c.(BT601YCbCrColor); ok {
		return c
	}
	r, g, b, _ := c.RGBA()
	R := int32(r >> 8)
	G := int32(g >> 8)
	B := int32(b >> 8)
	Y := (66*R + 129*G + 25*B + 128) >> 8 + 16
	Cb := (-38*R - 74*G + 112*B + 128) >> 8 + 128
	Cr := (112*R - 94*G - 18*B + 128) >> 8 + 128
	return BT601YCbCrColor{
		Y:  clamp0255(Y),
		Cb: clamp0255(Cb),
		Cr: clamp0255(Cr),
	}
}

func bt601NYCbCrAModel(c color.Color) color.Color {
	if _, ok := c.(BT601NYCbCrAColor); ok {
		return c
	}
	_, _, _, a := c.RGBA()
	yc := bt601YCbCrModel(c).(BT601YCbCrColor)
	return BT601NYCbCrAColor{BT601YCbCrColor: yc, A: uint8(a >> 8)}
}

func clamp0255(v int32) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// BT601YCbCr wraps an *image.YCbCr whose samples are in VP8's
// limited-range BT.601. It forwards everything to the embedded image
// except At(), which returns BT601YCbCrColor for spec-correct RGB
// conversion.
//
// Callers who specifically want the raw stdlib YCbCr (e.g. for fast
// plane-level access) can dereference the embedded *image.YCbCr
// field directly.
type BT601YCbCr struct {
	*image.YCbCr
}

func (b *BT601YCbCr) At(x, y int) color.Color {
	return b.BT601YCbCrAt(x, y)
}

// BT601YCbCrAt is a typed helper that avoids the color.Color interface
// allocation when callers know they want BT601YCbCrColor.
func (b *BT601YCbCr) BT601YCbCrAt(x, y int) BT601YCbCrColor {
	if !(image.Point{X: x, Y: y}.In(b.Rect)) {
		return BT601YCbCrColor{}
	}
	yi := b.YOffset(x, y)
	ci := b.COffset(x, y)
	return BT601YCbCrColor{Y: b.Y[yi], Cb: b.Cb[ci], Cr: b.Cr[ci]}
}

func (b *BT601YCbCr) ColorModel() color.Model { return BT601YCbCrColorModel }

// BT601NYCbCrA wraps an *image.NYCbCrA for sources with an ALPH chunk.
type BT601NYCbCrA struct {
	*image.NYCbCrA
}

func (b *BT601NYCbCrA) At(x, y int) color.Color {
	return b.BT601NYCbCrAAt(x, y)
}

func (b *BT601NYCbCrA) BT601NYCbCrAAt(x, y int) BT601NYCbCrAColor {
	if !(image.Point{X: x, Y: y}.In(b.Rect)) {
		return BT601NYCbCrAColor{}
	}
	yi := b.YOffset(x, y)
	ci := b.COffset(x, y)
	ai := b.AOffset(x, y)
	return BT601NYCbCrAColor{
		BT601YCbCrColor: BT601YCbCrColor{Y: b.Y[yi], Cb: b.Cb[ci], Cr: b.Cr[ci]},
		A:               b.A[ai],
	}
}

func (b *BT601NYCbCrA) ColorModel() color.Model { return BT601NYCbCrAColorModel }

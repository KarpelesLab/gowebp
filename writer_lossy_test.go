package nativewebp

import (
    "bytes"
    "image"
    "image/color"
    "testing"

    xwebp "golang.org/x/image/webp"
)

// TestEncodeLossyRoundtrip verifies that Options.Lossy=true produces a
// VP8-encoded file that decodes through x/image/webp's Decode. The
// Phase-B scaffolding skip-all encoder doesn't preserve content, but the
// bitstream must be structurally valid and dimensions must match.
func TestEncodeLossyRoundtrip(t *testing.T) {
    cases := []struct{ w, h int }{
        {16, 16},
        {32, 32},
        {17, 23}, // non-MB-aligned to exercise padding
        {48, 16},
    }
    for _, c := range cases {
        img := image.NewNRGBA(image.Rect(0, 0, c.w, c.h))
        for y := 0; y < c.h; y++ {
            for x := 0; x < c.w; x++ {
                img.SetNRGBA(x, y, color.NRGBA{120, 180, 90, 255})
            }
        }

        var buf bytes.Buffer
        if err := Encode(&buf, img, &Options{Lossy: true, Quality: 75}); err != nil {
            t.Fatalf("%dx%d: Encode(Lossy=true): %v", c.w, c.h, err)
        }
        // The produced file must begin with "RIFF....WEBPVP8 " and decode
        // through the standard library webp decoder.
        if !bytes.Equal(buf.Bytes()[:4], []byte("RIFF")) {
            t.Errorf("%dx%d: missing RIFF magic", c.w, c.h)
        }
        if !bytes.Equal(buf.Bytes()[8:12], []byte("WEBP")) {
            t.Errorf("%dx%d: missing WEBP magic", c.w, c.h)
        }
        if !bytes.Equal(buf.Bytes()[12:16], []byte("VP8 ")) {
            t.Errorf("%dx%d: missing VP8 chunk (got %q)",
                c.w, c.h, buf.Bytes()[12:16])
        }

        decoded, err := xwebp.Decode(bytes.NewReader(buf.Bytes()))
        if err != nil {
            t.Fatalf("%dx%d: xwebp.Decode: %v", c.w, c.h, err)
        }
        bnd := decoded.Bounds()
        if bnd.Dx() != c.w || bnd.Dy() != c.h {
            t.Errorf("%dx%d: decoded size %v", c.w, c.h, bnd)
        }
    }
}

// TestEncodeLosslessUnchanged guards against the lossy dispatch breaking
// the existing VP8L path: default options (nil or Lossy=false) must still
// produce VP8L output.
func TestEncodeLosslessUnchanged(t *testing.T) {
    img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
    for y := 0; y < 8; y++ {
        for x := 0; x < 8; x++ {
            img.SetNRGBA(x, y, color.NRGBA{uint8(x * 32), uint8(y * 32), 50, 255})
        }
    }
    var nilOpts, falseOpts bytes.Buffer
    if err := Encode(&nilOpts, img, nil); err != nil {
        t.Fatalf("Encode(nil): %v", err)
    }
    if err := Encode(&falseOpts, img, &Options{Lossy: false}); err != nil {
        t.Fatalf("Encode(Lossy=false): %v", err)
    }
    if !bytes.Equal(nilOpts.Bytes(), falseOpts.Bytes()) {
        t.Errorf("nil opts and Lossy=false produced different output (%d vs %d bytes)",
            nilOpts.Len(), falseOpts.Len())
    }
    // Must be a VP8L file, not VP8.
    if !bytes.Contains(nilOpts.Bytes()[:20], []byte("VP8L")) {
        t.Errorf("default Encode produced non-VP8L output")
    }
}

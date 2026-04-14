package nativewebp

import (
    "bytes"
    "fmt"
    "image"
    "image/color"
    "testing"

    xvp8 "golang.org/x/image/vp8"
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

// TestEncodeAllLossy verifies that Options.Lossy=true inside EncodeAll
// produces an animation where each frame is a VP8 chunk inside ANMF.
// The x/image/webp decoder only decodes the first frame of an animation
// (it treats the whole thing as a static WebP), so we verify structure
// at the chunk level and decode the first frame's pixel dimensions.
func TestEncodeAllLossy(t *testing.T) {
    w, h := 32, 32
    makeFrame := func(r, g, b uint8) image.Image {
        img := image.NewNRGBA(image.Rect(0, 0, w, h))
        for y := 0; y < h; y++ {
            for x := 0; x < w; x++ {
                img.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
            }
        }
        return img
    }
    ani := &Animation{
        Images:          []image.Image{makeFrame(200, 50, 50), makeFrame(50, 200, 50), makeFrame(50, 50, 200)},
        Durations:       []uint{100, 100, 100},
        Disposals:       []uint{0, 0, 0},
        LoopCount:       0,
        BackgroundColor: 0xffffffff,
    }
    var buf bytes.Buffer
    if err := EncodeAll(&buf, ani, &Options{Lossy: true, Quality: 75}); err != nil {
        t.Fatalf("EncodeAll(Lossy=true): %v", err)
    }
    data := buf.Bytes()
    // Must contain VP8X + ANIM + ANMF chunks + VP8 payloads.
    if !bytes.Equal(data[:4], []byte("RIFF")) {
        t.Fatalf("missing RIFF magic")
    }
    if !bytes.Contains(data, []byte("VP8X")) {
        t.Errorf("missing VP8X chunk for animation container")
    }
    if !bytes.Contains(data, []byte("ANIM")) {
        t.Errorf("missing ANIM chunk")
    }
    if !bytes.Contains(data, []byte("ANMF")) {
        t.Errorf("missing ANMF chunk")
    }
    // Must contain VP8 chunks (space-padded to 4 chars) — note this
    // differs from VP8L which the default (non-lossy) path emits.
    if !bytes.Contains(data, []byte("VP8 ")) {
        t.Errorf("missing VP8 chunk for lossy frame")
    }
    if bytes.Contains(data, []byte("VP8L")) {
        t.Errorf("unexpected VP8L chunk in lossy animation")
    }
    // x/image/webp has no animation support; parse chunk-by-chunk and
    // ensure each ANMF's embedded VP8 payload decodes as a valid
    // standalone VP8 keyframe via the low-level x/image/vp8 decoder.
    verifyLossyAnimFrames(t, data, w, h)
}

func verifyLossyAnimFrames(t *testing.T, data []byte, expectedW, expectedH int) {
    t.Helper()
    // Skip past RIFF header (12 bytes: "RIFF" + size + "WEBP").
    i := 12
    frameCount := 0
    for i+8 <= len(data) {
        fourcc := string(data[i : i+4])
        size := uint32(data[i+4]) | uint32(data[i+5])<<8 |
            uint32(data[i+6])<<16 | uint32(data[i+7])<<24
        body := data[i+8 : i+8+int(size)]
        if fourcc == "ANMF" {
            // Frame header is 16 bytes; sub-chunk follows at offset 16.
            if len(body) < 16+8 {
                t.Errorf("ANMF frame too short: %d bytes", len(body))
                return
            }
            subFourCC := string(body[16:20])
            subSize := uint32(body[20]) | uint32(body[21])<<8 |
                uint32(body[22])<<16 | uint32(body[23])<<24
            if subFourCC != "VP8 " {
                t.Errorf("ANMF sub-chunk is %q, want %q", subFourCC, "VP8 ")
                return
            }
            vp8Payload := body[24 : 24+int(subSize)]
            if err := decodeVP8Frame(vp8Payload, expectedW, expectedH); err != nil {
                t.Errorf("frame %d VP8 decode: %v", frameCount, err)
            }
            frameCount++
        }
        i += 8 + int(size)
        if size&1 == 1 {
            i++
        }
    }
    if frameCount == 0 {
        t.Errorf("no ANMF frames found")
    }
}

func decodeVP8Frame(payload []byte, wantW, wantH int) error {
    d := xvp8.NewDecoder()
    d.Init(bytes.NewReader(payload), len(payload))
    fh, err := d.DecodeFrameHeader()
    if err != nil {
        return err
    }
    if fh.Width != wantW || fh.Height != wantH {
        return fmt.Errorf("frame size %dx%d, want %dx%d",
            fh.Width, fh.Height, wantW, wantH)
    }
    _, err = d.DecodeFrame()
    return err
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

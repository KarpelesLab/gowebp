package gowebp

import (
    //------------------------------
    //general
    //------------------------------
    "io"
    "bytes"
    "encoding/binary"
    //------------------------------
    //imaging
    //------------------------------
    "image"
    //------------------------------
    //errors
    //------------------------------
    decoderWebP "golang.org/x/image/webp"
)

// registers the webp decoder so image.Decode can detect and use it.
func init() {
    image.RegisterFormat("webp", "RIFF", Decode, DecodeConfig)
}

// Decode reads a WebP image from the provided io.Reader and returns it as an image.Image.
//
// For VP8L (lossless) sources the returned image is an *image.NRGBA (or
// compatible) from the underlying x/image/webp decoder. For VP8 (lossy)
// sources it is wrapped as a *BT601YCbCr (or *BT601NYCbCrA when the
// source has an ALPH chunk) so that img.At(x, y).RGBA() produces the
// colors a spec-compliant VP8 decoder would show; Go's stdlib
// color.YCbCrToRGB uses JFIF full-range math which shifts VP8 pixels
// by several units per channel.
//
// Callers that specifically need the raw *image.YCbCr / *image.NYCbCrA
// (e.g. for zero-copy plane access) can type-assert to *BT601YCbCr
// or *BT601NYCbCrA and read the embedded field.
func Decode(r io.Reader) (image.Image, error) {
    img, err := decoderWebP.Decode(r)
    if err != nil {
        return nil, err
    }
    return wrapBT601(img), nil
}

// wrapBT601 wraps *image.YCbCr / *image.NYCbCrA results in BT.601-
// limited-range types. Other image types (notably the NRGBA that
// VP8L decoding produces) pass through unchanged.
func wrapBT601(img image.Image) image.Image {
    switch m := img.(type) {
    case *image.YCbCr:
        return &BT601YCbCr{YCbCr: m}
    case *image.NYCbCrA:
        return &BT601NYCbCrA{NYCbCrA: m}
    }
    return img
}

// DecodeConfig reads the image configuration from the provided io.Reader without fully decoding the image.
//
// This function is a wrapper around the underlying WebP decode package (golang.org/x/image/webp) and
// provides access to the image's metadata, such as its dimensions and color model.
// It is useful for obtaining image information before performing a full decode.
//
// Parameters:
//   r - The source io.Reader containing the WebP encoded image.
//
// Returns:
//   An image.Config containing the image's dimensions and color model, or an error if the configuration cannot be retrieved
func DecodeConfig(r io.Reader) (image.Config, error) {
    return decoderWebP.DecodeConfig(r)
}

// DecodeIgnoreAlphaFlag reads a WebP image from the provided io.Reader and returns it as an image.Image.
//
// This function fixes x/image/webp rejecting VP8L images with the VP8X alpha flag, expecting an ALPHA chunk.  
// VP8L handles transparency internally, and the WebP spec requires the flag for transparency.
//
// This function is a wrapper around the underlying WebP decode package (golang.org/x/image/webp).
// It supports both lossy and lossless WebP formats, decoding the image accordingly.
//
// Parameters:
//   r - The source io.Reader containing the WebP encoded image.
//
// Returns:
//   The decoded image as image.Image or an error if the decoding fails.
func DecodeIgnoreAlphaFlag(r io.Reader) (image.Image, error) {
    data, err := io.ReadAll(r)
    if err != nil {
        return nil, err
    }

    if len(data) >= 30 && string(data[8:16]) == "WEBPVP8X" {
        for i := 30; i + 8 < len(data); {
            // Detect VP8L chunk, which handles transparency internally.
            // The x/image/webp package misinterprets this, so we clear the alpha flag.
            if string(data[i: i + 4]) == "VP8L" {
                flags := binary.LittleEndian.Uint32(data[20:24])
                flags &^= 0x00000010
                binary.LittleEndian.PutUint32(data[20:24], flags)
                break
            }

            i += 8 + int(binary.LittleEndian.Uint32(data[i + 4: i + 8]))
        }
    }

    img, err := decoderWebP.Decode(bytes.NewReader(data))
    if err != nil {
        return nil, err
    }
    return wrapBT601(img), nil
}
[![Codecov Coverage](https://codecov.io/gh/HugoSmits86/nativewebp/branch/main/graph/badge.svg)](https://codecov.io/gh/HugoSmits86/nativewebp)
[![Go Reference](https://pkg.go.dev/badge/github.com/HugoSmits86/nativewebp.svg)](https://pkg.go.dev/github.com/HugoSmits86/nativewebp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

# Native WebP for Go

This is a native WebP encoder written entirely in Go, with **no dependencies on libwebp** or other external libraries. Designed for performance and efficiency, this encoder generates smaller files than the standard Go PNG encoder and is approximately **50% faster** in execution.

Supported output formats:

- **VP8L (lossless)** — default, used when `Options.Lossy` is false or
  `Options` is nil. Preserves every pixel exactly.
- **VP8 (lossy)** — enabled by `Options.Lossy = true`. Pure-Go VP8
  keyframe encoder supporting I16 (4 modes), B_PRED (10 I4 modes per
  4×4 sub-block), UV8 chroma prediction, DCT + Walsh-Hadamard
  transforms, round-to-nearest quantization, context-adaptive boolean
  arithmetic coding of coefficient tokens, the decoder-side loop
  filter, and per-MB I16/B_PRED arbitration at Method=3.

  Measured RGB-PSNR (spec-correct, limited-range BT.601) on smooth
  content: ~41 dB at Q=90, ~40 dB at Q=75, ~32 dB at Q=50 — visually
  lossless at high Q and comparable to `cwebp` within a few dB. Files
  are roughly 1.5–3× `cwebp`'s size at the same quality setting;
  closing that gap with full rate-distortion optimization and trellis
  quantization is future work.

  **BT.601 range note**: VP8 uses limited-range BT.601 YCbCr
  (luma 16–235) per RFC 6386. Go's stdlib `image/color.YCbCrToRGB`
  uses JFIF full-range BT.601, so when you decode a lossy WebP with
  `golang.org/x/image/webp` and convert to RGB via `.At().RGBA()`,
  colors shift 2–5 units per channel — making Go's naive RGB-PSNR
  look like ~28 dB even on near-perfect output. Real VP8 decoders
  (libwebp, browsers) apply the correct inverse and show the
  encoder's true quality. This is a Go stdlib issue, not an encoder
  bug.
- **Animation (ANIM/ANMF)** — supported for both VP8L and VP8 frames.
  `EncodeAll` respects `Options.Lossy` per-animation.

## Decoding Support

We provide WebP decoding through a wrapper around `golang.org/x/image/webp`, with an additional `DecodeIgnoreAlphaFlag` function to handle VP8X images where the alpha flag causes decoding issues.
## Benchmark

We conducted a quick benchmark to showcase file size reduction and encoding performance. Using an image from Google’s WebP Lossless and Alpha Gallery, we compared the results of our nativewebp encoder with the standard PNG encoder. <br/><br/>
For the PNG encoder, we applied the `png.BestCompression` setting to achieve the most competitive compression outcomes.
<br/><br/>

<table align="center">
  <tr>
    <th></th>
    <th></th>
    <th>PNG encoder</th>
    <th>nativeWebP encoder</th>
    <th>reduction</th>
  </tr>
  <tr>
    <td rowspan="2" height="110px"><p align="center"><img src="https://www.gstatic.com/webp/gallery3/1.png" height="100px"></p></td>
    <td>file size</td>
    <td>120 kb</td>
    <td>96 kb</td>
    <td>20% smaller</td>
  </tr>
  <tr>
    <td>encoding time</td>
    <td>42945049 ns/op</td>
    <td>27716447 ns/op</td>
    <td>35% faster</td>
  </tr>
  <tr>
    <td rowspan="2" height="110px"><p align="center"><img src="https://www.gstatic.com/webp/gallery3/2.png" height="100px"></p></td>
    <td>file size</td>
    <td>46 kb</td>
    <td>36 kb</td>
    <td>22% smaller</td>
  </tr>
  <tr>
    <td>encoding time</td>
    <td>98509399 ns/op</td>
    <td>31461759 ns/op</td>
    <td>68% faster</td>
  </tr>
  <tr>
    <td rowspan="2" height="110px"><p align="center"><img src="https://www.gstatic.com/webp/gallery3/3.png" height="100px"></p></td>
    <td>file size</td>
    <td>236 kb</td>
    <td>194 kb</td>
    <td>18% smaller</td>
  </tr>
  <tr>
    <td>encoding time</td>
    <td>178205535 ns/op</td>
    <td>102454192 ns/op</td>
    <td>43% faster</td>
  </tr>
  <tr>
    <td rowspan="2" height="110px"><p align="center"><img src="https://www.gstatic.com/webp/gallery3/4.png" height="60px"></p></td>
    <td>file size</td>
    <td>53 kb</td>
    <td>41 kb</td>
    <td>23% smaller</td>
  </tr>
  <tr>
    <td>encoding time</td>
    <td>29088555 ns/op</td>
    <td>14959849 ns/op</td>
    <td>49% faster</td>
  </tr>
  <tr>
    <td rowspan="2" height="110px"><p align="center"><img src="https://www.gstatic.com/webp/gallery3/5.png" height="100px"></p></td>
    <td>file size</td>
    <td>139 kb</td>
    <td>123 kb</td>
    <td>12% smaller</td>
  </tr>
  <tr>
    <td>encoding time</td>
    <td>63423995 ns/op</td>
    <td>21717392 ns/op</td>
    <td>66% faster</td>
  </tr>
</table>
<p align="center">
<sub>image source: https://developers.google.com/speed/webp/gallery2</sub>
</p>


## Installation

To install the nativewebp package, use the following command:
```Bash
go get github.com/HugoSmits86/nativewebp
```
## Usage

Here’s a simple example of how to encode an image losslessly (VP8L):
```Go
file, err := os.Create(name)
if err != nil {
  log.Fatalf("Error creating file %s: %v", name, err)
}
defer file.Close()

err = nativewebp.Encode(file, img, nil)
if err != nil {
  log.Fatalf("Error encoding image to WebP: %v", err)
}
```

Or encode with lossy VP8 compression:
```Go
err = nativewebp.Encode(file, img, &nativewebp.Options{
  Lossy:   true,
  Quality: 75, // 0 (smallest) to 100 (best); 75 is a reasonable default
  Method:  2,  // 0 = fastest/I16-DC-only, 1 = I16 with mode search,
               // 2 = B_PRED with 10 I4 modes per sub-block
})
```

Here’s a simple example of how to encode an animation:
```Go
file, err := os.Create(name)
if err != nil {
  log.Fatalf("Error creating file %s: %v", name, err)
}
defer file.Close()

ani := nativewebp.Animation{
  Images: []image.Image{
    frame1,
    frame2,
  },
  Durations: []uint {
    100,
    100,
  },
  Disposals: []uint {
    0,
    0,
  },
  LoopCount: 0,
  BackgroundColor: 0xffffffff,
}

err = nativewebp.EncodeAll(file, &ani, nil)
if err != nil {
  log.Fatalf("Error encoding WebP animation: %v", err)
}
```

Pass `&nativewebp.Options{Lossy: true, Quality: 75}` to `EncodeAll` to
produce an animation whose frames use VP8 (lossy) instead of VP8L.

## Implementation notes and references

The VP8 encoder in `internal/vp8enc/` is a pure-Go implementation built
against the **RFC 6386** specification (*The VP8 Data Format and
Decoding Guide*). The following open-source implementations were
consulted as references for bit-exact roundtrip compatibility; no code
was copied, but table values (which are specification constants) were
transcribed:

- [`golang.org/x/image/vp8`](https://pkg.go.dev/golang.org/x/image/vp8)
  — pure-Go VP8 decoder, used as the round-trip test oracle (BSD-3-Clause).
- [`libwebp`](https://chromium.googlesource.com/webm/libwebp) — the
  reference C encoder/decoder from Google (BSD-3-Clause).
- [`libvpx`](https://chromium.googlesource.com/webm/libvpx) — VP8/VP9
  reference codec (BSD-3-Clause).

Every file in `internal/vp8enc/` includes an RFC 6386 section pointer
and cross-reference to the equivalent path in the x/image/vp8 decoder,
which is what the encoder is verified against in the roundtrip tests.

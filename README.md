[![Tests](https://github.com/KarpelesLab/gowebp/actions/workflows/test.yml/badge.svg)](https://github.com/KarpelesLab/gowebp/actions/workflows/test.yml)
[![Coverage Status](https://coveralls.io/repos/github/KarpelesLab/gowebp/badge.svg?branch=main)](https://coveralls.io/github/KarpelesLab/gowebp?branch=main)
[![Go Reference](https://pkg.go.dev/badge/github.com/KarpelesLab/gowebp.svg)](https://pkg.go.dev/github.com/KarpelesLab/gowebp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

# gowebp

A pure-Go WebP codec. No `libwebp`, no `cgo`, no runtime dependencies
beyond `golang.org/x/image` (which is used for decoding only).

Encodes to either WebP format:

- **VP8L (lossless)** — default. Every pixel preserved exactly.
- **VP8 (lossy)** — opt-in via `Options.Lossy`. Full intra-only VP8
  keyframe encoder with I16 + B_PRED + UV8 intra prediction, DCT +
  Walsh-Hadamard transforms, boolean arithmetic coding, loop filter,
  and reconstruction-based RD mode selection.

Also encodes WebP **animations** (ANIM/ANMF) with either codec, and
preserves **alpha channels** via ALPH chunks (raw or VP8L-compressed,
static or in-animation).

## Installation

```bash
go get github.com/KarpelesLab/gowebp
```

## Usage

### Lossless

```go
import "github.com/KarpelesLab/gowebp"

f, _ := os.Create(name)
defer f.Close()
if err := gowebp.Encode(f, img, nil); err != nil {
    log.Fatal(err)
}
```

### Lossy

```go
err := gowebp.Encode(f, img, &gowebp.Options{
    Lossy:   true,
    Quality: 75, // 0 (smallest) to 100 (best), default 75
    Method:  4,  // 0 (fastest) to 6 (highest quality), default 0
})
```

### Animation

```go
ani := gowebp.Animation{
    Images:          []image.Image{frame1, frame2, frame3},
    Durations:       []uint{100, 100, 100}, // ms per frame
    Disposals:       []uint{0, 0, 0},       // 0=keep, 1=clear
    LoopCount:       0,                     // 0 = infinite
    BackgroundColor: 0xffffffff,            // BGRA
}
// Pass Options{Lossy: true} for VP8 frames; nil for VP8L.
gowebp.EncodeAll(f, &ani, nil)
```

### Decoding

```go
img, err := gowebp.Decode(r)
```

Wraps `golang.org/x/image/webp`. Also provides `DecodeIgnoreAlphaFlag`
for VP8X files where a misset alpha flag trips up the standard
decoder.

## Lossy method tiers

The `Method` field picks between different speed/quality/size
tradeoffs. Higher values spend more CPU per macroblock to produce
smaller or higher-quality output.

| Method | Strategy | Notes |
|---|---|---|
| 0 | I16 with DC_PRED only | fastest baseline |
| 1 | I16 with 4-mode SSE search | +1–3 dB over M0 |
| 2 | B_PRED with 10 I4 modes per sub-block | best on textured content |
| 3 | Per-MB I16/B_PRED arbitration (prediction SSE) | fast, good default |
| 4 | Per-MB arbitration with reconstruction-aware RDO | **recommended** |
| 5 | Dual-path (measure both I16 and B_PRED) | principled reference |
| 6 | Adds trailing-coefficient trellis trim | helps on high-freq content |

Encoding is goroutine-safe: each `Encode`/`EncodeAll` call is
self-contained and has no shared mutable state.

## Benchmarks

Measured on Intel i9-14900K (Linux, Go 1.25) against the 5 natural
photos linked from [Google's WebP Gallery](https://developers.google.com/speed/webp/gallery2).
Baseline is Go's `image/png` with `png.BestCompression`.

Reproduce:

```bash
NATIVEWEBP_FETCH=1 go test -run TestGalleryPSNR -v    # populate testdata/
go test -bench=. -benchtime=5x
```

### File sizes (bytes per frame)

| Image | PNG | Lossless | Lossy Q=50 M=4 | Lossy Q=75 M=3 | Lossy Q=75 M=4 | Lossy Q=90 M=4 |
|---|---:|---:|---:|---:|---:|---:|
| [`1.png`](https://www.gstatic.com/webp/gallery3/1.png) (400×301) | 120 188 | **97 720** | 16 766 | 17 648 | 24 906 | 43 518 |
| [`2.png`](https://www.gstatic.com/webp/gallery3/2.png) (386×395) | 45 659 | **37 018** | 15 050 | 13 808 | 21 888 | 33 632 |
| [`3.png`](https://www.gstatic.com/webp/gallery3/3.png) (800×600) | 236 018 | **194 714** | 80 992 | 80 552 | 95 330 | 130 828 |
| [`4.png`](https://www.gstatic.com/webp/gallery3/4.png) (421×163) | 52 681 | **41 554** | 27 304 | 24 838 | 35 736 | 47 510 |
| [`5.png`](https://www.gstatic.com/webp/gallery3/5.png) (300×300) | 138 919 | **123 932** | 74 202 | 78 300 | 97 588 | 133 548 |

- **Lossless (VP8L)** averages 13–23 % smaller than PNG's best compression.
- **Lossy Q=75 M=3** is 1.8×–3.3× smaller than lossless on the same images.
- **Lossy Q=50 M=4** can reach 7× smaller than PNG while still
  producing visually acceptable output on photos.

### Encode time (ns/op)

| Image | PNG | Lossless | Lossy Q=75 M=3 | Lossy Q=75 M=4 |
|---|---:|---:|---:|---:|
| `1.png` (400×301) | 38 034 | **25 745** | 30 476 | 30 629 |
| `2.png` (386×395) | 82 888 | **29 999** | 34 298 | 49 051 |
| `3.png` (800×600) | 152 804 | **100 415** | 110 645 | 117 259 |
| `4.png` (421×163) | 23 707 | **24 294** | 17 631 | 19 426 |
| `5.png` (300×300) | 54 435 | **20 294** | 27 094 | 27 931 |

All timings in microseconds for brevity: lossless WebP is **≈ 1.5–2.8×
faster than PNG** across the gallery, while producing smaller files.
Lossy encoding is typically within a small constant factor of
lossless speed.

### Quality (spec-correct Y-PSNR, lossy Q=75 M=3)

Y-PSNR measured against VP8 limited-range BT.601 (what a real decoder
shows). Higher is better; >40 dB is visually lossless on photographic
content.

| Image | Y-PSNR |
|---|---:|
| `1.png` | 39.2 dB |
| `2.png` | 42.2 dB |
| `3.png` | 43.5 dB |
| `4.png` | 38.7 dB |
| `5.png` | 36.7 dB |

Threshold floors on these numbers are asserted by `TestGalleryPSNR`
so future encoder changes can't quietly regress quality.

> **BT.601 range note.** VP8 uses limited-range BT.601 YCbCr (luma
> 16–235) per RFC 6386. Go's stdlib `image/color.YCbCrToRGB` uses
> JFIF full-range BT.601, so naively computing RGB-PSNR after
> `x/image/webp.Decode` shifts 2–5 units per channel and makes the
> output look ~28 dB even when it's actually near-lossless. Real VP8
> decoders (libwebp, browsers) apply the correct inverse.

## Implementation notes

The lossy VP8 encoder lives in `internal/vp8enc/` and is a pure-Go
implementation of **RFC 6386** (*The VP8 Data Format and Decoding
Guide*). The following open-source implementations were consulted as
references for bit-exact compatibility; no code was copied, but table
values (which are specification constants) were transcribed:

- [`golang.org/x/image/vp8`](https://pkg.go.dev/golang.org/x/image/vp8)
  — pure-Go VP8 decoder, used as the round-trip test oracle (BSD-3-Clause).
- [`libwebp`](https://chromium.googlesource.com/webm/libwebp) — the
  reference C encoder/decoder from Google (BSD-3-Clause).
- [`libvpx`](https://chromium.googlesource.com/webm/libvpx) — VP8/VP9
  reference codec (BSD-3-Clause).

Every file in `internal/vp8enc/` references the relevant RFC 6386
section and the matching path in the x/image/vp8 decoder, which is
the oracle used by the test suite's round-trip integration tests.

Remaining work (tracked in the source, not here): full rate-distortion
optimization with calibrated λ; proper Viterbi trellis quantization
with context-aware token-tree rate estimation.

## License

MIT, same as the upstream project this was forked from
(`HugoSmits86/nativewebp`).

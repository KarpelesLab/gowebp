# Test data

Real-world images used by `TestGalleryPSNR` (in `writer_lossy_test.go`).
Not committed to the repo — fetched on demand.

## Populate

```bash
cd testdata
for i in 1 2 3 4 5; do
  curl -O "https://www.gstatic.com/webp/gallery3/$i.png"
done
```

Or set `NATIVEWEBP_FETCH=1` and the test will download them
automatically (writes to this directory):

```bash
NATIVEWEBP_FETCH=1 go test -run TestGalleryPSNR -v
```

## Source

The 5 images are from Google's WebP Gallery, used throughout the
README's benchmark table:

- <https://www.gstatic.com/webp/gallery3/>

They exercise the encoder on natural photographic content with
realistic texture, gradients, and color variation — more
representative than synthetic gradients / checkerboards / solid
colors that most unit tests use.

## Git

`testdata/*.png` are `.gitignore`d. This directory's `README.md`
is committed so contributors know what to put here.

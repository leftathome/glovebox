# OCR enricher testdata

Tiny PNG fixtures consumed by the build-tagged tests in
`../ocr_test.go` (`//go:build enrichruntime`). Total size is well
under 5 KB so the fixtures travel cheaply with the repo.

| File | What it is | Used by |
|---|---|---|
| `hello.png` | 428x150 white background, the word **HELLO** rendered in large block letters (60x90 px each, 12-px stroke). Hand-drawn from `image/png` stdlib, no font dependency. | `TestEnrich_ImageWithEnglishText` |
| `blank.png` | 400x200 solid white. | `TestEnrich_ImageNoText` |
| `tiny.png` | 10x10 solid white — below tesseract's effective minimum. | `TestEnrich_ImageTooSmall` |
| `corrupt.png` | The valid PNG magic header followed by garbage bytes; not a decodable image. | `TestEnrich_CorruptImage` |

## Regenerating

The fixtures are committed binaries. Regenerate by running a
short stdlib-only Go program that uses `image`, `image/color`, and
`image/png` to draw the rectangles. Block letters are constructed
from filled rectangles per glyph (left/right verticals plus
horizontal bars); see commit history for the generator source if
you need to add new letters.

No `golang.org/x/image` dependency is required — the generator
intentionally uses only stdlib so the test surface stays lean.

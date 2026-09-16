# go-mosaic

Builds a **photo mosaic**: a target image reproduced with the photos of a folder
as its tiles. With `-watch` the folder stays under observation and the mosaic is
rebuilt on every change, while the built-in web page updates itself as soon as a
new version is ready.

## Install

Grab a binary for your platform from the
[latest release](https://github.com/dedo1911/go-mosaic/releases/latest), or
build it yourself:

```bash
go install github.com/dedo1911/go-mosaic@latest
```

From a clone:

```bash
go build -o go-mosaic .
```

Requires Go 1.26 or newer (the floor comes from the `golang.org/x/...` modules).

## Quick start

```bash
# one-shot: 80x60 tiles, 4K output
./go-mosaic -src ./photos -target portrait.jpg -grid 80x60 -size 3840x2160 -out mosaic.jpg
```

```bash
# rebuild on every change + web page on http://localhost:8080
./go-mosaic -src ./photos -target portrait.jpg -grid 60x40 -watch
```

## How it works

1. **Tile preparation.** Every image in the source folder is resized to the cell
   size keeping its aspect ratio: it is scaled until the **shorter side** reaches
   the required measure, then the excess is cropped from the centre (left/right
   or top/bottom). Meanwhile the target is brought to the output resolution, in
   parallel horizontal bands.
2. **Colour signature.** Each tile gets a 3×3 grid of average colours in
   **CIE L\*a\*b\***, so matching follows human perception and takes into account
   *where* the colours sit inside the photo.
3. **Matching.** For every cell of the target the closest tiles are collected.
   Under a reuse limit the assignment is global — the most similar cell/photo
   pairs are placed first — instead of row by row, so the first cells do not grab
   all the best photos.
4. **Composition.** Tiles are pasted onto the canvas and, when `-blend` is above
   zero, blended towards the target to make the subject more readable.

When the grid holds far more cells than the library can cover, step 3 only ranks
the cells that stand a chance of getting a photo: sorting thirty million
cell/photo pairs to fill 220 slots is wasted work. Together with the parallel
target scaling, a 1000×1000 build went from 6.0 s to 0.8 s, placing exactly the
same photos.

### Grid and resolution

`-grid` sets **how many tiles** there are horizontally and vertically, `-size`
the **final resolution in pixels**. They are independent:

| Command | Result |
| --- | --- |
| `-grid 80x60 -size 3840x2160` | 4800 tiles of 48×36 px |
| `-grid 80x60` | 4800 tiles of 64×64 px (`-tile`), output 5120×3840 |
| `-grid 80 -size 1920x1080` | rows derived from the aspect ratio (45), tiles of 24×24 px |
| `-grid 80` | rows derived from the target, tiles of `-tile` px |
| `-grid 80 -aspect 16:9` | rows derived from 16:9 (45), square tiles, output 5120×2880 |
| `-grid 80x60 -aspect 16:9` | grid kept, so the tiles turn 4:3 (64×48 px) |
| `-grid 40 -size 1600 -aspect 16:9` | the missing side follows the ratio: 1600×900 |

**Tiles need not be square.** `-aspect` states the shape of the output without
computing pixels; when the grid does not have the same proportions, it is the
tile that stretches. So `-grid 80x60 -aspect 16:9` keeps your 80×60 grid and
makes every tile 4:3 — each source photo is cropped to that shape, centred, with
the same cover rule as everywhere else. `-aspect` together with a full `-size`
is refused, since the two would contradict each other.

Leftover pixels are distributed across the cells, so the output always has
**exactly** the requested resolution.

Watch out for `-grid` without `-size`: the canvas becomes `grid × -tile` pixels,
so `-grid 1000x1000` alone means 64000×64000 px, that is 15 GiB per buffer and
two of them are needed. Pass `-size` for large grids; the program warns when the
canvas alone goes past 1 GiB.

When the target has a different aspect ratio than the canvas, `-fit` decides what
happens: `cover` (default, crops), `contain` (empty bands) or `stretch`
(distorts).

### When there are not enough photos: the mosaic composes itself

The grid needs `columns × rows` tiles. If the source folder does not hold enough
of them — within the `-max-reuse` limit — the missing cells stay **solid black**:
no trace of the target shows through, not even with a high `-blend`. The image
therefore **composes progressively** as files land in the folder, and every cell
appears only once it has a real photo.

```
48 photos for 300 cells: 252 cells stay black (-max-reuse 1)
build #2: 20×15 tiles on 800×600 px, 48 of 48 photos used (max 1× per photo), 252 cells still black, 20ms → mosaic.jpg
```

This is a statement of fact, not a warning: black cells are a legitimate result.
One single black tile covers all of them — with a million-cell grid, creating one
per empty cell would mean comparing every cell against a million identical tiles.

### Where photos land while cells are still black

`-reveal` decides where photos go while the grid is incomplete:

- **`fit`** (default) puts each photo where it matches best, so the subject shows
  up early: on a white sign on black, the bright photos draw the lettering right
  away.
- **`random`** uncovers cells in a fixed random order, so the subject only emerges
  as the grid fills, evenly across the whole image. The order depends only on the
  grid, so it survives rebuilds and restarts: a cell that has a photo keeps one
  as more photos arrive, it never goes black again.

Once the grid is full the two modes produce the very same mosaic; only the path
there differs. In both, cells that match equally well — every cell of a flat
background, say — are picked in that random order rather than by position, so
photos never pile up from the top-left corner row by row.

With `random` the photos inside the uncovered cells are still rearranged to match
best at every rebuild, so when a photo arrives some of the others swap places:
on a 1296-cell grid holding 520 photos, one new photo moved 75 of them, against
21 with `fit`.

The default is `-max-reuse 1`: **every photo appears exactly once**, so you need
as many photos as there are cells. With `-max-reuse N` a photo may repeat up to N
times; with `-max-reuse 0` reuse is unlimited and the grid always fills up (the
classic mosaic, but photos repeat many times).

### Which photos get picked

With unlimited reuse every cell independently takes its closest match, so a photo
only appears if it wins at least one cell. In a library shot in one place under
one light — where most photos share a palette — a few "representative" photos win
every similar cell and the rest never show up.

Two knobs against that:

- **`-variety`** (0 to 1, default `0.25`) widens a tolerance band around the best
  match: among the candidates inside it — the ones that look equivalent anyway —
  the least used photo wins. It costs nothing in cell coverage.
- **`-use-all`** guarantees every photo appears at least once. Each photo is first
  given its best cell (the same global assignment used by `-max-reuse`, with a
  limit of one), then the rest of the grid is filled normally. On a large grid the
  mosaic looks unchanged and the whole library is in there.

Measured on a 600-cell grid with a 479-photo library, all with unlimited reuse:

| Setting | Distinct photos used |
| --- | --- |
| `-variety 0` | 135 |
| `-variety 0.25` (default) | 173 |
| `-variety 0.5` | 236 |
| `-variety 1` | 325 |
| `-use-all` | 479 |

The more photos you force in, the worse each cell matches its colour, so the
subject gets harder to read. `-use-all` is at its best on large grids, where the
extra placements are a rounding error: on a 1000×1000 grid it puts all 479 photos
in without visibly changing the result.

## Watch mode and the web page

With `-watch` the program observes the source folder (subfolders included) and
the target image. Every addition, change or removal starts a new build, with a
`-debounce` that groups rapid changes together — copying 500 photos at once
triggers a single rebuild.

The web page is rendered **server side** with `html/template`. The browser holds
an **SSE** connection on `/events`; when a new mosaic is ready the server sends an
event, the page asks `/fragment` for the updated HTML — again rendered server
side — and swaps it in. No client-side rendering, no polling.

`/mosaic` — the full-screen preview on a black background — is a server-rendered
page too, hooked to the same SSE stream: on notification it preloads the new
image and swaps it only once fully downloaded, so it never flickers. The mosaic
is scaled to fill the window, up or down, keeping its proportions: black bands on
the short side rather than cropping or distortion.

The page is served on **localhost only** by default. It shows the absolute paths
of your folders and hands out the mosaic to anyone who can reach it, so opening it
to the network has to be deliberate: `-serve 0.0.0.0:8080`. There is no
authentication — only do that on a network you trust.

| Route | Description |
| --- | --- |
| `/` | full panel (SSR) |
| `/fragment` | only the part that changes (SSR) |
| `/mosaic` | full-screen preview, updated over SSE (SSR) |
| `/image?v=N` | the bytes of the latest mosaic, served from memory |
| `/image?v=N&download=1` | the same, as a downloadable attachment |
| `/events` | SSE notification stream |
| `/healthz` | liveness check |

The tile library lives in memory between builds: after the first scan only new or
modified photos are reloaded. With `-cache FILE` the thumbnails are written to
disk and restored on the next start.

## Progress

Long steps report a progress bar, drawn with the `progress` component of
[Bubbles](https://github.com/charmbracelet/bubbles):

```
Processing images ███████████████░░░░░░░░░░░  58% 278/479
Matching tiles    █████████████████████░░░░░  60% 2420000/4000000
```

Both halves of the work are covered: decoding the source photos, and building
the mosaic itself — which on a large grid takes longer than the decoding. The
build reports its phases in turn: `Preparing target`, `Analysing target`,
`Matching tiles`, `Placing photos`, `Composing mosaic`.

A bar appears only after its step has been running for 250 ms, so quick work
never makes the terminal flicker; on a small mosaic nothing shows up at all. The
bar is drawn only when the output is an interactive terminal — redirect to a file
or a pipe and you get the plain log lines, with no control codes — and it is
redrawn at most every 60 ms, because with millions of cells writing to the
terminal would cost more than the work itself.

## Options

| Flag | Default | Description |
| --- | --- | --- |
| `-src` | — | folder holding the images used as tiles (required) |
| `-target` | — | image to reproduce (required) |
| `-out` | `mosaic.jpg` | output file (`.jpg` or `.png`) |
| `-grid` | `64` | grid size in tiles, e.g. `80x60`; width only means the height is derived |
| `-size` | — | final resolution, e.g. `3840x2160`; empty means grid × `-tile` |
| `-aspect` | — | aspect ratio of the output, e.g. `16:9`; tiles turn rectangular when the grid does not match (empty = follow the target) |
| `-tile` | `64` | tile side in pixels, used only without `-size` |
| `-fit` | `cover` | how the target fits the canvas: `cover`, `contain`, `stretch` |
| `-blend` | `0.25` | tile/target blend, from `0` (untouched photos) to `1` (target only) |
| `-max-reuse` | `1` | how many times a photo may repeat (`0` = unlimited); cells without a photo stay black |
| `-allow-adjacent` | `false` | allow the same photo in two neighbouring cells |
| `-variety` | `0.25` | with unlimited reuse, how far from the best match to go to pick a less used photo (0 to 1) |
| `-use-all` | `false` | make sure every photo appears at least once (unlimited reuse only) |
| `-reveal` | `fit` | where photos go while cells are still black: `fit` (where they match best) or `random` (fixed random order) |
| `-candidates` | `32` | candidate tiles evaluated per cell |
| `-recursive` | `true` | also look for images in subfolders |
| `-workers` | CPUs | processing goroutines |
| `-memory` | `512MiB` | memory ceiling for the process; less RAM means slower scans (`0` = no ceiling) |
| `-quality` | `92` | JPEG quality of the output |
| `-cache` | — | thumbnail cache file |
| `-watch` | `false` | watch the source and rebuild on every change |
| `-debounce` | `700ms` | how long to wait after the last change before rebuilding |
| `-serve` | `auto` | web address (`auto` = `localhost:8080` with `-watch`, `off` to disable; `0.0.0.0:8080` to expose it) |

Formats read: JPEG, PNG, GIF, BMP, TIFF, WebP. Files that cannot be decoded are
reported and skipped.

## Memory

Almost all the RAM goes into **decoding the source photos**, not into the mosaic.
A 24 megapixel photo takes about 50 MB while being decoded, and decoding thirty
of them at once means over 4 GB of peak — even though the tile that comes out
weighs 16 KB.

That is why the process has a ceiling, `-memory` (default `512MiB`), built from
two mechanisms:

- a **weighted semaphore**: before opening a file its header is read to estimate
  the cost, and that much memory is reserved, so one 24 MP photo counts as much
  as four 6 MP ones. A plain limit on the number of goroutines would not do,
  because the cost depends on megapixels, not on the number of files;
- the **garbage collector limit** (`debug.SetMemoryLimit`), raised automatically
  as the thumbnail library grows, so a large library does not make the collector
  spin.

On top of that, heavy reductions go through a **box average** before the quality
filter: getting 64 pixels out of a 24 MP photo with CatmullRom alone costs time
and buffers proportional to the source. This halves the per-photo time and
removes the temporary buffers.

Real measurements on 479 photos (up to 6048×4024) with 32 CPUs:

| `-memory` | Peak RSS | Time |
| --- | --- | --- |
| `256MiB` | 347 MiB | 75 s |
| `512MiB` (default) | 525 MiB | 35 s |
| `1GiB` | 1046 MiB | 20 s |
| `2GiB` | 2056 MiB | 16 s |
| `0` (no ceiling) | 4448 MiB | 15 s |

In `-watch` mode the process **gives the memory back to the operating system**
after every build instead of waiting for the runtime scavenger: on the same
library it sits at **30 MB** between rebuilds. The web panel shows the current
heap.

If the first scan feels slow, the answer is not raising `-memory` but using
`-cache`: on the same library the second start restores the 479 thumbnails from
disk, comes up in 73 ms and peaks at just 95 MB.

The rest is negligible in comparison: the canvas is held twice (mosaic plus
scaled target), that is `width × height × 8` bytes — a 4K output takes ~66 MB —
and the thumbnail library costs `tiles × tileW × tileH × 4` bytes (5000 photos
with 48×36 tiles ≈ 35 MB).

## Development

```bash
go test ./...
go test -race ./...
```

Source comments are in Italian; everything the program prints, and this document,
are in English.

## License

[MIT](LICENSE) © Dario Emerson

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"strconv"
)

// T2 directory gravity — server-rendered HMF v1 preview thumbnails.
//
// Every published world gets a small PNG rendered straight from its HMF v1
// tile grid (server/thumbnail.go). The directory entry carries the URL
// (/api/worlds/{id}/thumbnail) and the client <img> loads it lazily, so the
// /worlds page shows a true map preview instead of a placeholder gradient.
//
// Rendering is deterministic: same tiles => same pixels. Tile colors mirror
// the client palette (client/src/world/tiles.ts) so the preview matches what
// the player sees in-world.

// thumbTileColor maps the frozen HMF v1 palette to a flat preview color.
// floor (0) is the implicit base tile and renders as the background.
var thumbTileColor = map[string]color.RGBA{
	"wall":    {R: 0x3b, G: 0x31, B: 0x54, A: 0xff},
	"water":   {R: 0x1d, G: 0x4e, B: 0x6e, A: 0xff},
	"grass":   {R: 0x2f, G: 0x5d, B: 0x3a, A: 0xff},
	"stone":   {R: 0x55, G: 0x52, B: 0x5e, A: 0xff},
	"sand":    {R: 0x8a, G: 0x7a, B: 0x54, A: 0xff},
	"path":    {R: 0x6b, G: 0x5f, B: 0x4e, A: 0xff},
	"wood":    {R: 0x5a, G: 0x3d, B: 0x2b, A: 0xff},
	"lava":    {R: 0x9a, G: 0x33, B: 0x14, A: 0xff},
	"ice":     {R: 0x7f, G: 0xc8, B: 0xd9, A: 0xff},
	"flower":  {R: 0xa8, G: 0x4a, B: 0x8a, A: 0xff},
	"bush":    {R: 0x2e, G: 0x4a, B: 0x2b, A: 0xff},
	"rock":    {R: 0x4a, G: 0x4a, B: 0x52, A: 0xff},
	"tree":    {R: 0x1f, G: 0x3d, B: 0x24, A: 0xff},
	"roof":    {R: 0x3a, G: 0x2a, B: 0x40, A: 0xff},
	"door":    {R: 0x6b, G: 0x45, B: 0x2a, A: 0xff},
	"fence":   {R: 0x7a, G: 0x6e, B: 0x5c, A: 0xff},
	"bridge":  {R: 0x6e, G: 0x54, B: 0x38, A: 0xff},
	"crystal": {R: 0x59, G: 0xd1, B: 0xc8, A: 0xff},
	"dirt":    {R: 0x4a, G: 0x39, B: 0x2c, A: 0xff},
}

// thumbFloor is the background color for implicit floor tiles.
var thumbFloor = color.RGBA{R: 0x24, G: 0x1b, B: 0x33, A: 0xff}

// thumbScale caps the rendered preview at this many pixels on the long edge.
const thumbScale = 160

// renderWorldThumbnail draws the world's tile grid into a PNG, scaled so the
// long edge is at most thumbScale px. Deterministic: same world, same bytes.
func renderWorldThumbnail(w *World) ([]byte, error) {
	long := w.Width
	if w.Height > long {
		long = w.Height
	}
	px := thumbScale / long
	if px < 2 {
		px = 2 // tiny worlds still get readable cells
	}
	dw, dh := w.Width*px, w.Height*px
	img := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// fill floor
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			img.SetRGBA(x, y, thumbFloor)
		}
	}
	// draw non-floor tiles
	for y := 0; y < w.Height; y++ {
		for x := 0; x < w.Width; x++ {
			name := w.TileAt(x, y)
			if name == "floor" || name == "" {
				continue
			}
			c, ok := thumbTileColor[name]
			if !ok {
				continue
			}
			x0, y0 := x*px, y*px
			for dy := 0; dy < px; dy++ {
				for dx := 0; dx < px; dx++ {
					img.SetRGBA(x0+dx, y0+dy, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// handleWorldThumbnail: GET /api/worlds/{id}/thumbnail -> image/png.
func (h *Hub) handleWorldThumbnail(w http.ResponseWriter, r *http.Request, id string) {
	world, err := h.store.LoadWorld(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	meta, err := h.store.worldMeta(id)
	if err != nil || !meta.IsPublished {
		http.NotFound(w, r)
		return
	}
	pngBytes, err := renderWorldThumbnail(world)
	if err != nil {
		log.Printf("thumbnail %s: %v", id, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Length", strconv.Itoa(len(pngBytes)))
	_, _ = w.Write(pngBytes)
}

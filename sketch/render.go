package sketch

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"

	"golang.org/x/image/vector"
)

// The canvas as an image: what the canvas tool shows an agent, the page
// shows without JavaScript, and the writing check looks at. Marks are
// drawn in order on white at 512 pixels a side, with smooth (antialiased)
// edges: a stroke as round-capped lines; a shape (from svg) as its fill
// (golang.org/x/image/vector, a pure-Go antialiasing rasterizer, over the
// shape's bounding box only), then its outline as a stroke's lines.
// Deterministic: the same marks give the same bytes.

const renderSize = 512

// renderPNG draws strokes on a size×size canvas as a PNG.
func renderPNG(strokes []stroke, size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	scale := float64(size) / canvasSize
	for _, s := range strokes {
		if s.Shape != nil {
			drawShape(img, s.Shape, scale)
			continue
		}
		c := palette[s.Color]
		r := float64(s.Width) * scale / 2
		if len(s.Points) == 1 {
			p := s.Points[0]
			capsule(img, c, float64(p[0])*scale, float64(p[1])*scale, float64(p[0])*scale, float64(p[1])*scale, r)
		}
		for i := 1; i < len(s.Points); i++ {
			a, b := s.Points[i-1], s.Points[i]
			capsule(img, c, float64(a[0])*scale, float64(a[1])*scale, float64(b[0])*scale, float64(b[1])*scale, r)
		}
	}
	var buf bytes.Buffer
	(&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img)
	return buf.Bytes()
}

// drawShape paints a shape: its fill, then its outline.
func drawShape(img *image.NRGBA, sh *shape, scale float64) {
	if c, ok := palette[sh.Fill]; ok {
		op := opaque(sh.FillAlpha)
		fill(img, sh.Paths, scale, func(x, y int, a float64) { blend(img, x, y, c, a*op) })
	}
	c, ok := palette[sh.Color]
	if !ok || sh.Width <= 0 {
		return
	}
	if sh.StrokeAlpha > 0 {
		// A translucent outline: its color mixed with white, so its
		// overlapping segments don't darken where they join.
		c = tint(c, sh.StrokeAlpha)
	}
	r := sh.Width * scale / 2
	for _, sp := range sh.Paths {
		pts := sp.Points
		if len(pts) == 1 {
			capsule(img, c, pts[0][0]*scale, pts[0][1]*scale, pts[0][0]*scale, pts[0][1]*scale, r)
		}
		for i := 1; i < len(pts); i++ {
			capsule(img, c, pts[i-1][0]*scale, pts[i-1][1]*scale, pts[i][0]*scale, pts[i][1]*scale, r)
		}
	}
}

// opaque is a shape's alpha (0: opaque).
func opaque(a float64) float64 {
	if a <= 0 || a >= 1 {
		return 1
	}
	return a
}

// tint is c at alpha a over white.
func tint(c color.NRGBA, a float64) color.NRGBA {
	mix := func(v uint8) uint8 { return uint8(math.Round(float64(v)*a + 255*(1-a))) }
	return color.NRGBA{mix(c.R), mix(c.G), mix(c.B), 0xff}
}

// fill rasterizes paths' insides (each subpath closed, nonzero) clipped to
// the canvas, calling paint for each pixel they cover, by how much.
func fill(img *image.NRGBA, paths []subpath, scale float64, paint func(x, y int, a float64)) {
	size := img.Rect.Dx()
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, sp := range paths {
		for _, p := range sp.Points {
			minX, maxX = math.Min(minX, p[0]*scale), math.Max(maxX, p[0]*scale)
			minY, maxY = math.Min(minY, p[1]*scale), math.Max(maxY, p[1]*scale)
		}
	}
	x0, y0 := max(0, int(math.Floor(minX))), max(0, int(math.Floor(minY)))
	x1, y1 := min(size, int(math.Ceil(maxX))), min(size, int(math.Ceil(maxY)))
	if x1 <= x0 || y1 <= y0 {
		return
	}
	// The rasterizer covers the part of the shape's box on the canvas, and
	// clips what's outside it.
	w, h := x1-x0, y1-y0
	z := vector.NewRasterizer(w, h)
	toX := func(v float64) float32 { return float32(v - float64(x0)) }
	toY := func(v float64) float32 { return float32(v - float64(y0)) }
	for _, sp := range paths {
		if len(sp.Points) < 3 {
			continue
		}
		z.MoveTo(toX(sp.Points[0][0]*scale), toY(sp.Points[0][1]*scale))
		for _, p := range sp.Points[1:] {
			z.LineTo(toX(p[0]*scale), toY(p[1]*scale))
		}
		z.ClosePath()
	}
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	for y := range h {
		row := mask.Pix[y*mask.Stride : y*mask.Stride+w]
		for x, a := range row {
			if a > 0 {
				paint(x0+x, y0+y, float64(a)/255)
			}
		}
	}
}

// capsule paints the line from (ax, ay) to (bx, by), r thick on each side
// with round ends, in c: each pixel by how much of it the line covers. A
// capsule is convex, so each row it crosses is one run of pixels.
func capsule(img *image.NRGBA, c color.NRGBA, ax, ay, bx, by, r float64) {
	size := img.Rect.Dx()
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	reach := r + 1
	y0 := max(0, int(math.Floor(min(ay, by)-reach)))
	y1 := min(size-1, int(math.Ceil(max(ay, by)+reach)))
	x0 := max(0, int(math.Floor(min(ax, bx)-reach)))
	x1 := min(size-1, int(math.Ceil(max(ax, bx)+reach)))
	for y := y0; y <= y1; y++ {
		py := float64(y) + 0.5
		lo, hi := x1+1, x0-1
		// The row's run: the pixels within reach of the segment.
		for _, end := range [][2]float64{{ax, ay}, {bx, by}} {
			if h := reach*reach - (py-end[1])*(py-end[1]); h >= 0 {
				w := math.Sqrt(h)
				lo = min(lo, int(math.Floor(end[0]-w)))
				hi = max(hi, int(math.Ceil(end[0]+w)))
			}
		}
		if l2 > 0 {
			// Where the band around the segment's line crosses this row,
			// between the perpendiculars at its ends.
			l := math.Sqrt(l2)
			if s, ok := bandRun(ax, ay, dx, dy, l, py, reach); ok {
				lo = min(lo, int(math.Floor(s[0])))
				hi = max(hi, int(math.Ceil(s[1])))
			}
		}
		lo, hi = max(lo, x0), min(hi, x1)
		for x := lo; x <= hi; x++ {
			px := float64(x) + 0.5
			d := segDist(px, py, ax, ay, dx, dy, l2)
			cover := r + 0.5 - d
			if cover <= 0 {
				continue
			}
			blend(img, x, y, c, min(cover, 1))
		}
	}
}

// bandRun is the x range on row py of the points within reach of the
// segment's line whose projection falls on the segment.
func bandRun(ax, ay, dx, dy, l, py, reach float64) ([2]float64, bool) {
	lo, hi := math.Inf(-1), math.Inf(1)
	clip := func(k, c, lo2, hi2 float64) bool { // lo2 ≤ k·x + c ≤ hi2
		if k == 0 {
			return c >= lo2 && c <= hi2
		}
		a, b := (lo2-c)/k, (hi2-c)/k
		if a > b {
			a, b = b, a
		}
		lo, hi = max(lo, a), min(hi, b)
		return lo <= hi
	}
	// Distance from the line: |(x-ax)·dy − (py-ay)·dx| / l ≤ reach.
	if !clip(dy, -ax*dy-(py-ay)*dx, -reach*l, reach*l) {
		return [2]float64{}, false
	}
	// Projection: 0 ≤ (x-ax)·dx + (py-ay)·dy ≤ l².
	if !clip(dx, -ax*dx+(py-ay)*dy, 0, l*l) {
		return [2]float64{}, false
	}
	if math.IsInf(lo, 0) || math.IsInf(hi, 0) {
		return [2]float64{}, false
	}
	return [2]float64{lo, hi}, true
}

// segDist is the distance from (px, py) to the segment from (ax, ay) along
// (dx, dy).
func segDist(px, py, ax, ay, dx, dy, l2 float64) float64 {
	t := 0.0
	if l2 > 0 {
		t = max(0, min(1, ((px-ax)*dx+(py-ay)*dy)/l2))
	}
	ex, ey := ax+t*dx-px, ay+t*dy-py
	return math.Sqrt(ex*ex + ey*ey)
}

// blend paints c over the pixel at (x, y), alpha of it.
func blend(img *image.NRGBA, x, y int, c color.NRGBA, alpha float64) {
	i := img.PixOffset(x, y)
	p := img.Pix[i : i+3 : i+3]
	for k, v := range []uint8{c.R, c.G, c.B} {
		p[k] = uint8(math.Round(float64(p[k])*(1-alpha) + float64(v)*alpha))
	}
}

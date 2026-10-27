package sketch

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Drawing with SVG: a draw call's svg is a small, documented subset of SVG,
// checked against an allowlist and flattened to shapes (outlines of
// straight segments in canvas units, a fill and an outline). Only the
// shapes are kept: the agent's markup never reaches another player, a page
// or a browser, so nothing in it (a comment, an id) can leak the word, and
// nothing in it can run.
//
// The subset, in the canvas's 100×100 viewBox ([0, 0] at the top left):
//
//	<svg> (optional; its viewBox, if given, "0 0 100 100"), <g>,
//	<path d> (M L H V C S Q T A Z, absolute or relative),
//	<circle cx cy r>, <ellipse cx cy rx ry>, <rect x y width height rx ry>,
//	<line x1 y1 x2 y2>, <polyline points>, <polygon points>;
//	fill and stroke (a palette color or none; other CSS colors snap to the
//	palette, svgcolor.go), stroke-width (0 to svgMaxWidth: wider is
//	narrowed), opacity, fill-opacity, stroke-opacity, and transform
//	(translate, scale, rotate), inherited from <g> as in SVG. Fill is black
//	unless set, and there's no outline unless stroke is set.
//
// Any other element is refused, with what's allowed (<text> with "no
// writing"). Any other attribute is dropped, except those that could
// reach outside the drawing or run something (href, event handlers, a
// url() anywhere), which are refused. So are DOCTYPEs and entities,
// processing instructions (but the XML declaration) and text.
//
// Security: the input is at most svgMaxBytes; encoding/xml never fetches
// anything and expands no entities beyond XML's five (Strict, no Entity
// map, no charset reader: anything but UTF-8 is refused), and a DOCTYPE is
// refused outright, so there's nothing to expand. Elements, nesting,
// numbers, path commands and points after flattening are capped, so
// parsing, flattening and rendering are bounded in time and memory, and
// parsing stops at svgParseTimeout regardless.

const (
	svgMaxBytes    = 16 << 10 // the SVG text of one draw call
	svgMaxElements = 80       // elements in one call, <svg> and <g> included
	svgMaxDepth    = 8        // nesting
	svgMaxNumbers  = 4000     // numbers in all the attributes of one call
	svgMaxCommands = 600      // path commands in one call
	svgMaxPoints   = 6000     // points after flattening, in one call
	svgMaxWidth    = 8        // stroke-width
	svgCoordLimit  = 200      // |coordinate| after transforms: off-canvas parts are clipped, wild ones refused

	maxRoundShapes = 300   // shapes in a round's drawing, at most
	maxShapePoints = 15000 // their points, at most

	svgParseTimeout = 200 * time.Millisecond
)

// svgElements is the elements the subset has, as the errors list them.
const svgElements = "svg, g, path, circle, ellipse, rect, line, polyline and polygon"

// shape is an SVG element flattened: its outlines (subpaths of straight
// segments, in canvas units rounded to 0.1), filled with Fill (if any,
// nonzero) and outlined in Color (if any) Width wide, with round ends.
type shape struct {
	Paths []subpath `json:"d"`
	Fill  string    `json:"f,omitempty"`
	Color string    `json:"c,omitempty"`
	Width float64   `json:"w,omitempty"`
	// The fill's and outline's opacity, below 1 (0: opaque).
	FillAlpha   float64 `json:"fa,omitempty"`
	StrokeAlpha float64 `json:"sa,omitempty"`
}

type subpath struct {
	Points [][2]float64 `json:"p"`
	Closed bool         `json:"z,omitempty"`
}

func (s *shape) points() int {
	n := 0
	for _, sp := range s.Paths {
		n += len(sp.Points)
	}
	return n
}

// svgStyle is what an element inherits.
type svgStyle struct {
	fill, stroke string
	width        float64
	m            affine
	// opacity multiplies down the tree (a group's is applied to each shape
	// in it, not to the group as one layer); fill- and stroke-opacity inherit.
	opacity, fillOp, strokeOp float64
}

type affine [6]float64 // a b c d e f: x' = a·x + c·y + e, y' = b·x + d·y + f

var identity = affine{1, 0, 0, 1, 0, 0}

func (m affine) mul(n affine) affine { // m then n applied first: m·n
	return affine{
		m[0]*n[0] + m[2]*n[1], m[1]*n[0] + m[3]*n[1],
		m[0]*n[2] + m[2]*n[3], m[1]*n[2] + m[3]*n[3],
		m[0]*n[4] + m[2]*n[5] + m[4], m[1]*n[4] + m[3]*n[5] + m[5],
	}
}

func (m affine) apply(x, y float64) [2]float64 {
	return [2]float64{m[0]*x + m[2]*y + m[4], m[1]*x + m[3]*y + m[5]}
}

// scale is how much m stretches a length, on average: for stroke widths.
func (m affine) scale() float64 { return math.Sqrt(math.Abs(m[0]*m[3] - m[1]*m[2])) }

type svgParser struct {
	elements, nums, commands, points int
	snapped                          int      // colors snapped to the palette (svgcolor.go)
	dropped                          []string // attributes outside the subset, ignored (the first few)
	deadline                         time.Time
	shapes                           []shape
}

var errSVGTimeout = errors.New("svg: took too long to read; draw it with fewer, simpler shapes")

// svgDrawing is a draw call's SVG checked and flattened.
type svgDrawing struct {
	Shapes  []shape
	Snapped int      // colors that weren't the palette's, snapped to it
	Dropped []string // attributes outside the subset that were ignored, the first few
}

// parseSVG checks an SVG drawing against the subset and flattens it to shapes.
func parseSVG(src string) (svgDrawing, error) {
	p := &svgParser{deadline: time.Now().Add(svgParseTimeout)}
	s, err := p.parse(src)
	if err != nil {
		return svgDrawing{}, err
	}
	return svgDrawing{Shapes: s, Snapped: p.snapped, Dropped: p.dropped}, nil
}

func (p *svgParser) parse(src string) ([]shape, error) {
	if len(src) > svgMaxBytes {
		return nil, fmt.Errorf("svg: at most %d bytes a call; this is %d: send the rest in another call", svgMaxBytes, len(src))
	}
	if !utf8.ValidString(src) {
		return nil, errors.New("svg: not valid UTF-8")
	}
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, errors.New("svg is required")
	}
	// Bare elements, with no <svg> around them, are taken as the canvas's;
	// a closing </svg> after them with no opening one is dropped (DeepSeek
	// V4.1 Flash sends that in two drawings out of three).
	if !strings.HasPrefix(src, "<svg") && !strings.HasPrefix(src, "<?xml") {
		if !strings.Contains(src, "<svg") {
			src = strings.TrimSpace(strings.TrimSuffix(src, "</svg>"))
		}
		src = `<svg viewBox="0 0 100 100">` + src + `</svg>`
	}
	dec := xml.NewDecoder(strings.NewReader(src))
	dec.Strict = true
	dec.Entity = nil        // XML's own five only: no other entity expands
	dec.CharsetReader = nil // UTF-8 only
	type frame struct {
		name string
		st   svgStyle
	}
	var stack []frame
	root := false
	tokens := 0
	for {
		if tokens++; tokens%64 == 0 && time.Now().After(p.deadline) {
			return nil, errSVGTimeout
		}
		tok, err := dec.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("svg: %v", err)
		}
		switch t := tok.(type) {
		case xml.ProcInst:
			// The XML declaration, and only at the very start.
			if t.Target != "xml" || tokens != 1 {
				return nil, errors.New("svg: no processing instructions")
			}
		case xml.Directive:
			return nil, errors.New("svg: no DOCTYPE, entities or other directives")
		case xml.Comment:
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				return nil, errors.New("svg: no text: only elements (writing isn't allowed anyway)")
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != svgName(t.Name) {
				return nil, errors.New("svg: unbalanced elements")
			}
			stack = stack[:len(stack)-1]
		case xml.StartElement:
			if p.elements++; p.elements > svgMaxElements {
				return nil, fmt.Errorf("svg: at most %d elements a call; send the rest in another call", svgMaxElements)
			}
			if len(stack) >= svgMaxDepth {
				return nil, fmt.Errorf("svg: elements nest at most %d deep", svgMaxDepth)
			}
			name := svgName(t.Name)
			parent := svgStyle{fill: "black", width: 1, m: identity, opacity: 1, fillOp: 1, strokeOp: 1}
			if len(stack) > 0 {
				parent = stack[len(stack)-1].st
			}
			switch {
			case len(stack) == 0:
				if name != "svg" || root {
					return nil, errors.New("svg: one <svg> element around the drawing")
				}
				root = true
			case name == "svg":
				return nil, errors.New("svg: no <svg> inside <svg>")
			case stack[len(stack)-1].name != "svg" && stack[len(stack)-1].name != "g":
				return nil, fmt.Errorf("svg: <%s> can't hold elements: only svg and g can", stack[len(stack)-1].name)
			}
			st, err := p.element(name, t.Attr, parent)
			if err != nil {
				return nil, err
			}
			stack = append(stack, frame{name, st})
		}
	}
	if !root || len(stack) != 0 {
		return nil, errors.New("svg: one <svg> element around the drawing")
	}
	if len(p.shapes) == 0 {
		return nil, errors.New("svg: nothing drawn: add shapes (path, circle, ellipse, rect, line, polyline, polygon)")
	}
	return p.shapes, nil
}

// svgName is an element's name as the errors give it: "svg:rect" for a
// prefixed one (RawToken doesn't resolve prefixes), which isn't in the subset.
func svgName(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

// The attributes each element takes, beyond the shared ones.
var svgGeometry = map[string][]string{
	"svg":      {"viewBox"},
	"g":        nil,
	"path":     {"d"},
	"circle":   {"cx", "cy", "r"},
	"ellipse":  {"cx", "cy", "rx", "ry"},
	"rect":     {"x", "y", "width", "height", "rx", "ry"},
	"line":     {"x1", "y1", "x2", "y2"},
	"polyline": {"points"},
	"polygon":  {"points"},
}

var svgShared = []string{"fill", "stroke", "stroke-width", "transform", "opacity", "fill-opacity", "stroke-opacity"}

// refused is why an attribute the subset doesn't have is refused rather
// than dropped: one that could reach outside the drawing or run something.
func refused(name string, a xml.Attr) error {
	k := strings.ToLower(a.Name.Local)
	v := strings.ToLower(a.Value)
	switch {
	case k == "href" || k == "src":
		return fmt.Errorf("svg: <%s> can't have %s: drawings can't refer to anything", name, k)
	case strings.HasPrefix(k, "on"):
		return fmt.Errorf("svg: <%s> can't have %s: drawings can't run anything", name, k)
	case strings.Contains(v, "url(") || strings.Contains(v, "javascript:") || strings.Contains(v, "expression("):
		return fmt.Errorf("svg: <%s> %s: no url() or other references: gradients, patterns, images and the like aren't supported", name, svgName(a.Name))
	}
	return nil
}

// element checks an element and its attributes and, for a shape, flattens it.
func (p *svgParser) element(name string, attrs []xml.Attr, parent svgStyle) (svgStyle, error) {
	geo, ok := svgGeometry[name]
	if !ok {
		if name == "text" || name == "tspan" || name == "textPath" {
			return parent, errors.New("svg: no <text>: drawings can't have writing in them")
		}
		return parent, fmt.Errorf("svg: no <%s>: the elements are %s (no text, images, gradients, patterns, filters, masks, clip paths, use, defs, style or script)", name, svgElements)
	}
	st := parent
	vals := map[string]string{}
	for _, a := range attrs {
		if err := refused(name, a); err != nil {
			return parent, err
		}
		k := a.Name.Local
		if a.Name.Space != "" || (!slices.Contains(svgShared, k) && !slices.Contains(geo, k)) {
			// Not in the subset (id, class, style, stroke-linecap…): dropped,
			// and noted for the drawer (namespace declarations aren't).
			if n := svgName(a.Name); a.Name.Space != "xmlns" && n != "xmlns" && len(p.dropped) < 12 && !slices.Contains(p.dropped, n) {
				p.dropped = append(p.dropped, clipN(n, 32))
			}
			continue
		}
		if _, dup := vals[k]; dup {
			return parent, fmt.Errorf("svg: <%s> has %s twice", name, k)
		}
		vals[k] = a.Value
	}
	if v, ok := vals["viewBox"]; ok {
		n, err := p.numbers(v, 4)
		if err != nil || n[0] != 0 || n[1] != 0 || n[2] != 100 || n[3] != 100 {
			return parent, errors.New(`svg: the viewBox is "0 0 100 100"`)
		}
	}
	for _, k := range []string{"fill", "stroke"} {
		if v, ok := vals[k]; ok {
			c, snapped, err := svgColor(v)
			if err != nil {
				return parent, fmt.Errorf("svg: <%s> %s: %v", name, k, err)
			}
			if snapped {
				p.snapped++
			}
			if k == "fill" {
				st.fill = c
			} else {
				st.stroke = c
			}
		}
	}
	if v, ok := vals["stroke-width"]; ok {
		w, err := p.number(strings.TrimSuffix(strings.TrimSpace(v), "px"))
		if err != nil || w < 0 {
			return parent, fmt.Errorf("svg: <%s> stroke-width is a number, 0 to %d", name, svgMaxWidth)
		}
		st.width = min(w, svgMaxWidth)
	}
	for _, k := range []string{"opacity", "fill-opacity", "stroke-opacity"} {
		if v, ok := vals[k]; ok {
			s := strings.TrimSpace(v)
			a, err := p.number(strings.TrimSuffix(s, "%"))
			if err != nil {
				return parent, fmt.Errorf("svg: <%s> %s is 0 to 1", name, k)
			}
			if strings.HasSuffix(s, "%") {
				a /= 100
			}
			a = max(0, min(1, a)) // as CSS clamps it
			switch k {
			case "opacity":
				st.opacity *= a
			case "fill-opacity":
				st.fillOp = a
			default:
				st.strokeOp = a
			}
		}
	}
	if v, ok := vals["transform"]; ok {
		m, err := p.transform(v)
		if err != nil {
			return parent, fmt.Errorf("svg: <%s> transform: %v", name, err)
		}
		st.m = st.m.mul(m)
	}
	if name == "svg" || name == "g" {
		return st, nil
	}
	paths, err := p.geometry(name, vals)
	if errors.Is(err, errSVGTimeout) {
		return parent, err
	}
	if err != nil {
		return parent, fmt.Errorf("svg: <%s>: %v", name, err)
	}
	sh := shape{Fill: st.fill, Color: st.stroke}
	if name == "line" {
		sh.Fill = "" // a line has no inside
	}
	if sh.Color != "" {
		// Narrowed again after transforms: scale(100) would make an
		// 8-wide stroke 800 wide, and each segment's render cover the
		// whole canvas.
		sh.Width = round1(min(st.width*st.m.scale(), svgMaxWidth))
		if sh.Width == 0 {
			sh.Color = ""
		}
	}
	alpha := func(a float64) float64 { // 0: opaque, -1: invisible
		a = math.Round(a*st.opacity*100) / 100
		switch {
		case a >= 1:
			return 0
		case a <= 0:
			return -1
		}
		return a
	}
	if sh.FillAlpha = alpha(st.fillOp); sh.FillAlpha < 0 {
		sh.Fill, sh.FillAlpha = "", 0
	}
	if sh.StrokeAlpha = alpha(st.strokeOp); sh.StrokeAlpha < 0 {
		sh.Color, sh.StrokeAlpha = "", 0
	}
	if sh.Fill == "" {
		sh.FillAlpha = 0
	}
	if sh.Color == "" {
		sh.Width, sh.StrokeAlpha = 0, 0
	}
	if sh.Fill == "" && sh.Color == "" {
		return st, nil // invisible: nothing to draw
	}
	for _, sp := range paths {
		if len(sp.Points) == 0 {
			continue
		}
		out := subpath{Closed: sp.Closed, Points: make([][2]float64, len(sp.Points))}
		for i, pt := range sp.Points {
			q := st.m.apply(pt[0], pt[1])
			if math.IsNaN(q[0]) || math.IsNaN(q[1]) || math.Abs(q[0]) > svgCoordLimit || math.Abs(q[1]) > svgCoordLimit {
				return parent, fmt.Errorf("svg: <%s> is far off the canvas (0 to 100)", name)
			}
			out.Points[i] = [2]float64{round1(q[0]), round1(q[1])}
		}
		sh.Paths = append(sh.Paths, out)
	}
	if len(sh.Paths) > 0 {
		p.shapes = append(p.shapes, sh)
	}
	return st, nil
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (p *svgParser) count(n int) error {
	if p.nums += n; p.nums > svgMaxNumbers {
		return fmt.Errorf("at most %d numbers a call", svgMaxNumbers)
	}
	return nil
}

func (p *svgParser) number(s string) (float64, error) {
	ns, err := p.numbers(s, 1)
	if err != nil {
		return 0, err
	}
	return ns[0], nil
}

// numbers reads want numbers (or any number, if want is 0), separated by
// commas or spaces.
func (p *svgParser) numbers(s string, want int) ([]float64, error) {
	var out []float64
	sc := numScanner{s: s}
	for {
		sc.skipSep()
		if sc.done() {
			break
		}
		v, err := sc.num()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if err := p.count(1); err != nil {
			return nil, err
		}
	}
	if want > 0 && len(out) != want {
		return nil, fmt.Errorf("%d numbers, not %d", want, len(out))
	}
	return out, nil
}

// geometry is a shape element's outline, in its own coordinates.
func (p *svgParser) geometry(name string, v map[string]string) ([]subpath, error) {
	num := func(k string, def float64, need bool) (float64, error) {
		s, ok := v[k]
		if !ok {
			if need {
				return 0, fmt.Errorf("%s is required", k)
			}
			return def, nil
		}
		x, err := p.number(strings.TrimSuffix(strings.TrimSpace(s), "px"))
		if err != nil {
			return 0, fmt.Errorf("%s: %v", k, err)
		}
		if math.Abs(x) > 1000 {
			return 0, fmt.Errorf("%s is out of range", k)
		}
		return x, nil
	}
	var nums [4]float64
	get := func(keys []string, need bool) error {
		for i, k := range keys {
			x, err := num(k, 0, need && i >= 2)
			if err != nil {
				return err
			}
			nums[i] = x
		}
		return nil
	}
	switch name {
	case "circle", "ellipse":
		keys := []string{"cx", "cy", "r"}
		if name == "ellipse" {
			keys = []string{"cx", "cy", "rx", "ry"}
		}
		if err := get(keys, true); err != nil {
			return nil, err
		}
		rx, ry := nums[2], nums[2]
		if name == "ellipse" {
			ry = nums[3]
		}
		if rx <= 0 || ry <= 0 {
			return nil, nil // as in SVG: a zero radius draws nothing
		}
		return p.add([]subpath{ellipse(nums[0], nums[1], rx, ry)})
	case "rect":
		if err := get([]string{"x", "y", "width", "height"}, true); err != nil {
			return nil, err
		}
		x, y, w, h := nums[0], nums[1], nums[2], nums[3]
		if w <= 0 || h <= 0 {
			return nil, nil // as in SVG: a zero size draws nothing
		}
		_, hasRX := v["rx"]
		_, hasRY := v["ry"]
		rx, err := num("rx", 0, false)
		if err != nil {
			return nil, err
		}
		ry, err := num("ry", 0, false)
		if err != nil {
			return nil, err
		}
		if hasRX && !hasRY {
			ry = rx
		} else if hasRY && !hasRX {
			rx = ry
		}
		rx, ry = math.Max(0, math.Min(rx, w/2)), math.Max(0, math.Min(ry, h/2))
		return p.add([]subpath{roundRect(x, y, w, h, rx, ry)})
	case "line":
		if err := get([]string{"x1", "y1", "x2", "y2"}, false); err != nil {
			return nil, err
		}
		return p.add([]subpath{{Points: [][2]float64{{nums[0], nums[1]}, {nums[2], nums[3]}}}})
	case "polyline", "polygon":
		s, ok := v["points"]
		if !ok {
			return nil, errors.New("points is required")
		}
		ns, err := p.numbers(s, 0)
		if err != nil {
			return nil, fmt.Errorf("points: %v", err)
		}
		if len(ns) < 4 || len(ns)%2 != 0 {
			return nil, errors.New("points: at least two x,y pairs")
		}
		sp := subpath{Closed: name == "polygon"}
		for i := 0; i < len(ns); i += 2 {
			sp.Points = append(sp.Points, [2]float64{ns[i], ns[i+1]})
		}
		if sp.Closed {
			sp.Points = append(sp.Points, sp.Points[0])
		}
		return p.add([]subpath{sp})
	case "path":
		d, ok := v["d"]
		if !ok {
			return nil, errors.New("d is required")
		}
		paths, err := p.pathData(d)
		if errors.Is(err, errSVGTimeout) {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("d: %v", err)
		}
		return p.add(paths)
	}
	return nil, nil
}

// add counts paths' points against the call's cap.
func (p *svgParser) add(paths []subpath) ([]subpath, error) {
	for _, sp := range paths {
		p.points += len(sp.Points)
	}
	if p.points > svgMaxPoints {
		return nil, fmt.Errorf("too detailed: at most %d points a call once curves are flattened", svgMaxPoints)
	}
	if time.Now().After(p.deadline) {
		return nil, errSVGTimeout
	}
	return paths, nil
}

const svgTransforms = "translate(x [y]), scale(s [sy]) or rotate(a [cx cy])"

// transform reads a transform list: translate, scale and rotate.
func (p *svgParser) transform(s string) (affine, error) {
	m := identity
	rest := strings.TrimSpace(s)
	for n := 0; rest != ""; n++ {
		if n >= 8 {
			return m, errors.New("at most 8 transforms")
		}
		open := strings.IndexByte(rest, '(')
		close := strings.IndexByte(rest, ')')
		if open < 0 || close < open {
			return m, errors.New("the transforms are " + svgTransforms)
		}
		fn := strings.TrimSpace(rest[:open])
		args, err := p.numbers(rest[open+1:close], 0)
		if err != nil {
			return m, err
		}
		var t affine
		switch {
		case fn == "translate" && (len(args) == 1 || len(args) == 2):
			args = append(args, 0)
			t = affine{1, 0, 0, 1, args[0], args[1]}
		case fn == "scale" && (len(args) == 1 || len(args) == 2):
			if len(args) == 1 {
				args = append(args, args[0])
			}
			if math.Abs(args[0]) > 100 || math.Abs(args[1]) > 100 {
				return m, errors.New("scale is at most 100")
			}
			t = affine{args[0], 0, 0, args[1], 0, 0}
		case fn == "rotate" && (len(args) == 1 || len(args) == 3):
			a := args[0] * math.Pi / 180
			c, sn := math.Cos(a), math.Sin(a)
			t = affine{c, sn, -sn, c, 0, 0}
			if len(args) == 3 {
				t = affine{1, 0, 0, 1, args[1], args[2]}.mul(t).mul(affine{1, 0, 0, 1, -args[1], -args[2]})
			}
		default:
			return m, errors.New("the transforms are " + svgTransforms)
		}
		m = m.mul(t)
		rest = strings.TrimLeft(rest[close+1:], " \t\r\n,")
	}
	return m, nil
}

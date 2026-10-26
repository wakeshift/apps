package sketch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"strings"
	"testing"
	"time"
)

// The SVG subset (svg.go): what it takes, what it refuses, how it's
// flattened and how it renders.

const svgHouse = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <!-- sky and ground -->
  <rect x="-10" y="70" width="120" height="40" fill="green"/>
  <circle cx="82" cy="18" r="9" fill="yellow" stroke="orange" stroke-width="2"/>
  <g fill="brown" stroke="black" stroke-width="1.5" stroke-linecap="round">
    <rect x="25" y="45" width="40" height="30"/>
    <polygon points="20,46 45,22 70,46" fill="red"/>
    <rect x="40" y="58" width="10" height="17" rx="2" fill="white"/>
  </g>
  <path d="M5 20 Q 15 10 25 20 T 45 20" fill="none" stroke="blue" stroke-width="2"/>
  <path d="M10,40 c0,-5 10,-5 10,0 s10,5 10,0 A5 5 0 0 1 10 40z" fill="gray"/>
  <ellipse cx="80" cy="85" rx="10" ry="4" fill="blue" transform="rotate(-10 80 85)"/>
  <line x1="5" y1="95" x2="30" y2="95" stroke="black" stroke-width="3"/>
  <polyline points="60,95 65,90 70,95 75,90" fill="none" stroke="pink" stroke-width="2"/>
  <g transform="translate(5 5) scale(0.5)"><circle cx="20" cy="140" r="6" fill="purple"/></g>
</svg>`

func mustSVG(t *testing.T, src string) []shape {
	t.Helper()
	d, err := parseSVG(src)
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	return d.Shapes
}

// pixel is the color at canvas units (x, y) of a rendered drawing.
func pixel(t *testing.T, b []byte, x, y float64) [3]uint8 {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r, g, bl, _ := img.At(int(x*renderSize/100), int(y*renderSize/100)).RGBA()
	return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)}
}

func rgb(name string) [3]uint8 {
	c := palette[name]
	return [3]uint8{c.R, c.G, c.B}
}

func asMarks(shapes []shape) []stroke {
	out := make([]stroke, len(shapes))
	for i := range shapes {
		out[i] = stroke{Shape: &shapes[i]}
	}
	return out
}

func TestSVGDrawingRenders(t *testing.T) {
	shapes := mustSVG(t, svgHouse)
	if len(shapes) != 11 {
		t.Errorf("%d shapes, want 11", len(shapes))
	}
	// SVG's defaults and inheritance: fill black unless said, stroke from <g>.
	if s := shapes[2]; s.Fill != "brown" || s.Color != "black" || s.Width != 1.5 {
		t.Errorf("the house's wall: %+v", s)
	}
	if s := shapes[5]; s.Fill != "" || s.Color != "blue" {
		t.Errorf("the wave: fill %q, stroke %q", s.Fill, s.Color)
	}
	if s := shapes[6]; s.Fill != "gray" || s.Color != "" || s.Width != 0 {
		t.Errorf("an unstroked shape: %+v", s)
	}
	a := renderPNG(asMarks(shapes), renderSize)
	if b := renderPNG(asMarks(shapes), renderSize); !bytes.Equal(a, b) {
		t.Error("rendering isn't deterministic")
	}
	// The roof is filled red, the wall brown, the sky white, the ground green.
	for _, c := range []struct {
		x, y float64
		want string
	}{{45, 38, "red"}, {30, 52, "brown"}, {10, 60, "white"}, {50, 85, "green"}, {82, 18, "yellow"}} {
		if got := pixel(t, a, c.x, c.y); got != rgb(c.want) {
			t.Errorf("(%v, %v) is %v, want %s", c.x, c.y, got, c.want)
		}
	}
}

// Leniency the models need: bare elements, a stray </svg>, zero-size
// shapes, attributes outside the subset dropped, colors snapped, a wide
// stroke narrowed and opacity clamped.
func TestSVGLeniency(t *testing.T) {
	for _, src := range []string{
		`<circle cx="50" cy="50" r="20" fill="red"/><line x1="0" y1="0" x2="100" y2="100" stroke="black"/>`,
		"<circle cx=\"50\" cy=\"50\" r=\"20\" fill=\"red\"/>\n<line x1=\"0\" y1=\"0\" x2=\"100\" y2=\"100\" stroke=\"black\" stroke-dasharray=\"2 2\"/>\n</svg>\n",
		`<circle cx="50" cy="50" r="20" fill="red"/><circle cx="5" cy="5" r="0"/><rect width="0" height="5"/><ellipse rx="0" ry="4"/><line x1="0" y1="0" x2="100" y2="100" stroke="black"/>`,
		`<?xml version="1.0" encoding="UTF-8"?><svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="512" height="512" version="1.1" viewBox="0 0 100 100">` +
			`<circle id="sun" class="big" style="fill: blue" data-x="1" cx="50" cy="50" r="20" fill="red" stroke-linejoin="round" fill-rule="evenodd" xml:space="preserve"/>` +
			`<line x1="0px" y1="0" x2="100" y2="100" stroke="black"/></svg>`,
	} {
		d, err := parseSVG(src)
		if err != nil || len(d.Shapes) != 2 {
			t.Errorf("%q: %v, %d shapes", src, err, len(d.Shapes))
			continue
		}
		if d.Shapes[0].Fill != "red" {
			t.Errorf("%q: the circle is %q", src, d.Shapes[0].Fill)
		}
	}
	// What was ignored is said, namespace declarations aside.
	d, _ := parseSVG(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><circle id="a" class="b" r="5" xml:space="x"/><rect id="c" width="1" height="1"/></svg>`)
	if fmt.Sprint(d.Dropped) != "[id class xml:space]" {
		t.Errorf("dropped %q", d.Dropped)
	}
	s := mustSVG(t, `<line x1="0" y1="0" x2="10" y2="10" stroke="black" stroke-width="40"/><rect width="5" height="5" opacity="2" fill-opacity="-1" stroke="red" stroke-opacity="150%"/>`)
	if s[0].Width != svgMaxWidth {
		t.Errorf("a 40-wide line is %v wide, want %d", s[0].Width, svgMaxWidth)
	}
	if len(s) != 2 || s[1].Fill != "" || s[1].Color != "red" || s[1].StrokeAlpha != 0 {
		t.Errorf("clamped opacity: %+v", s[1:])
	}
}

// Comments, ids and anything else in the markup never reach the shapes:
// they're all another player ever gets.
func TestSVGMarkupDoesntLeak(t *testing.T) {
	s := mustSVG(t, `<svg><!-- a cow, moo --><g id="cow-group" class="cow" data-word="cow"><ellipse id="cow" cx="50" cy="50" rx="30" ry="15" fill="white" stroke="black"/></g></svg>`)
	b, _ := json.Marshal(asMarks(s))
	if strings.Contains(string(b), "cow") || strings.Contains(string(b), "moo") {
		t.Errorf("the markup leaked into the shapes: %s", b)
	}
	if txt := strokeText(asMarks(s), 1); txt != "#1 shape: fill white, outline black w1" {
		t.Errorf("strokeText %q", txt)
	}
}

func TestSVGRefuses(t *testing.T) {
	big := strings.Repeat(`<circle cx="50" cy="50" r="1"/>`, svgMaxElements+1)
	deep := strings.Repeat("<g>", svgMaxDepth+1) + `<circle cx="5" cy="5" r="1"/>` + strings.Repeat("</g>", svgMaxDepth+1)
	manyCommands := `<path d="M0 0` + strings.Repeat(" L1 1", svgMaxCommands+1) + `"/>`
	manyPoints := `<path d="M0 0` + strings.Repeat(" C 0 100 100 100 100 0 C 100 -100 0 -100 0 0", 140) + `" fill="none" stroke="black"/>`
	manyNumbers := `<polyline points="` + strings.Repeat("1 1 ", svgMaxNumbers/2+1) + `"/>`
	laughs := `<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">` +
		`<!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;"><!ENTITY lol4 "&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;">]>` +
		`<svg><circle r="1" fill="&lol4;"/></svg>`
	for _, c := range []struct{ name, svg, want string }{
		// Writing.
		{"text", `<svg><text x="10" y="10">CAT</text></svg>`, "no <text>: drawings can't have writing"},
		{"tspan", `<svg><tspan>CAT</tspan></svg>`, "no <text>"},
		{"textPath", `<textPath>CAT</textPath>`, "no <text>"},
		{"bare text", `<svg>CAT</svg>`, "no text"},
		{"CDATA", `<svg><![CDATA[alert(1)]]></svg>`, "no text"},
		// Active content and external references.
		{"script", `<svg><script>alert(1)</script></svg>`, "no <script>"},
		{"script in CDATA", `<script><![CDATA[alert(1)]]></script>`, "no <script>"},
		{"prefixed script", `<svg xmlns:s="http://www.w3.org/2000/svg"><s:script/></svg>`, "no <s:script>"},
		{"prefixed svg", `<x:svg xmlns:x="http://www.w3.org/2000/svg"><x:circle r="5"/></x:svg>`, "no <x:svg>"},
		{"prefixed shape", `<svg xmlns:x="http://www.w3.org/2000/svg"><x:circle r="5"/></svg>`, "no <x:circle>"},
		{"style element", `<svg><style>circle{fill:red}</style></svg>`, "no <style>"},
		{"onload", `<svg onload="alert(1)"><circle r="1"/></svg>`, "can't have onload"},
		{"onclick", `<circle r="1" onClick="alert(1)"/>`, "can't have onclick"},
		{"image", `<svg><image href="https://example.com/x.png"/></svg>`, "no <image>"},
		{"use", `<svg><use href="#a"/></svg>`, "no <use>"},
		{"href", `<g href="https://example.com"><circle r="1"/></g>`, "can't have href"},
		{"xlink:href", `<svg xmlns:xlink="http://www.w3.org/1999/xlink"><g xlink:href="#a"><circle r="1"/></g></svg>`, "can't have href"},
		{"a", `<a href="https://example.com"><circle r="1"/></a>`, "no <a>"},
		{"foreignObject", `<svg><foreignObject><div/></foreignObject></svg>`, "no <foreignObject>"},
		{"iframe", `<iframe src="https://example.com"/>`, "no <iframe>"},
		{"src", `<circle r="1" src="https://example.com"/>`, "can't have src"},
		{"animate", `<svg><circle r="5"><animate attributeName="r"/></circle></svg>`, "can't hold elements"},
		{"set", `<set attributeName="fill" to="red"/>`, "no <set>"},
		{"url fill", `<circle cx="1" cy="1" r="1" fill="url(#g)"/>`, "no url()"},
		{"url stroke", `<circle cx="1" cy="1" r="1" stroke="url(https://example.com/x)"/>`, "no url()"},
		{"style with url", `<circle cx="1" cy="1" r="1" style="fill: url(https://example.com/x.svg#g)"/>`, "no url()"},
		{"style with expression", `<circle cx="1" cy="1" r="1" style="width: expression(alert(1))"/>`, "no url()"},
		{"javascript", `<circle cx="1" cy="1" r="1" filter="javascript:alert(1)"/>`, "no url()"},
		// Unsupported features list what's allowed.
		{"defs", `<svg><defs/></svg>`, "the elements are svg, g, path, circle, ellipse, rect, line, polyline and polygon"},
		{"gradient", `<linearGradient id="g"><stop offset="0"/></linearGradient>`, "no <linearGradient>"},
		{"pattern", `<pattern id="p"/>`, "no <pattern>"},
		{"filter", `<svg><filter id="f"/></svg>`, "no <filter>"},
		{"mask", `<mask id="m"/>`, "no <mask>"},
		{"clipPath", `<clipPath id="c"/>`, "no <clipPath>"},
		{"symbol", `<symbol id="s"/>`, "no <symbol>"},
		{"matrix", `<circle r="1" transform="matrix(1 0 0 1 5 5)"/>`, "translate(x [y]), scale(s [sy]) or rotate(a [cx cy])"},
		{"skew", `<circle r="1" transform="skewX(30)"/>`, "the transforms are"},
		// XML that could expand, fetch or confuse.
		{"billion laughs", laughs, "no DOCTYPE"},
		{"external entity", `<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg>&x;</svg>`, "no DOCTYPE"},
		{"external DTD", `<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd"><svg><circle r="1"/></svg>`, "no DOCTYPE"},
		{"undeclared entity", `<svg><circle r="1" fill="&x;"/></svg>`, "svg:"},
		{"processing instruction", `<svg><?php echo 1 ?><circle r="1"/></svg>`, "no processing"},
		{"late XML declaration", `<!-- hi --><?xml version="1.0"?><svg><circle r="1"/></svg>`, "svg:"},
		{"two XML declarations", `<?xml version="1.0"?><?xml version="1.0"?><svg><circle r="1"/></svg>`, "svg:"},
		{"stylesheet PI", `<?xml-stylesheet href="https://example.com/x.css"?><svg><circle r="1"/></svg>`, "svg:"},
		{"latin-1", `<?xml version="1.0" encoding="ISO-8859-1"?><svg><circle r="1"/></svg>`, "svg:"},
		{"invalid utf-8", "<svg>\xff</svg>", "UTF-8"},
		{"unbalanced", `<svg><g></svg>`, "svg:"},
		{"two roots", `<svg><circle r="1"/></svg><svg/>`, "one <svg>"},
		// Colors, numbers and geometry.
		{"unknown color", `<circle cx="1" cy="1" r="1" fill="sparkly"/>`, "no color"},
		{"bad hex", `<circle cx="1" cy="1" r="1" fill="#ff00"/>`, "no color"},
		{"viewBox", `<svg viewBox="0 0 10 10"><circle r="1"/></svg>`, `the viewBox is "0 0 100 100"`},
		{"far off", `<circle cx="5000" cy="5" r="1"/>`, "out of range"},
		{"far off by transform", `<g transform="translate(900 0)"><circle cx="5" cy="5" r="1"/></g>`, "far off"},
		{"huge number", `<circle cx="1e300" cy="5" r="1"/>`, "out of range"},
		{"long number", `<circle cx="0.00000000000000000000000001" cy="5" r="1"/>`, "too long"},
		{"bad path", `<path d="L 5 5"/>`, "starts with M"},
		{"numbers after Z", `<path d="M0 0 L5 5 Z 3 3"/>`, "after Z"},
		{"negative width", `<line x1="0" y1="0" x2="1" y2="1" stroke="black" stroke-width="-1"/>`, "stroke-width"},
		{"no radius", `<circle cx="1" cy="1"/>`, "r is required"},
		{"duplicate", `<circle r="1" r="2"/>`, "svg:"},
		{"nested svg", `<svg><svg/></svg>`, "inside <svg>"},
		{"nothing", `<svg></svg>`, "nothing drawn"},
		{"only invisible", `<circle r="5" fill="none"/>`, "nothing drawn"},
		{"empty", "   ", "svg is required"},
		// Limits.
		{"too big", `<svg>` + strings.Repeat(" ", svgMaxBytes) + `</svg>`, "at most 16384 bytes"},
		{"too many elements", big, "elements a call"},
		{"too deep", deep, "nest"},
		{"too many commands", manyCommands, "path commands"},
		{"too many points", manyPoints, "points"},
		{"too many numbers", manyNumbers, "numbers a call"},
		{"too many transforms", `<circle r="1" transform="` + strings.Repeat("scale(1) ", 9) + `"/>`, "at most 8 transforms"},
		{"huge scale", `<circle r="1" transform="scale(1000)"/>`, "scale is at most 100"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseSVG(c.svg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

// Hostile input is refused at once, however it's built: it never gets
// near the deadline.
func TestSVGHostileInputIsQuick(t *testing.T) {
	for name, src := range map[string]string{
		"a megabyte":           strings.Repeat("<g>", 1<<20/3),
		"deep under the cap":   strings.Repeat("<g>", svgMaxBytes/3-10),
		"laughs in a megabyte": `<!DOCTYPE x [` + strings.Repeat(`<!ENTITY a "&a;&a;">`, 1000) + `]><svg/>`,
		"one long path":        `<path d="M0 0` + strings.Repeat("l0.1 0.1", svgMaxBytes/8) + `"/>`,
		"one long number":      `<circle r="` + strings.Repeat("9", svgMaxBytes-20) + `"/>`,
	} {
		start := time.Now()
		if _, err := parseSVG(src); err == nil {
			t.Errorf("%s: no error", name)
		}
		if d := time.Since(start); d > raceSlowdown*svgParseTimeout/2 {
			t.Errorf("%s: took %v", name, d)
		}
	}
}

// Parsing stops at its deadline.
func TestSVGDeadline(t *testing.T) {
	p := &svgParser{deadline: time.Now().Add(-time.Millisecond)}
	if _, err := p.parse(svgHouse); err != errSVGTimeout {
		t.Errorf("past the deadline: %v", err)
	}
	p = &svgParser{deadline: time.Now().Add(-time.Millisecond)}
	if _, err := p.pathData("M0 0 L 1 1"); err != errSVGTimeout {
		t.Errorf("path data past the deadline: %v", err)
	}
}

// The worst a call can be, at the caps, parses and renders quickly.
func TestSVGWorstCaseIsFast(t *testing.T) {
	var b strings.Builder
	b.WriteString("<svg>")
	for range 45 {
		fmt.Fprintf(&b, `<path d="M0 0 C 0 100 100 100 100 0 C 100 -100 0 -100 0 0 C 0 100 100 100 100 0 C 100 -100 0 -100 0 0Z" fill="red" stroke="black" stroke-width="8"/>`)
	}
	b.WriteString("</svg>")
	if b.Len() > svgMaxBytes {
		t.Fatalf("the test's SVG is %d bytes", b.Len())
	}
	t0 := time.Now()
	shapes := mustSVG(t, b.String())
	parse := time.Since(t0)
	t0 = time.Now()
	renderPNG(asMarks(shapes), renderSize)
	render := time.Since(t0)
	_, _, pts := marks(asMarks(shapes))
	t.Logf("%d shapes, %d points: parsed in %v, rendered in %v", len(shapes), pts, parse, render)
	// Under the race detector (as CI runs it) everything is many times slower.
	if parse > svgParseTimeout || render > raceSlowdown*500*time.Millisecond {
		t.Errorf("too slow: parse %v, render %v", parse, render)
	}
}

// Flattening: each element's outline in canvas units, after transforms.
func TestSVGFlattening(t *testing.T) {
	near := func(a, b [2]float64) bool { return math.Abs(a[0]-b[0]) <= 0.11 && math.Abs(a[1]-b[1]) <= 0.11 }
	first := func(s shape) subpath { return s.Paths[0] }
	last := func(sp subpath) [2]float64 { return sp.Points[len(sp.Points)-1] }

	// A rect is a closed square; rx rounds it inside its box.
	r := first(mustSVG(t, `<rect x="10" y="20" width="30" height="40"/>`)[0])
	if !r.Closed || len(r.Points) != 5 || r.Points[0] != [2]float64{10, 20} || r.Points[2] != [2]float64{40, 60} {
		t.Errorf("rect: %+v", r)
	}
	rr := first(mustSVG(t, `<rect x="10" y="10" width="20" height="20" rx="50"/>`)[0])
	for _, p := range rr.Points {
		if d := math.Hypot(p[0]-20, p[1]-20); d > 10.1 {
			t.Errorf("rx past half the width: %v is %v from the center", p, d)
		}
	}
	// A circle's points are on it, and it closes.
	c := first(mustSVG(t, `<circle cx="50" cy="50" r="20"/>`)[0])
	for _, p := range c.Points {
		if d := math.Hypot(p[0]-50, p[1]-50); math.Abs(d-20) > 0.11 {
			t.Fatalf("a circle's point %v is %v from its center", p, d)
		}
	}
	if !c.Closed || c.Points[0] != last(c) || len(c.Points) < 13 {
		t.Errorf("circle: closed %v, %d points", c.Closed, len(c.Points))
	}
	// Transforms: translate, scale and rotate, nested, scale the outline.
	s := mustSVG(t, `<g transform="translate(10 20)"><g transform="scale(2)"><line x1="0" y1="0" x2="5" y2="0" stroke="black" stroke-width="1.5"/></g></g>`)[0]
	if l := first(s); l.Points[0] != [2]float64{10, 20} || l.Points[1] != [2]float64{20, 20} || s.Width != 3 || s.Fill != "" {
		t.Errorf("translate then scale: %+v", s)
	}
	rot := first(mustSVG(t, `<line x1="60" y1="50" x2="70" y2="50" stroke="black" transform="rotate(90 50 50)"/>`)[0])
	if !near(rot.Points[0], [2]float64{50, 60}) || !near(rot.Points[1], [2]float64{50, 70}) {
		t.Errorf("rotate about a point: %v", rot.Points)
	}
	// Numbers after M are lines, absolute after M and relative after m.
	mm := mustSVG(t, `<path d="M 10 10 20 20 m 5 5 1 1" fill="none" stroke="black"/>`)[0]
	if fmt.Sprint(mm.Paths) != "[{[[10 10] [20 20]] false} {[[25 25] [26 26]] false}]" {
		t.Errorf("implicit lines after M: %v", mm.Paths)
	}
	// S and T reflect only a curve's control point: after a line, the
	// first control point is the current point.
	sl := first(mustSVG(t, `<path d="M0 0 C 0 50 50 50 50 0 L 60 0 S 100 0 100 0" fill="none" stroke="black"/>`)[0])
	for _, pt := range sl.Points[len(sl.Points)-5:] {
		if pt[1] != 0 {
			t.Errorf("S after L reflected an old control point: %v", sl.Points)
			break
		}
	}
	// Relative commands, H, V and Z.
	p := first(mustSVG(t, `<path d="M10 10 h10 v10 l-10 0 z"/>`)[0])
	if want := [][2]float64{{10, 10}, {20, 10}, {20, 20}, {10, 20}, {10, 10}}; fmt.Sprint(p.Points) != fmt.Sprint(want) || !p.Closed {
		t.Errorf("relative path: %v", p.Points)
	}
	// Two subpaths: drawing on after Z starts where the last began.
	two := mustSVG(t, `<path d="M0 0 L10 0 L10 10 Z L 0 10"/>`)[0]
	if len(two.Paths) != 2 || two.Paths[1].Points[0] != [2]float64{0, 0} || two.Paths[1].Closed {
		t.Errorf("after Z: %+v", two.Paths)
	}
	// Curves end where they say, and S reflects C's control point.
	cv := first(mustSVG(t, `<path d="M0 50 C 0 0 50 0 50 50 S 100 100 100 50" fill="none" stroke="black"/>`)[0])
	if last(cv) != [2]float64{100, 50} || len(cv.Points) < 10 {
		t.Errorf("curve: ends at %v, %d points", last(cv), len(cv.Points))
	}
	mid := cv.Points[len(cv.Points)*3/4] // the S half bulges down, as the reflection makes it
	if mid[1] <= 50 {
		t.Errorf("S didn't reflect C: %v", mid)
	}
	q := first(mustSVG(t, `<path d="M0 50 Q 25 0 50 50 T 100 50" fill="none" stroke="black"/>`)[0])
	if last(q) != [2]float64{100, 50} || q.Points[len(q.Points)*3/4][1] <= 50 {
		t.Errorf("T didn't reflect Q: %v", q.Points)
	}
	// An arc: a half circle from (30, 50) to (70, 50) passes (50, 30).
	a := first(mustSVG(t, `<path d="M30 50 A20 20 0 0 1 70 50" fill="none" stroke="black"/>`)[0])
	top := a.Points[0]
	for _, pt := range a.Points {
		if pt[1] < top[1] {
			top = pt
		}
	}
	if last(a) != [2]float64{70, 50} || !near(top, [2]float64{50, 30}) {
		t.Errorf("arc: ends %v, top %v", last(a), top)
	}
	// Polygon closes; polyline doesn't.
	pg := first(mustSVG(t, `<polygon points="0,0 10,0 5,8"/>`)[0])
	pl := first(mustSVG(t, `<polyline points="0,0 10,0 5,8" fill="none" stroke="red"/>`)[0])
	if !pg.Closed || pg.Points[3] != [2]float64{0, 0} || pl.Closed || len(pl.Points) != 3 {
		t.Errorf("polygon %+v, polyline %+v", pg, pl)
	}
	// Coordinates are rounded to tenths.
	if p := first(mustSVG(t, `<line x1="1.234" y1="5.678" x2="9" y2="9" stroke="red"/>`)[0]); p.Points[0] != [2]float64{1.2, 5.7} {
		t.Errorf("rounding: %v", p.Points[0])
	}
}

// Colors outside the palette are snapped to its nearest.
func TestSVGColorsSnap(t *testing.T) {
	for in, want := range map[string]string{"#ff0000": "red", "skyblue": "blue", "darkgreen": "green", "#8B4513": "brown", "rgb(250, 250, 250)": "white",
		"#222": "black", "gold": "yellow", "hotpink": "pink", "silver": "gray", "Red": "red", "grey": "gray", "none": "", "transparent": ""} {
		got, _, err := svgColor(in)
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", in, got, err, want)
		}
	}
	d, err := parseSVG(`<g fill="skyblue"><circle cx="5" cy="5" r="2"/><rect x="1" y="1" width="2" height="2" stroke="#000"/></g>`)
	if err != nil || d.Snapped != 2 || d.Shapes[0].Fill != "blue" || d.Shapes[1].Color != "black" {
		t.Errorf("snapped %d, %v; want 2", d.Snapped, err)
	}
}

func TestSVGOpacity(t *testing.T) {
	s := mustSVG(t, `<g opacity="0.5"><rect x="0" y="0" width="50" height="50" fill="blue" fill-opacity="0.5" stroke="black"/></g><circle cx="5" cy="5" r="2" opacity="0"/>`)
	if len(s) != 1 || s[0].FillAlpha != 0.25 || s[0].StrokeAlpha != 0.5 {
		t.Errorf("%+v", s)
	}
	got := pixel(t, renderPNG(asMarks(s), renderSize), 20, 20)
	if got[0] < 180 || got[1] < 190 || got[2] < 250 {
		t.Errorf("a quarter-opaque blue over white is %v", got)
	}
}

// Strokes and shapes share a drawing, drawn in order.
func TestMixedDrawingRenders(t *testing.T) {
	ms := append([]stroke{{Points: [][2]int{{0, 50}, {100, 50}}, Color: "blue", Width: 6}},
		asMarks(mustSVG(t, `<rect x="40" y="40" width="20" height="20" fill="red"/>`))...)
	ms = append(ms, stroke{Points: [][2]int{{50, 0}, {50, 100}}, Color: "green", Width: 2})
	b := renderPNG(ms, renderSize)
	for _, c := range []struct {
		x, y float64
		want string
	}{{10, 50, "blue"}, {45, 50, "red"}, {50, 45, "green"}, {50, 10, "green"}, {10, 10, "white"}} {
		if got := pixel(t, b, c.x, c.y); got != rgb(c.want) {
			t.Errorf("(%v, %v) is %v, want %s", c.x, c.y, got, c.want)
		}
	}
	if s := marksSaid(ms); s != "2 strokes and 1 shape" {
		t.Errorf("marksSaid %q", s)
	}
	// A shape off the canvas's edge fills what's on it.
	edge := renderPNG(asMarks(mustSVG(t, `<rect x="-50" y="-50" width="80" height="80" fill="purple"/>`)), renderSize)
	if got := pixel(t, edge, 1, 1); got != rgb("purple") {
		t.Errorf("a shape over the edge: %v", got)
	}
	img, _ := png.Decode(bytes.NewReader(edge))
	if img.Bounds() != image.Rect(0, 0, renderSize, renderSize) {
		t.Errorf("bounds %v", img.Bounds())
	}
}

// The draw tool takes svg or strokes, never both, and keeps a round's
// shapes and their points under its caps.
func TestCheckDrawSVG(t *testing.T) {
	if _, err := checkDraw(drawIn{SVG: `<circle r="1"/>`, Strokes: []strokeIn{{Points: [][]float64{{1, 1}}}}}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("both: %v", err)
	}
	if _, err := checkDraw(drawIn{}); err == nil || !strings.Contains(err.Error(), "svg or strokes is required") {
		t.Errorf("neither: %v", err)
	}
	if _, err := checkDraw(drawIn{Strokes: []strokeIn{}}); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Errorf("no strokes: %v", err)
	}
	d, err := checkDraw(drawIn{SVG: `<circle cx="50" cy="50" r="20" fill="skyblue"/><rect width="5" height="5"/>`})
	if err != nil || !d.svg || len(d.marks) != 2 || d.snapped != 1 || d.marks[0].Shape == nil {
		t.Fatalf("%+v %v", d, err)
	}
	full := make([]stroke, maxRoundShapes-1)
	for i := range full {
		full[i] = d.marks[1]
	}
	if err := d.fits(full); err == nil || !strings.Contains(err.Error(), "at most 300 shapes") {
		t.Errorf("past the round's shapes: %v", err)
	}
	if err := d.fits(full[:10]); err != nil {
		t.Errorf("under the caps: %v", err)
	}
	big := mustSVG(t, `<path d="M0 0`+strings.Repeat(" L1 1", 500)+`"/>`)
	var heavy []stroke
	for range maxShapePoints/501 + 1 {
		heavy = append(heavy, asMarks(big)...)
	}
	if err := d.fits(heavy); err == nil || !strings.Contains(err.Error(), "points") {
		t.Errorf("past the round's points: %v", err)
	}
	// Strokes still count against strokes' own cap, not shapes'.
	strokes := make([]stroke, maxRoundStrokes)
	for i := range strokes {
		strokes[i] = line(1, 1, 2, 2)
	}
	if err := d.fits(strokes); err != nil {
		t.Errorf("shapes after a full set of strokes: %v", err)
	}
	if err := (drawCall{marks: []stroke{line(1, 1, 2, 2)}}).fits(strokes); err == nil || !strings.Contains(err.Error(), "at most 200 strokes") {
		t.Errorf("a stroke past the cap: %v", err)
	}
}

// The bake-off's tool takes svg as the draw tool does.
func TestCheckDrawOfflineSVG(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"svg": `<circle cx="50" cy="50" r="20" fill="yellow"/>`, "note": "a sun"})
	got, err := CheckDraw(args, nil)
	if err != nil || !strings.HasPrefix(string(got), `[{"sh":{"d":[{"p":[[70,50],`) {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := CheckDraw([]byte(`{"svg": "<circle r='1'/>", "plan": "x"}`), nil); err == nil {
		t.Error("an unknown argument passed")
	}
	png, err := RenderPNG(got)
	if err != nil || pixel(t, png, 50, 50) != rgb("yellow") {
		t.Errorf("render: %v", err)
	}
	if _, err := RenderPNG([]byte(`[{"sh":{"d":[{"p":[[0,0],[5,5],[0,5]]}],"f":"teal"}}]`)); err == nil {
		t.Error("a shape with a color not in the palette rendered")
	}
}

// Fuzzing (go test -fuzz FuzzParseSVG): whatever comes in, parsing doesn't
// panic, and what it accepts is within the caps and on (or near) the canvas.
func FuzzParseSVG(f *testing.F) {
	for _, s := range []string{svgHouse, `<circle r="1"/>`, `<path d="M0 0 A5 5 0 1 1 10 10 Q 1 2 3 4 T 5 6 S 1 2 3 4 Z"/>`, `<g transform="rotate(30 5 5) scale(2)"><rect width="5" height="5" rx="1"/></g>`,
		`<!DOCTYPE x><svg/>`, `<svg><text>hi</text></svg>`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		start := time.Now()
		d, err := parseSVG(src)
		if took := time.Since(start); took > raceSlowdown*2*svgParseTimeout {
			t.Fatalf("took %v", took)
		}
		if err != nil {
			return
		}
		pts := 0
		for _, s := range d.Shapes {
			pts += s.points()
			if s.Fill == "" && s.Color == "" {
				t.Fatalf("an invisible shape: %+v", s)
			}
			for _, c := range []string{s.Fill, s.Color} {
				if _, ok := palette[c]; c != "" && !ok {
					t.Fatalf("a color not in the palette: %q", c)
				}
			}
			for _, sp := range s.Paths {
				for _, p := range sp.Points {
					if math.Abs(p[0]) > svgCoordLimit || math.Abs(p[1]) > svgCoordLimit {
						t.Fatalf("a point off the canvas: %v", p)
					}
				}
			}
		}
		if len(d.Shapes) > svgMaxElements || pts > svgMaxPoints {
			t.Fatalf("%d shapes, %d points", len(d.Shapes), pts)
		}
	})
}

// Previews are limited per player: one every previewGap, previewsPerRound a round.
func TestPreviewLimits(t *testing.T) {
	var l previewLimits
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := l.allow("p", "g", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := l.allow("p", "g", 1, now.Add(time.Second)); err == nil {
		t.Error("two previews a second apart")
	}
	if err := l.allow("q", "g", 1, now.Add(time.Second)); err != nil {
		t.Errorf("another player: %v", err)
	}
	for i := 2; i <= previewsPerRound; i++ {
		now = now.Add(previewGap)
		if err := l.allow("p", "g", 1, now); err != nil {
			t.Fatalf("preview %d: %v", i, err)
		}
	}
	now = now.Add(previewGap)
	if err := l.allow("p", "g", 1, now); err == nil || !strings.Contains(err.Error(), "previews this round") {
		t.Errorf("past the round's previews: %v", err)
	}
	if err := l.allow("p", "g", 2, now); err != nil {
		t.Errorf("the next round: %v", err)
	}
}

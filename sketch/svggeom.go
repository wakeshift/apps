package sketch

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The SVG subset's geometry (svg.go reads the elements): numbers, path
// data, and curves, arcs, ellipses and rounded corners flattened to
// straight segments.

// segments is how many straight pieces a curve about length long becomes:
// about one every 1.5 canvas units, 2 to 32.
func segments(length float64) int {
	return max(2, min(32, int(math.Ceil(length/1.5))))
}

func ellipse(cx, cy, rx, ry float64) subpath {
	n := max(12, min(64, int(math.Ceil(2*math.Pi*math.Max(rx, ry)/1.5))))
	sp := subpath{Closed: true}
	for i := 0; i <= n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		sp.Points = append(sp.Points, [2]float64{cx + rx*math.Cos(a), cy + ry*math.Sin(a)})
	}
	return sp
}

func roundRect(x, y, w, h, rx, ry float64) subpath {
	sp := subpath{Closed: true}
	if rx == 0 || ry == 0 {
		sp.Points = [][2]float64{{x, y}, {x + w, y}, {x + w, y + h}, {x, y + h}, {x, y}}
		return sp
	}
	n := max(3, min(12, int(math.Ceil(math.Pi/2*math.Max(rx, ry)/1.5))))
	corner := func(cx, cy, from float64) {
		for i := 0; i <= n; i++ {
			a := from + math.Pi/2*float64(i)/float64(n)
			sp.Points = append(sp.Points, [2]float64{cx + rx*math.Cos(a), cy + ry*math.Sin(a)})
		}
	}
	corner(x+w-rx, y+ry, -math.Pi/2)
	corner(x+w-rx, y+h-ry, 0)
	corner(x+rx, y+h-ry, math.Pi/2)
	corner(x+rx, y+ry, math.Pi)
	sp.Points = append(sp.Points, sp.Points[0])
	return sp
}

// numScanner reads SVG numbers and path data.
type numScanner struct {
	s string
	i int
}

func (sc *numScanner) done() bool { return sc.i >= len(sc.s) }

func (sc *numScanner) skipSep() {
	for sc.i < len(sc.s) && strings.IndexByte(" \t\r\n,", sc.s[sc.i]) >= 0 {
		sc.i++
	}
}

func (sc *numScanner) num() (float64, error) {
	start := sc.i
	if sc.i < len(sc.s) && (sc.s[sc.i] == '+' || sc.s[sc.i] == '-') {
		sc.i++
	}
	digits, dot := 0, false
	for sc.i < len(sc.s) {
		c := sc.s[sc.i]
		if c >= '0' && c <= '9' {
			digits++
		} else if c == '.' && !dot {
			dot = true
		} else {
			break
		}
		sc.i++
	}
	if digits == 0 {
		sc.i = start
		return 0, fmt.Errorf("a number expected at %q", clip(sc.s[start:]))
	}
	if sc.i < len(sc.s) && (sc.s[sc.i] == 'e' || sc.s[sc.i] == 'E') {
		j := sc.i + 1
		if j < len(sc.s) && (sc.s[j] == '+' || sc.s[j] == '-') {
			j++
		}
		k := j
		for k < len(sc.s) && sc.s[k] >= '0' && sc.s[k] <= '9' {
			k++
		}
		if k > j {
			sc.i = k
		}
	}
	if sc.i-start > 24 {
		return 0, errors.New("a number too long")
	}
	v, err := strconv.ParseFloat(sc.s[start:sc.i], 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) || math.Abs(v) > 1e4 {
		return 0, fmt.Errorf("a number out of range at %q", clip(sc.s[start:]))
	}
	return v, nil
}

// flag reads an arc's flag: 0 or 1, maybe with no separator after it.
func (sc *numScanner) flag() (bool, error) {
	sc.skipSep()
	if sc.i < len(sc.s) && (sc.s[sc.i] == '0' || sc.s[sc.i] == '1') {
		sc.i++
		return sc.s[sc.i-1] == '1', nil
	}
	return false, errors.New("an arc flag (0 or 1) expected")
}

// clip is the start of s, for an error: a few characters.
func clip(s string) string { return clipN(s, 12) }

// clipN is the start of s, at most n bytes, cut at a rune.
func clipN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// pathData flattens a path's d to subpaths.
func (p *svgParser) pathData(d string) ([]subpath, error) {
	sc := numScanner{s: d}
	var out []subpath
	var cur *subpath
	var x, y, sx, sy float64 // current point, subpath start
	var lcx, lcy float64     // the last control point, for S and T
	var last byte            // the last command
	pts := 0                 // points so far
	var fromX, fromY float64 // where the command being read starts
	add := func(px, py float64) {
		if cur == nil {
			// Drawing on after Z starts a new subpath where the last began.
			out = append(out, subpath{Points: [][2]float64{{fromX, fromY}}})
			cur = &out[len(out)-1]
			pts++
		}
		cur.Points = append(cur.Points, [2]float64{px, py})
		pts++
	}
	nums := func(n int) ([]float64, error) {
		v := make([]float64, n)
		for i := range v {
			sc.skipSep()
			f, err := sc.num()
			if err != nil {
				return nil, err
			}
			v[i] = f
		}
		return v, p.count(n)
	}
	var cmd byte
	for {
		sc.skipSep()
		if sc.done() {
			break
		}
		if time.Now().After(p.deadline) {
			return nil, errSVGTimeout
		}
		c := sc.s[sc.i]
		switch {
		case strings.IndexByte("MmLlHhVvCcSsQqTtAaZz", c) >= 0:
			cmd = c
			sc.i++
		case cmd == 0:
			return nil, fmt.Errorf("starts with %q: a path starts with M", clip(sc.s[sc.i:]))
		case cmd == 'M':
			cmd = 'L' // numbers after a moveto are linetos
		case cmd == 'm':
			cmd = 'l'
		case cmd == 'Z' || cmd == 'z':
			return nil, fmt.Errorf("numbers after Z at %q", clip(sc.s[sc.i:]))
		}
		if p.commands++; p.commands > svgMaxCommands {
			return nil, fmt.Errorf("at most %d path commands a call", svgMaxCommands)
		}
		fromX, fromY = x, y
		rel := cmd >= 'a'
		ox, oy := 0.0, 0.0
		if rel {
			ox, oy = x, y
		}
		upper := cmd &^ 0x20
		if last == 0 && upper != 'M' {
			return nil, errors.New("a path starts with M")
		}
		switch upper {
		case 'M':
			v, err := nums(2)
			if err != nil {
				return nil, err
			}
			x, y = ox+v[0], oy+v[1]
			sx, sy = x, y
			out = append(out, subpath{Points: [][2]float64{{x, y}}})
			cur = &out[len(out)-1]
			pts++
		case 'L':
			v, err := nums(2)
			if err != nil {
				return nil, err
			}
			x, y = ox+v[0], oy+v[1]
			add(x, y)
		case 'H':
			v, err := nums(1)
			if err != nil {
				return nil, err
			}
			x = ox + v[0]
			add(x, y)
		case 'V':
			v, err := nums(1)
			if err != nil {
				return nil, err
			}
			y = oy + v[0]
			add(x, y)
		case 'C', 'S':
			var c1x, c1y float64
			var v []float64
			var err error
			if upper == 'C' {
				if v, err = nums(6); err != nil {
					return nil, err
				}
				c1x, c1y = ox+v[0], oy+v[1]
				v = v[2:]
			} else {
				if v, err = nums(4); err != nil {
					return nil, err
				}
				c1x, c1y = x, y
				if l := last &^ 0x20; l == 'C' || l == 'S' {
					c1x, c1y = 2*x-lcx, 2*y-lcy
				}
			}
			c2x, c2y, ex, ey := ox+v[0], oy+v[1], ox+v[2], oy+v[3]
			n := segments(math.Hypot(c1x-x, c1y-y) + math.Hypot(c2x-c1x, c2y-c1y) + math.Hypot(ex-c2x, ey-c2y))
			x0, y0 := x, y
			for i := 1; i <= n; i++ {
				t := float64(i) / float64(n)
				u := 1 - t
				add(u*u*u*x0+3*u*u*t*c1x+3*u*t*t*c2x+t*t*t*ex, u*u*u*y0+3*u*u*t*c1y+3*u*t*t*c2y+t*t*t*ey)
			}
			lcx, lcy, x, y = c2x, c2y, ex, ey
		case 'Q', 'T':
			var cx, cy float64
			var v []float64
			var err error
			if upper == 'Q' {
				if v, err = nums(4); err != nil {
					return nil, err
				}
				cx, cy = ox+v[0], oy+v[1]
				v = v[2:]
			} else {
				if v, err = nums(2); err != nil {
					return nil, err
				}
				cx, cy = x, y
				if l := last &^ 0x20; l == 'Q' || l == 'T' {
					cx, cy = 2*x-lcx, 2*y-lcy
				}
			}
			ex, ey := ox+v[0], oy+v[1]
			n := segments(math.Hypot(cx-x, cy-y) + math.Hypot(ex-cx, ey-cy))
			x0, y0 := x, y
			for i := 1; i <= n; i++ {
				t := float64(i) / float64(n)
				u := 1 - t
				add(u*u*x0+2*u*t*cx+t*t*ex, u*u*y0+2*u*t*cy+t*t*ey)
			}
			lcx, lcy, x, y = cx, cy, ex, ey
		case 'A':
			v, err := nums(3)
			if err != nil {
				return nil, err
			}
			large, err := sc.flag()
			if err != nil {
				return nil, err
			}
			sweep, err := sc.flag()
			if err != nil {
				return nil, err
			}
			e, err := nums(2)
			if err != nil {
				return nil, err
			}
			if err := p.count(2); err != nil {
				return nil, err
			}
			ex, ey := ox+e[0], oy+e[1]
			for _, pt := range arc(x, y, v[0], v[1], v[2], large, sweep, ex, ey) {
				add(pt[0], pt[1])
			}
			x, y = ex, ey
		case 'Z':
			if cur != nil {
				cur.Closed = true
				if l := cur.Points[len(cur.Points)-1]; l != [2]float64{sx, sy} {
					cur.Points = append(cur.Points, [2]float64{sx, sy})
					pts++
				}
			}
			x, y = sx, sy
			cur = nil
		}
		last = cmd
		// Points are counted as they come, so a path can't grow past the
		// call's cap before add sees it.
		if p.points+pts > svgMaxPoints {
			return nil, fmt.Errorf("too detailed: at most %d points a call once curves are flattened", svgMaxPoints)
		}
	}
	return out, nil
}

// arc flattens an SVG elliptical arc from (x1, y1) to (x2, y2) (SVG 1.1,
// appendix F.6): the points after the start.
func arc(x1, y1, rx, ry, rot float64, large, sweep bool, x2, y2 float64) [][2]float64 {
	if x1 == x2 && y1 == y2 {
		return nil
	}
	rx, ry = math.Abs(rx), math.Abs(ry)
	if rx == 0 || ry == 0 {
		return [][2]float64{{x2, y2}}
	}
	phi := rot * math.Pi / 180
	cp, sp := math.Cos(phi), math.Sin(phi)
	dx, dy := (x1-x2)/2, (y1-y2)/2
	x1p, y1p := cp*dx+sp*dy, -sp*dx+cp*dy
	if l := x1p*x1p/(rx*rx) + y1p*y1p/(ry*ry); l > 1 {
		s := math.Sqrt(l)
		rx, ry = rx*s, ry*s
	}
	num := rx*rx*ry*ry - rx*rx*y1p*y1p - ry*ry*x1p*x1p
	den := rx*rx*y1p*y1p + ry*ry*x1p*x1p
	co := 0.0
	if den > 0 && num > 0 {
		co = math.Sqrt(num / den)
	}
	if large == sweep {
		co = -co
	}
	cxp, cyp := co*rx*y1p/ry, -co*ry*x1p/rx
	cx, cy := cp*cxp-sp*cyp+(x1+x2)/2, sp*cxp+cp*cyp+(y1+y2)/2
	ang := func(ux, uy, vx, vy float64) float64 {
		return math.Atan2(ux*vy-uy*vx, ux*vx+uy*vy)
	}
	t1 := ang(1, 0, (x1p-cxp)/rx, (y1p-cyp)/ry)
	dt := ang((x1p-cxp)/rx, (y1p-cyp)/ry, (-x1p-cxp)/rx, (-y1p-cyp)/ry)
	if !sweep && dt > 0 {
		dt -= 2 * math.Pi
	} else if sweep && dt < 0 {
		dt += 2 * math.Pi
	}
	n := segments(math.Abs(dt) * math.Max(rx, ry))
	out := make([][2]float64, 0, n)
	for i := 1; i <= n; i++ {
		t := t1 + dt*float64(i)/float64(n)
		ex, ey := rx*math.Cos(t), ry*math.Sin(t)
		out = append(out, [2]float64{cp*ex - sp*ey + cx, sp*ex + cp*ey + cy})
	}
	out[len(out)-1] = [2]float64{x2, y2}
	return out
}

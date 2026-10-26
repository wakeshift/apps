package sketch

import (
	"bytes"
	"flag"
	"image/png"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden image")

// The canvas renders as it always has: the golden image is a drawing with
// every color, every width, a dot, lines at every angle and a polyline.
// Run with -update after a deliberate change, and look at the new image.
func TestRenderGolden(t *testing.T) {
	strokes := []stroke{
		{Points: [][2]int{{10, 80}, {50, 40}, {90, 80}, {10, 80}}, Color: "brown", Width: 3},           // a roof
		{Points: [][2]int{{50, 10}, {50, 40}}, Color: "black", Width: 1},                               // vertical
		{Points: [][2]int{{5, 5}, {95, 5}}, Color: "blue", Width: 6},                                   // horizontal, thick
		{Points: [][2]int{{80, 15}, {95, 30}}, Color: "red", Width: 4},                                 // diagonal
		{Points: [][2]int{{20, 20}}, Color: "yellow", Width: 6},                                        // a dot
		{Points: [][2]int{{0, 100}, {100, 0}}, Color: "white", Width: 2},                               // white over the rest
		{Points: [][2]int{{30, 95}, {35, 88}, {40, 95}, {45, 88}, {50, 95}}, Color: "green", Width: 2}, // grass
	}
	for i, c := range colors {
		strokes = append(strokes, stroke{Points: [][2]int{{60 + i*3, 60}, {60 + i*3, 70}}, Color: c, Width: 1 + i%maxWidth})
	}
	got := renderPNG(strokes, renderSize)
	img, err := png.Decode(bytes.NewReader(got))
	if err != nil || img.Bounds().Dx() != renderSize || img.Bounds().Dy() != renderSize {
		t.Fatalf("not a %dpx PNG: %v", renderSize, err)
	}
	const golden = "testdata/canvas.golden.png"
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		os.WriteFile(t.TempDir()+"/canvas.png", got, 0o644)
		t.Fatalf("the canvas renders differently from %s (run with -update if that's deliberate)", golden)
	}
	// An empty canvas is white.
	blank, _ := png.Decode(bytes.NewReader(renderPNG(nil, 64)))
	if r, g, b, _ := blank.At(31, 31).RGBA(); r != 0xffff || g != 0xffff || b != 0xffff {
		t.Error("a blank canvas isn't white")
	}
}

// A full round's drawing renders quickly: the canvas tool and the pages
// render on every change.
func BenchmarkRender(b *testing.B) {
	var strokes []stroke
	for i := range maxRoundStrokes {
		var pts [][2]int
		for j := range maxPoints {
			pts = append(pts, [2]int{(i*7 + j*13) % 101, (i*11 + j*29) % 101})
		}
		strokes = append(strokes, stroke{Points: pts, Color: colors[i%len(colors)], Width: 1 + i%maxWidth})
	}
	for b.Loop() {
		renderPNG(strokes, renderSize)
	}
}

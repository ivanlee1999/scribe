package render

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"testing"

	"github.com/ivanlee1999/scribe/internal/sb"
)

func pt(x, y float32, pressure float32) sb.Point {
	p := pressure
	return sb.Point{X: x, Y: y, Pressure: &p}
}

// curve samples a parametric function into a stroke, so the fixtures look like writing rather
// than like the straight lines that would hide every joint and cap bug.
func curve(f func(t float64) (float64, float64), steps int) []sb.Point {
	pts := make([]sb.Point, 0, steps+1)
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x, y := f(t)
		pts = append(pts, pt(float32(x), float32(y), 0.4+0.5*float32(math.Sin(t*math.Pi))))
	}
	return pts
}

// handwriting builds a word — "hello" — with a highlighter swipe beneath it.
func handwriting() []Stroke {
	const base = 300.0
	x := 200.0
	var strokes []Stroke
	add := func(pts []sb.Point, size float32, pen string) {
		strokes = append(strokes, Stroke{Points: pts, Size: size, Pen: pen})
	}

	add(curve(func(t float64) (float64, float64) { return x, base - 90 + t*90 }, 20), 4, "BALLPEN")
	add(curve(func(t float64) (float64, float64) {
		return x + 30*math.Sin(t*math.Pi), base - 35*math.Sin(t*math.Pi)
	}, 24), 4, "BALLPEN")
	x += 55

	add(curve(func(t float64) (float64, float64) {
		a := t*2*math.Pi*0.85 + 2.4
		return x + 18*math.Cos(a), base - 18 + 18*math.Sin(a)
	}, 26), 4, "BALLPEN")
	x += 50

	for range 2 {
		here := x
		add(curve(func(t float64) (float64, float64) { return here, base - 90 + t*90 }, 20), 4, "BALLPEN")
		x += 34
	}

	here := x
	add(curve(func(t float64) (float64, float64) {
		a := t * 2 * math.Pi
		return here + 19*math.Cos(a), base - 18 + 18*math.Sin(a)
	}, 28), 4, "BALLPEN")

	add(curve(func(t float64) (float64, float64) { return 190 + t*220, base + 4 }, 12), 34, "MARKER")
	return strokes
}

func decode(t *testing.T, result Result) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(result.PNG))
	if err != nil {
		t.Fatalf("the output is not a readable PNG: %v", err)
	}
	return img
}

// darkness reports the fraction of pixels that are ink rather than paper.
func darkness(img image.Image) float64 {
	bounds := img.Bounds()
	var dark int
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if (r+g+b)/3 < 0x8000 {
				dark++
			}
		}
	}
	return float64(dark) / float64(bounds.Dx()*bounds.Dy())
}

func TestAPageOfWritingRendersAsInkOnWhite(t *testing.T) {
	result, err := Render(handwriting(), DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if result.Empty {
		t.Fatal("a page with writing on it rendered as empty")
	}

	img := decode(t, result)
	// Corners are paper. A renderer that filled the background with ink, or forgot to fill it at
	// all and left transparent black, fails here.
	for _, p := range []image.Point{
		img.Bounds().Min,
		{X: img.Bounds().Max.X - 1, Y: img.Bounds().Min.Y},
		{X: img.Bounds().Min.X, Y: img.Bounds().Max.Y - 1},
	} {
		r, g, b, a := img.At(p.X, p.Y).RGBA()
		if r < 0xF000 || g < 0xF000 || b < 0xF000 || a < 0xF000 {
			t.Errorf("corner %v is not white: %v", p, img.At(p.X, p.Y))
		}
	}

	// Some ink, but not a blot. Both failures are ones a human would spot instantly and no
	// other assertion here would.
	if d := darkness(img); d < 0.005 || d > 0.35 {
		t.Errorf("ink covers %.1f%% of the image, which is not writing", d*100)
	}

	// RENDER_ARTIFACT=path writes the image out so it can actually be looked at. Rendering is
	// one of the few things where the eye catches what assertions cannot.
	if path := os.Getenv("RENDER_ARTIFACT"); path != "" {
		if err := os.WriteFile(path, result.PNG, 0o644); err != nil {
			t.Fatalf("writing artifact: %v", err)
		}
		t.Logf("wrote %s (%dx%d at %.2fx)", path, result.Width, result.Height, result.Scale)
	}
}

func TestTheImageIsCroppedToTheWriting(t *testing.T) {
	// A few words at the top of an A4 page. Without cropping the model sees them shrunk into a
	// corner of a mostly blank sheet, and every recognizer resizes its input.
	strokes := handwriting()

	result, err := Render(strokes, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// The writing spans roughly 200 units of height on a 1980-unit page; the render must not be
	// anywhere near page-shaped.
	if ratio := float64(result.Height) / float64(result.Width); ratio > 0.9 {
		t.Errorf("output is %dx%d — that is the whole sheet, not the writing", result.Width, result.Height)
	}
}

func TestTheHighlighterDoesNotBuryTheWriting(t *testing.T) {
	// MARKER is a 25-80 unit wide light grey wash meant to sit behind text. Drawn in ink, or
	// drawn last, it turns the words it highlights into a black bar.
	word := handwriting()

	var withoutMarker []Stroke
	for _, s := range word {
		if s.Pen != "MARKER" {
			withoutMarker = append(withoutMarker, s)
		}
	}

	withMarker, err := Render(word, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	plain, err := Render(withoutMarker, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// The highlighter is light, so it must add almost nothing to the *dark* pixel count.
	added := darkness(decode(t, withMarker)) - darkness(decode(t, plain))
	if added > 0.02 {
		t.Errorf("the highlighter darkened the page by %.1f%% — it is being drawn as ink", added*100)
	}
}

func TestStrokesAreSolidRatherThanHollow(t *testing.T) {
	// The bug this exists for: the rasterizer accumulates signed area, so a segment quad wound
	// opposite to its round joins cancels where the two overlap. Every stroke then renders as a
	// hollow outline with notches at the joints — which still registers as "some ink" on every
	// coverage check, and is unreadable to a recognizer. Only looking *inside* a stroke finds it.
	const y = 200
	thick := Stroke{Size: 40, Pen: "BALLPEN"}
	for x := 100; x <= 500; x += 20 {
		thick.Points = append(thick.Points, pt(float32(x), y, 0.9))
	}

	result, err := Render([]Stroke{thick}, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img := decode(t, result)

	// Walk the middle of the stroke end to end. A solid stroke has no paper along its spine.
	bounds := img.Bounds()
	row := bounds.Min.Y + bounds.Dy()/2
	holes := 0
	for x := bounds.Min.X + bounds.Dx()/10; x < bounds.Max.X-bounds.Dx()/10; x++ {
		if isPaper(img.At(x, row)) {
			holes++
		}
	}
	if holes > 0 {
		t.Errorf("%d paper pixels inside a 40-unit stroke — it is being drawn hollow", holes)
	}
}

func TestAPageWithNoInkIsNotRendered(t *testing.T) {
	// Handing a recognizer a blank sheet spends a model run to be told what was already known.
	result, err := Render(nil, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !result.Empty || result.PNG != nil {
		t.Error("an empty page produced an image")
	}

	result, err = Render([]Stroke{{Pen: "BALLPEN", Size: 3}}, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !result.Empty {
		t.Error("a stroke with no points produced an image")
	}
}

func TestASingleDotIsDrawn(t *testing.T) {
	// A full stop, or the tittle on an i. A renderer that only draws segments loses both.
	result, err := Render([]Stroke{{
		Points: []sb.Point{pt(100, 100, 0.8)}, Size: 6, Pen: "BALLPEN",
	}}, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if result.Empty {
		t.Fatal("a dot rendered as nothing")
	}
	if darkness(decode(t, result)) == 0 {
		t.Error("a dot rendered as a blank image")
	}
}

func TestAVeryTallPageStaysWithinItsPixelBudget(t *testing.T) {
	// Continuous scroll produces pages far taller than a sheet. Scaling those by the default
	// factor would ask for a bitmap measured in gigabytes.
	var strokes []Stroke
	for y := 0; y < 40000; y += 500 {
		strokes = append(strokes, Stroke{
			Points: []sb.Point{pt(100, float32(y), 0.5), pt(600, float32(y)+30, 0.5)},
			Size:   4, Pen: "BALLPEN",
		})
	}

	opts := DefaultOptions(1400, 1980)
	result, err := Render(strokes, opts)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if longest := max(result.Width, result.Height); longest > opts.LongestSide {
		t.Errorf("longest side is %d, over the %d budget", longest, opts.LongestSide)
	}
}

func TestInkOutsideTheDeclaredPageIsStillDrawn(t *testing.T) {
	// A page whose declared size is wrong — or one written past its own end, which continuous
	// scroll does routinely — must still be readable.
	strokes := []Stroke{{
		Points: []sb.Point{pt(100, 5000, 0.6), pt(400, 5040, 0.6)},
		Size:   4, Pen: "BALLPEN",
	}}

	result, err := Render(strokes, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if result.Empty || darkness(decode(t, result)) == 0 {
		t.Error("ink below the declared page height was dropped")
	}
}

func TestPressureNarrowsTheLineWithoutBreakingIt(t *testing.T) {
	// A stroke tapering to nothing renders as a broken hairline, which reads worse than one
	// drawn slightly too heavy.
	faint := Stroke{Size: 4, Pen: "BALLPEN"}
	for x := 100; x < 400; x += 10 {
		faint.Points = append(faint.Points, pt(float32(x), 200, 0.001))
	}

	result, err := Render([]Stroke{faint}, DefaultOptions(1400, 1980))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img := decode(t, result)

	// Walk the row the stroke was drawn on; it must be continuous.
	bounds := img.Bounds()
	row := bounds.Min.Y + bounds.Dy()/2
	var gaps, longestGap, run int
	for x := bounds.Min.X; x < bounds.Max.X; x++ {
		if isPaper(img.At(x, row)) {
			run++
			if run > longestGap {
				longestGap = run
			}
		} else {
			if run > 0 {
				gaps++
			}
			run = 0
		}
	}
	if longestGap > bounds.Dx()/4 {
		t.Errorf("the faint stroke has a %dpx hole in a %dpx image", longestGap, bounds.Dx())
	}
}

func isPaper(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return (r+g+b)/3 > 0xC000
}

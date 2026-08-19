// Package render turns decoded ink into an image a recognizer can read.
//
// The output is deliberately not a faithful reproduction of the page. It is the input to an OCR
// model, so it is optimized for that: ink on plain white, no paper template, no placed images,
// and cropped to the writing. The iPad's recognizer makes the same choices for the same reason —
// ruled and dotted paper is precisely the input that degrades handwriting recognition, and none
// of it is the user's writing.
package render

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"golang.org/x/image/vector"

	"github.com/ivanlee1999/scribe/internal/sb"
)

// Stroke is one pen movement, in page coordinates.
type Stroke struct {
	Points []sb.Point
	// Nib width in page units.
	Size float32
	// The pen it was drawn with, as the wire format spells it (BALLPEN, MARKER, ...).
	Pen string
}

// Options control the bitmap the model is handed.
type Options struct {
	// PageWidth and PageHeight bound the sheet, in page units. Ink outside them is still drawn:
	// a page whose declared size is wrong should still be readable.
	PageWidth, PageHeight float32
	// LongestSide caps the output's larger dimension in pixels. Bigger is not better past the
	// point the model downsamples anyway, and it is memory the N100 does not have to spare.
	LongestSide int
	// MaxScale stops a nearly-empty crop from being blown up into a blurry poster.
	MaxScale float64
	// Margin is the white space kept around the ink, in page units. Some breathing room helps:
	// several recognizers do noticeably worse on glyphs that touch the edge of the image.
	Margin float32
}

// DefaultOptions are tuned for handwriting at the sizes these apps produce — roughly a 40-unit
// tall line of writing, which lands well clear of the ~32px x-height most recognizers want.
func DefaultOptions(pageWidth, pageHeight float32) Options {
	return Options{
		PageWidth:   pageWidth,
		PageHeight:  pageHeight,
		LongestSide: 3000,
		MaxScale:    3,
		Margin:      24,
	}
}

// Result is the rendered page, with what it took to get there.
type Result struct {
	PNG []byte
	// Pixel dimensions of the image.
	Width, Height int
	// What the page coordinates were multiplied by.
	Scale float64
	// True when there was no ink to draw, in which case PNG is nil.
	Empty bool
}

var (
	inkColor       = color.RGBA{A: 255}                             // black
	highlightColor = color.RGBA{R: 170, G: 170, B: 170, A: 255}      // MARKER, kept light
	paper          = color.RGBA{R: 255, G: 255, B: 255, A: 255}
)

// Render draws the strokes and encodes them as a PNG.
//
// It returns Empty for a page with no ink rather than a blank image: handing a recognizer an
// empty sheet wastes a model run to be told what was already obvious.
func Render(strokes []Stroke, opts Options) (Result, error) {
	bounds, ok := inkBounds(strokes)
	if !ok {
		return Result{Empty: true}, nil
	}

	// Crop to the writing. On a page with a few lines at the top, this is the difference between
	// the model seeing the words at full resolution and seeing them shrunk into the corner of a
	// mostly blank sheet — every recognizer resizes its input, so the empty half costs accuracy.
	bounds = bounds.expand(opts.Margin)
	bounds = bounds.clampTo(opts.PageWidth, opts.PageHeight)

	width, height := bounds.size()
	scale := opts.MaxScale
	if longest := math.Max(float64(width), float64(height)); longest > 0 {
		if fit := float64(opts.LongestSide) / longest; fit < scale {
			scale = fit
		}
	}
	if scale <= 0 {
		scale = 1
	}

	pxWidth := int(math.Ceil(float64(width) * scale))
	pxHeight := int(math.Ceil(float64(height) * scale))
	if pxWidth < 1 {
		pxWidth = 1
	}
	if pxHeight < 1 {
		pxHeight = 1
	}

	img := image.NewRGBA(image.Rect(0, 0, pxWidth, pxHeight))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: paper}, image.Point{}, draw.Src)

	// Highlighter first, ink over it — the order they were meant to be read in. Drawing a wide
	// grey MARKER stroke on top of the writing it highlights would bury the writing.
	for _, pass := range []struct {
		colour color.RGBA
		want   func(Stroke) bool
	}{
		{highlightColor, func(s Stroke) bool { return isHighlighter(s.Pen) }},
		{inkColor, func(s Stroke) bool { return !isHighlighter(s.Pen) }},
	} {
		r := vector.NewRasterizer(pxWidth, pxHeight)
		drew := false
		for _, stroke := range strokes {
			if !pass.want(stroke) {
				continue
			}
			if addStroke(r, stroke, bounds, scale) {
				drew = true
			}
		}
		if drew {
			r.Draw(img, img.Bounds(), &image.Uniform{C: pass.colour}, image.Point{})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return Result{}, err
	}
	return Result{
		PNG:    buf.Bytes(),
		Width:  pxWidth,
		Height: pxHeight,
		Scale:  scale,
	}, nil
}

// isHighlighter reports whether this pen lays down a wash meant to sit behind writing.
func isHighlighter(pen string) bool { return pen == "MARKER" }

// addStroke traces one stroke into the rasterizer, returning whether anything was drawn.
//
// Each segment becomes a quadrilateral whose ends are as wide as the nib was at those points,
// and each point gets a disc — which is what turns a chain of quads into a stroke with round
// joins and caps instead of a string of sausages with notches at every bend.
func addStroke(r *vector.Rasterizer, stroke Stroke, bounds box, scale float64) bool {
	if len(stroke.Points) == 0 {
		return false
	}

	at := func(p sb.Point) (float32, float32) {
		return float32((float64(p.X) - float64(bounds.minX)) * scale),
			float32((float64(p.Y) - float64(bounds.minY)) * scale)
	}

	radii := make([]float64, len(stroke.Points))
	for i, p := range stroke.Points {
		radii[i] = radius(stroke, p, scale)
	}

	// A single point is a dot — a full stop, or the tittle on an i, both of which matter.
	if len(stroke.Points) == 1 {
		x, y := at(stroke.Points[0])
		disc(r, x, y, radii[0])
		return true
	}

	for i := 0; i < len(stroke.Points)-1; i++ {
		x0, y0 := at(stroke.Points[i])
		x1, y1 := at(stroke.Points[i+1])
		segment(r, x0, y0, x1, y1, radii[i], radii[i+1])
	}
	for i, p := range stroke.Points {
		x, y := at(p)
		disc(r, x, y, radii[i])
	}
	return true
}

// radius is half the nib width at one point, in pixels.
//
// Pressure narrows the line but never to nothing: a light stroke that renders as a broken hairline
// reads worse than one drawn a shade too heavy, and the tablets' own renderers keep a floor for
// the same reason.
func radius(stroke Stroke, p sb.Point, scale float64) float64 {
	width := float64(stroke.Size)
	if width <= 0 {
		width = 3
	}
	if p.Pressure != nil {
		pressure := float64(*p.Pressure)
		if pressure < 0 {
			pressure = 0
		} else if pressure > 1 {
			pressure = 1
		}
		// Square root rather than linear: it matches the curve the BOOX's fountain pen uses, and
		// keeps light strokes visible instead of letting them fade out.
		width *= 0.35 + 0.65*math.Sqrt(pressure)
	}
	r := width * scale / 2
	if r < 0.4 {
		r = 0.4
	}
	return r
}

func segment(r *vector.Rasterizer, x0, y0, x1, y1 float32, r0, r1 float64) {
	dx := float64(x1 - x0)
	dy := float64(y1 - y0)
	length := math.Hypot(dx, dy)
	if length == 0 {
		return
	}
	// Perpendicular unit vector, scaled to each end's radius.
	nx := -dy / length
	ny := dx / length

	// Wound the same way round as disc(), and that is not cosmetic. The rasterizer accumulates
	// signed area, so two overlapping subpaths of opposite handedness cancel to nothing — which
	// renders every stroke as a hollow outline with notches at the joints, ink only where the
	// quad and the disc happen not to overlap. Same direction, and they add instead.
	r.MoveTo(x0+float32(nx*r0), y0+float32(ny*r0))
	r.LineTo(x0-float32(nx*r0), y0-float32(ny*r0))
	r.LineTo(x1-float32(nx*r1), y1-float32(ny*r1))
	r.LineTo(x1+float32(nx*r1), y1+float32(ny*r1))
	r.ClosePath()
}

// discSegments is enough sides that a joint reads as round at these radii, and few enough that a
// page of several thousand strokes still renders in well under a second.
const discSegments = 12

func disc(r *vector.Rasterizer, cx, cy float32, radius float64) {
	if radius <= 0 {
		return
	}
	for i := 0; i <= discSegments; i++ {
		angle := 2 * math.Pi * float64(i) / discSegments
		x := cx + float32(math.Cos(angle)*radius)
		y := cy + float32(math.Sin(angle)*radius)
		if i == 0 {
			r.MoveTo(x, y)
		} else {
			r.LineTo(x, y)
		}
	}
	r.ClosePath()
}

// box is an axis-aligned rectangle in page coordinates.
type box struct {
	minX, minY, maxX, maxY float32
}

func inkBounds(strokes []Stroke) (box, bool) {
	b := box{
		minX: float32(math.Inf(1)), minY: float32(math.Inf(1)),
		maxX: float32(math.Inf(-1)), maxY: float32(math.Inf(-1)),
	}
	found := false
	for _, stroke := range strokes {
		for _, p := range stroke.Points {
			// Half the nib hangs outside the centre line; without it a thick stroke at the edge
			// of the writing gets shaved off by the crop.
			pad := float32(stroke.Size) / 2
			if p.X-pad < b.minX {
				b.minX = p.X - pad
			}
			if p.Y-pad < b.minY {
				b.minY = p.Y - pad
			}
			if p.X+pad > b.maxX {
				b.maxX = p.X + pad
			}
			if p.Y+pad > b.maxY {
				b.maxY = p.Y + pad
			}
			found = true
		}
	}
	return b, found
}

func (b box) expand(by float32) box {
	return box{b.minX - by, b.minY - by, b.maxX + by, b.maxY + by}
}

// clampTo keeps the crop inside the sheet where the sheet is known, but never lets it collapse:
// ink drawn beyond a page's declared height (which continuous scroll produces routinely) has to
// stay in the picture.
func (b box) clampTo(pageWidth, pageHeight float32) box {
	if pageWidth > 0 && b.minX < 0 {
		b.minX = 0
	}
	if pageHeight > 0 && b.minY < 0 {
		b.minY = 0
	}
	if pageWidth > 0 && b.maxX < pageWidth && b.maxX < b.minX {
		b.maxX = b.minX
	}
	return b
}

func (b box) size() (float32, float32) {
	w := b.maxX - b.minX
	h := b.maxY - b.minY
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

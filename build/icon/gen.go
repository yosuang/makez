// Command icon draws the makez app icon and writes SVG and PNG files
// into build/icon.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// canvasSize is the viewBox side. Every length below is in this space.
	canvasSize = 512.0

	// padding is the margin above and below the centered mark. Mark height
	// is canvasSize-2*padding. The mark is slightly wider than it is tall,
	// so the side margins come out a little under this (89px).
	padding = 96.0

	// cornerRadius is the rounded-square tile corner.
	cornerRadius = 120.0

	// barThickness is the vertical thickness of the top and bottom bars.
	barThickness = 100.0

	// The main diagonal, the bar-end cuts, and the thin stroke all share
	// this slope: 2 units right for every 3 units down (about 56.3°).
	diagonalRun  = 2.0
	diagonalRise = 3.0

	// diagonalWidth is the perpendicular thickness of the main stroke.
	diagonalWidth = 100.0

	// innerRun is the length of each bar's flat inner edge, the underside
	// of the top bar and the topside of the bottom bar.
	innerRun = 108.0

	// accentWidth is the perpendicular thickness of the thin stroke that
	// runs on the lower-right side of the diagonal.
	accentWidth = 28.0

	// accentGap is the perpendicular gap between the main diagonal and the
	// thin stroke. The stroke spans the opening between the bars, so its
	// ends lie on those inner edges.
	accentGap = 18.0

	// supersample is the grid of samples averaged into each PNG pixel.
	supersample = 8
)

var pngSizes = []int{16, 32, 180, 512}

type variant int

const (
	variantDark variant = iota
	variantLight
	variantMark
)

type point struct {
	x, y float64
}

type geometry struct {
	z      []point
	accent []point
	sin    float64
	cos    float64
	cNW    float64
	cSE    float64
	cNear  float64
	cFar   float64
}

func main() {
	root, err := moduleRoot()
	if err != nil {
		fail(err)
	}
	dir := filepath.Join(root, "build", "icon")
	names, err := writeIcons(dir)
	if err != nil {
		fail(err)
	}
	for _, name := range names {
		fmt.Println(filepath.Join("build", "icon", name))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Stat(filepath.Join(dir, "go.mod"))
		if statErr == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}

// layout builds the mark in canvas coordinates.
//
// The main Z is one polygon. Its outer diagonal edges are parallel lines
// a perpendicular distance diagonalWidth apart: the lower-left point and
// the upper-right point sit on those lines, so each bar ends in a sharp
// cut at the diagonal slope. The thin stroke is the same slope, offset
// past the lower-right edge by accentGap, and clipped to the horizontal
// inner edges of the bars. The finished mark is translated so its bounding
// box is centered on the canvas.
func layout() (geometry, error) {
	sin, cos := diagonalBasis()
	markH := canvasSize - 2*padding
	if barThickness*2 >= markH {
		return geometry{}, fmt.Errorf("bar thickness %v leaves no opening in mark height %v", barThickness, markH)
	}
	yTop := barThickness
	yBot := markH - barThickness

	// Line form is x*sin + y*cos = c. (sin, cos) is a unit normal pointing
	// down-right, so the difference in c is the perpendicular distance.
	cNW := markH * cos
	cSE := cNW + diagonalWidth
	right := cSE / sin
	xNW := (cNW - yTop*cos) / sin
	xSEBot := (cSE - yBot*cos) / sin
	xLeft := xNW - innerRun
	xRight := right - xLeft
	if xLeft <= 0 || xRight <= xSEBot {
		return geometry{}, fmt.Errorf("inner run %v does not fit the bars", innerRun)
	}

	z := []point{
		{xLeft, 0},
		{right, 0},
		{xSEBot, yBot},
		{xRight, yBot},
		{xRight, markH},
		{0, markH},
		{xNW, yTop},
		{xLeft, yTop},
	}
	cNear := cSE + accentGap
	cFar := cNear + accentWidth
	accent := []point{
		{(cNear - yTop*cos) / sin, yTop},
		{(cFar - yTop*cos) / sin, yTop},
		{(cFar - yBot*cos) / sin, yBot},
		{(cNear - yBot*cos) / sin, yBot},
	}
	if accent[1].x > right || accent[2].x > xRight {
		return geometry{}, fmt.Errorf("thin stroke extends past the mark")
	}

	minX, minY, maxX, maxY := bounds(z, accent)
	tx := (canvasSize - minX - maxX) / 2
	ty := (canvasSize - minY - maxY) / 2
	translate(z, tx, ty)
	translate(accent, tx, ty)
	shift := tx*sin + ty*cos
	return geometry{
		z: z, accent: accent,
		sin: sin, cos: cos,
		cNW: cNW + shift, cSE: cSE + shift,
		cNear: cNear + shift, cFar: cFar + shift,
	}, nil
}

func diagonalBasis() (sin, cos float64) {
	h := math.Hypot(diagonalRun, diagonalRise)
	return diagonalRise / h, diagonalRun / h
}

func translate(poly []point, tx, ty float64) {
	for i := range poly {
		poly[i].x += tx
		poly[i].y += ty
	}
}

func bounds(polys ...[]point) (minX, minY, maxX, maxY float64) {
	minX, minY = math.Inf(1), math.Inf(1)
	maxX, maxY = math.Inf(-1), math.Inf(-1)
	for _, poly := range polys {
		for _, p := range poly {
			minX = math.Min(minX, p.x)
			minY = math.Min(minY, p.y)
			maxX = math.Max(maxX, p.x)
			maxY = math.Max(maxY, p.y)
		}
	}
	return minX, minY, maxX, maxY
}

func contains(poly []point, x, y float64) bool {
	inside := false
	j := len(poly) - 1
	for i := range poly {
		pi, pj := poly[i], poly[j]
		if (pi.y > y) != (pj.y > y) {
			xAt := pi.x + (y-pi.y)/(pj.y-pi.y)*(pj.x-pi.x)
			if x < xAt {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

func inRoundedRect(x, y float64) bool {
	if x < 0 || y < 0 || x > canvasSize || y > canvasSize {
		return false
	}
	dx, dy := 0.0, 0.0
	if x < cornerRadius {
		dx = cornerRadius - x
	} else if x > canvasSize-cornerRadius {
		dx = x - (canvasSize - cornerRadius)
	}
	if y < cornerRadius {
		dy = cornerRadius - y
	} else if y > canvasSize-cornerRadius {
		dy = y - (canvasSize - cornerRadius)
	}
	if dx == 0 && dy == 0 {
		return true
	}
	return dx*dx+dy*dy <= cornerRadius*cornerRadius
}

func inMark(g geometry, x, y float64) bool {
	return contains(g.z, x, y) || contains(g.accent, x, y)
}

type style struct {
	tile bool
	bg   color.NRGBA
	fg   color.NRGBA
}

func styleOf(v variant) style {
	switch v {
	case variantLight:
		return style{tile: true, bg: color.NRGBA{R: 255, G: 255, B: 255, A: 255}, fg: color.NRGBA{A: 255}}
	case variantMark:
		return style{fg: color.NRGBA{A: 255}}
	default:
		return style{tile: true, bg: color.NRGBA{A: 255}, fg: color.NRGBA{R: 255, G: 255, B: 255, A: 255}}
	}
}

func render(g geometry, size int, v variant) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := styleOf(v)
	samples := supersample
	n := float64(samples * samples)
	unit := canvasSize / float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var pr, pg, pb, pa float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					cx := (float64(x) + (float64(sx)+0.5)/float64(samples)) * unit
					cy := (float64(y) + (float64(sy)+0.5)/float64(samples)) * unit
					onMark := inMark(g, cx, cy)
					if s.tile {
						if !inRoundedRect(cx, cy) {
							continue
						}
						pa++
						c := s.bg
						if onMark {
							c = s.fg
						}
						pr += float64(c.R) / 255
						pg += float64(c.G) / 255
						pb += float64(c.B) / 255
						continue
					}
					if onMark {
						pa++
						pr += float64(s.fg.R) / 255
						pg += float64(s.fg.G) / 255
						pb += float64(s.fg.B) / 255
					}
				}
			}
			if pa == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: channel(pr / pa),
				G: channel(pg / pa),
				B: channel(pb / pa),
				A: channel(pa / n),
			})
		}
	}
	return img
}

func channel(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return uint8(math.Round(v * 255))
}

func svg(g geometry, v variant) []byte {
	s := styleOf(v)
	var b strings.Builder
	side := coord(canvasSize)
	fmt.Fprintf(&b, "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"%s\" height=\"%s\" viewBox=\"0 0 %s %s\">\n", side, side, side, side)
	if s.tile {
		fmt.Fprintf(&b, "<rect width=\"%s\" height=\"%s\" rx=\"%s\" fill=\"%s\"/>\n", side, side, coord(cornerRadius), hex(s.bg))
	}
	fmt.Fprintf(&b, "<path fill=\"%s\" d=\"%s %s\"/>\n", hex(s.fg), pathData(g.z), pathData(g.accent))
	b.WriteString("</svg>\n")
	return []byte(b.String())
}

func pathData(poly []point) string {
	var b strings.Builder
	for i, p := range poly {
		if i == 0 {
			b.WriteByte('M')
		} else {
			b.WriteByte('L')
		}
		b.WriteString(coord(p.x))
		b.WriteByte(' ')
		b.WriteString(coord(p.y))
	}
	b.WriteByte('Z')
	return b.String()
}

func coord(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "-0" {
		return "0"
	}
	return s
}

func hex(c color.NRGBA) string {
	switch {
	case c.R == 0 && c.G == 0 && c.B == 0:
		return "#000"
	case c.R == 255 && c.G == 255 && c.B == 255:
		return "#fff"
	default:
		return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
	}
}

func fileName(v variant, size int) string {
	base := "icon-dark"
	switch v {
	case variantLight:
		base = "icon-light"
	case variantMark:
		base = "mark"
	}
	if size == 0 {
		return base + ".svg"
	}
	return fmt.Sprintf("%s-%d.png", base, size)
}

func outputNames() []string {
	var names []string
	for _, v := range []variant{variantDark, variantLight, variantMark} {
		names = append(names, fileName(v, 0))
		for _, size := range pngSizes {
			names = append(names, fileName(v, size))
		}
	}
	return names
}

func writeIcons(dir string) ([]string, error) {
	g, err := layout()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	names := outputNames()
	for _, v := range []variant{variantDark, variantLight, variantMark} {
		if err = os.WriteFile(filepath.Join(dir, fileName(v, 0)), svg(g, v), 0o644); err != nil {
			return nil, err
		}
		for _, size := range pngSizes {
			var buf bytes.Buffer
			if err = png.Encode(&buf, render(g, size, v)); err != nil {
				return nil, err
			}
			if err = os.WriteFile(filepath.Join(dir, fileName(v, size)), buf.Bytes(), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return names, nil
}

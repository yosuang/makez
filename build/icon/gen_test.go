package main

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutGeometry(t *testing.T) {
	g := mustLayout(t)
	sin, cos := diagonalBasis()

	if math.Abs((g.cSE-g.cNW)-diagonalWidth) > 1e-9 {
		t.Fatalf("diagonal width %v", g.cSE-g.cNW)
	}
	if math.Abs((g.cNear-g.cSE)-accentGap) > 1e-9 {
		t.Fatalf("accent gap %v", g.cNear-g.cSE)
	}
	if math.Abs((g.cFar-g.cNear)-accentWidth) > 1e-9 {
		t.Fatalf("accent width %v", g.cFar-g.cNear)
	}
	if math.Abs(g.sin-sin) > 1e-12 || math.Abs(g.cos-cos) > 1e-12 {
		t.Fatal("basis changed")
	}

	if math.Abs((g.z[7].y-g.z[0].y)-barThickness) > 1e-9 {
		t.Fatalf("top bar thickness %v", g.z[7].y-g.z[0].y)
	}
	if math.Abs((g.z[4].y-g.z[3].y)-barThickness) > 1e-9 {
		t.Fatalf("bottom bar thickness %v", g.z[4].y-g.z[3].y)
	}
	counter := canvasSize - 2*padding - 2*barThickness
	if math.Abs((g.z[3].y-g.z[7].y)-counter) > 1e-9 {
		t.Fatalf("counter height %v", g.z[3].y-g.z[7].y)
	}
	if math.Abs((g.z[6].x-g.z[7].x)-innerRun) > 1e-6 {
		t.Fatalf("top inner edge %v", g.z[6].x-g.z[7].x)
	}
	if math.Abs((g.z[3].x-g.z[2].x)-innerRun) > 1e-6 {
		t.Fatalf("bottom inner edge %v", g.z[3].x-g.z[2].x)
	}

	assertParallel(t, g.z[1], g.z[2], g.accent[0], g.accent[3])
	assertParallel(t, g.z[5], g.z[6], g.accent[1], g.accent[2])
	if math.Abs(g.accent[0].y-g.z[7].y) > 1e-9 || math.Abs(g.accent[1].y-g.z[7].y) > 1e-9 {
		t.Fatal("accent does not start on the top bar")
	}
	if math.Abs(g.accent[2].y-g.z[3].y) > 1e-9 || math.Abs(g.accent[3].y-g.z[3].y) > 1e-9 {
		t.Fatal("accent does not end on the bottom bar")
	}

	minX, minY, maxX, maxY := bounds(g.z, g.accent)
	if math.Abs((minX+maxX)/2-canvasSize/2) > 1e-6 || math.Abs((minY+maxY)/2-canvasSize/2) > 1e-6 {
		t.Fatalf("mark center (%v,%v)", (minX+maxX)/2, (minY+maxY)/2)
	}
	if math.Abs(minY-padding) > 1e-6 || math.Abs((canvasSize-maxY)-padding) > 1e-6 {
		t.Fatalf("vertical padding %v %v", minY, canvasSize-maxY)
	}
	if minX < 80 || canvasSize-maxX < 80 {
		t.Fatalf("side padding %v %v", minX, canvasSize-maxX)
	}
	if g.accent[1].x > maxX+1e-6 || g.accent[0].x < minX-1e-6 {
		t.Fatal("accent leaves the mark bounds")
	}

	for _, p := range append(append([]point{}, g.z...), g.accent...) {
		if !inRoundedRect(p.x, p.y) {
			t.Fatalf("vertex (%v,%v) falls outside the tile", p.x, p.y)
		}
	}

	midY := (g.z[7].y + g.z[3].y) / 2
	onLine := func(c float64) point {
		return point{x: (c - midY*g.cos) / g.sin, y: midY}
	}
	diagPt := onLine((g.cNW + g.cSE) / 2)
	if !contains(g.z, diagPt.x, diagPt.y) {
		t.Fatal("diagonal center is empty")
	}
	gapPt := onLine((g.cSE + g.cNear) / 2)
	if inMark(g, gapPt.x, gapPt.y) {
		t.Fatal("gap is filled")
	}
	accentPt := onLine((g.cNear + g.cFar) / 2)
	if !contains(g.accent, accentPt.x, accentPt.y) {
		t.Fatal("accent center is empty")
	}
	if crosses(g.z) || crosses(g.accent) {
		t.Fatal("polygon self-intersects")
	}
}

func TestSVGVariants(t *testing.T) {
	g := mustLayout(t)
	dark := string(svg(g, variantDark))
	light := string(svg(g, variantLight))
	mark := string(svg(g, variantMark))

	for _, doc := range []string{dark, light, mark} {
		if !strings.Contains(doc, `viewBox="0 0 512 512"`) {
			t.Fatalf("missing viewBox: %s", doc)
		}
		if strings.Count(doc, "M") != 2 {
			t.Fatal("expected the Z and the thin stroke as two subpaths")
		}
	}
	if !strings.Contains(dark, `fill="#000"`) || !strings.Contains(dark, `fill="#fff"`) || !strings.Contains(dark, `rx="120"`) {
		t.Fatal(dark)
	}
	if !strings.Contains(light, `<rect width="512" height="512" rx="120" fill="#fff"/>`) || !strings.Contains(light, `fill="#000"`) {
		t.Fatal(light)
	}
	if strings.Contains(mark, "<rect") || !strings.Contains(mark, `fill="#000"`) {
		t.Fatal(mark)
	}
}

func TestPNGTiles(t *testing.T) {
	g := mustLayout(t)
	dark := render(g, 512, variantDark)
	light := render(g, 512, variantLight)
	mark := render(g, 512, variantMark)

	if dark.Bounds().Dx() != 512 || dark.NRGBAAt(0, 0).A != 0 {
		t.Fatal("dark tile corner should be transparent")
	}
	bar := point{(g.z[0].x + g.z[1].x) / 2, (g.z[0].y + g.z[7].y) / 2}
	bg := point{canvasSize / 2, padding / 2}
	assertPixel(t, dark, bar, 255, 255)
	assertPixel(t, dark, bg, 0, 255)
	assertPixel(t, light, bar, 0, 255)
	assertPixel(t, light, bg, 255, 255)
	assertPixel(t, mark, bar, 0, 255)
	bx, by := pixelOf(bg, 512)
	if mark.NRGBAAt(bx, by).A != 0 {
		t.Fatal("mark background is not transparent")
	}
}

func TestAccentSeparatesAt32px(t *testing.T) {
	g := mustLayout(t)
	img := render(g, 32, variantDark)
	valley, peak, ok := accentSeparation(img)
	if !ok || valley > 80 || peak < 180 {
		t.Fatalf("32px thin stroke and gap are not distinct (valley %d peak %d ok %v)", valley, peak, ok)
	}
}

func TestOutputsMatchGenerator(t *testing.T) {
	dir := t.TempDir()
	names, err := writeIcons(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := moduleRootForTest(t)
	committed := filepath.Join(root, "build", "icon")
	for _, name := range names {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(committed, name))
		if err != nil {
			t.Fatalf("missing committed %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s does not match the generator output", name)
		}
	}
	f, err := os.Open(filepath.Join(committed, "icon-dark-512.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 512 || cfg.Height != 512 {
		t.Fatalf("icon-dark-512.png is %dx%d", cfg.Width, cfg.Height)
	}
}

func mustLayout(t *testing.T) geometry {
	t.Helper()
	g, err := layout()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func moduleRootForTest(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func assertParallel(t *testing.T, a, b, c, d point) {
	t.Helper()
	ex, ey := b.x-a.x, b.y-a.y
	fx, fy := d.x-c.x, d.y-c.y
	cross := ex*fy - ey*fx
	if math.Abs(cross) > 1e-6 {
		t.Fatalf("edges not parallel: cross %v", cross)
	}
}

func assertPixel(t *testing.T, img *image.NRGBA, p point, want, alpha uint8) {
	t.Helper()
	x, y := pixelOf(p, img.Bounds().Dx())
	c := img.NRGBAAt(x, y)
	if c.R != want || c.A < alpha {
		t.Fatalf("pixel (%d,%d) from (%v,%v) = %v, want channel %d alpha >= %d", x, y, p.x, p.y, c, want, alpha)
	}
}

func pixelOf(p point, size int) (int, int) {
	x := int(p.x / canvasSize * float64(size))
	y := int(p.y / canvasSize * float64(size))
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= size {
		x = size - 1
	}
	if y >= size {
		y = size - 1
	}
	return x, y
}

func crosses(poly []point) bool {
	n := len(poly)
	for i := 0; i < n; i++ {
		a, b := poly[i], poly[(i+1)%n]
		for j := i + 1; j < n; j++ {
			if j == i+1 || (i == 0 && j == n-1) {
				continue
			}
			c, d := poly[j], poly[(j+1)%n]
			if properCross(a, b, c, d) {
				return true
			}
		}
	}
	return false
}

func properCross(a, b, c, d point) bool {
	o1 := orient(a, b, c)
	o2 := orient(a, b, d)
	o3 := orient(c, d, a)
	o4 := orient(c, d, b)
	return o1*o2 < 0 && o3*o4 < 0
}

func orient(a, b, c point) float64 {
	return (b.x-a.x)*(c.y-a.y) - (b.y-a.y)*(c.x-a.x)
}

// accentSeparation reports the darkest pixel sitting between two bright
// runs on any row. It is how the small PNG sizes are checked for a visible
// gap beside the thin stroke.
func accentSeparation(img *image.NRGBA) (valley, peak int, ok bool) {
	b := img.Bounds()
	valley = 255
	for y := b.Min.Y; y < b.Max.Y; y++ {
		vals := make([]int, b.Dx())
		for x := 0; x < b.Dx(); x++ {
			vals[x] = int(img.NRGBAAt(x, y).R)
		}
		for x := 0; x < len(vals); x++ {
			if vals[x] < 180 {
				continue
			}
			darkest := 255
			for k := x + 1; k < len(vals) && k <= x+8; k++ {
				if vals[k] < darkest {
					darkest = vals[k]
				}
				if darkest < 160 && vals[k] >= 180 && vals[k] > darkest+30 {
					if darkest < valley {
						valley = darkest
						peak = vals[k]
						ok = true
					}
					break
				}
			}
		}
	}
	return valley, peak, ok
}

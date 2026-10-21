// Command icon writes the makez icon as two SVGs.
//
// makez-16.svg and makez-32.svg share one geometry, traced from the
// approved 1024×1024 artwork. Only the width and height differ. The tile
// is a full black square; the mark is white.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// view is the shared coordinate space. The constants below are pixel
	// positions measured on the 1024 reference (half-coverage edges).
	view = 1024

	// Top bar. The right end is the sharp top of the diagonal.
	topLeft   = 277
	topY      = 204
	topBottom = 310
	topPointX = 779

	// The diagonal's upper-left edge runs from the underside of the top
	// bar to the top of the vertical foot.
	meetX   = 567
	footX   = 260
	footTop = 690

	// The southeast edge is the straight cut from the top point to the
	// bottom of the foot, where it meets the vertical edge.
	footBottom = 809

	// Bottom bar. It does not join the diagonal. Its left end is a slant,
	// and the black channel beside the accent continues between that slant
	// and the foot.
	bottomTop     = 693
	bottomBottom  = 806
	bottomRight   = 767
	bottomTopLeft = 408
	bottomBotLeft = 316

	// Accent stroke, on the lower-right of the diagonal. The ends are
	// horizontal. Both long edges share one slope, so the stroke width is
	// constant; accentRun is how far the stroke shifts left from its top
	// to its bottom. It starts just below the top bar and stops above the
	// bottom bar.
	accentTop      = 314
	accentBottom   = 666
	accentTopLeft  = 725
	accentTopRight = 769
	accentRun      = 295
)

func main() {
	root, err := moduleRoot()
	if err != nil {
		fail(err)
	}
	dir := filepath.Join(root, "build", "icon")
	for _, size := range []int{16, 32} {
		name := fmt.Sprintf("makez-%d.svg", size)
		if err = os.WriteFile(filepath.Join(dir, name), svg(size), 0o644); err != nil {
			fail(err)
		}
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

// svg draws the black square and the three white pieces: the top bar with
// the diagonal and its foot, the accent, and the bottom bar.
func svg(size int) []byte {
	mark := fmt.Sprintf(
		"M%d %dH%dL%d %dV%dL%d %dH%dZ M%d %dH%dL%d %dH%dZ M%d %dH%dV%dH%dZ",
		topLeft, topY, topPointX, footX, footBottom, footTop, meetX, topBottom, topLeft,
		accentTopLeft, accentTop, accentTopRight, accentTopRight-accentRun, accentBottom, accentTopLeft-accentRun,
		bottomTopLeft, bottomTop, bottomRight, bottomBottom, bottomBotLeft,
	)
	return []byte(fmt.Sprintf(
		"<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"%d\" height=\"%d\" viewBox=\"0 0 %d %d\">\n"+
			"<rect width=\"%d\" height=\"%d\" fill=\"#000\"/>\n"+
			"<path fill=\"#fff\" d=\"%s\"/>\n"+
			"</svg>\n",
		size, size, view, view, view, view, mark,
	))
}

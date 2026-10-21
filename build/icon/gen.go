// Command icon writes the makez icon as one SVG.
//
// makez.svg is the approved mark scaled uniformly by 1.22, with its
// bounding box centered on the 1024 canvas. The file sets only a viewBox,
// so it scales to its container. The tile is a full black square; the
// mark is white.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// view is the shared coordinate space. The constants below are the
	// integer pixel positions of the mark after the 1.22 scale and centering.
	view = 1024

	// Top bar. The right end is the sharp top of the diagonal.
	topLeft   = 216
	topY      = 143
	topBottom = 272
	topPointX = 829

	// The diagonal's upper-left edge runs from the underside of the top
	// bar to the top of the vertical foot.
	meetX   = 570
	footX   = 195
	footTop = 736

	// The southeast edge is the straight cut from the top point to the
	// bottom of the foot, where it meets the vertical edge.
	footBottom = 881

	// Bottom bar. It does not join the diagonal. Its left end is a slant,
	// and the black channel beside the accent continues between that slant
	// and the foot.
	bottomTop     = 740
	bottomBottom  = 877
	bottomRight   = 814
	bottomTopLeft = 376
	bottomBotLeft = 264

	// Accent stroke, on the lower-right of the diagonal. The ends are
	// horizontal. Both long edges share one slope, so the stroke width is
	// constant; accentRun is how far the stroke shifts left from its top
	// to its bottom. It starts just below the top bar and stops above the
	// bottom bar.
	accentTop      = 277
	accentBottom   = 707
	accentTopLeft  = 763
	accentTopRight = 816
	accentRun      = 360
)

func main() {
	root, err := moduleRoot()
	if err != nil {
		fail(err)
	}
	path := filepath.Join(root, "build", "icon", "makez.svg")
	if err = os.WriteFile(path, svg(), 0o644); err != nil {
		fail(err)
	}
	fmt.Println(filepath.Join("build", "icon", "makez.svg"))
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
func svg() []byte {
	mark := fmt.Sprintf(
		"M%d %dH%dL%d %dV%dL%d %dH%dZ M%d %dH%dL%d %dH%dZ M%d %dH%dV%dH%dZ",
		topLeft, topY, topPointX, footX, footBottom, footTop, meetX, topBottom, topLeft,
		accentTopLeft, accentTop, accentTopRight, accentTopRight-accentRun, accentBottom, accentTopLeft-accentRun,
		bottomTopLeft, bottomTop, bottomRight, bottomBottom, bottomBotLeft,
	)
	return []byte(fmt.Sprintf(
		"<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 %d %d\">\n"+
			"<rect width=\"%d\" height=\"%d\" fill=\"#000\"/>\n"+
			"<path fill=\"#fff\" d=\"%s\"/>\n"+
			"</svg>\n",
		view, view, view, view, mark,
	))
}

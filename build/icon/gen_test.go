package main

import (
	"strings"
	"testing"
)

func TestIconVariants(t *testing.T) {
	const path = "M216 143H829L195 881V736L570 272H216Z M763 277H816L456 707H403Z M376 740H814V877H264Z"
	if got := mark(); got != path {
		t.Fatalf("mark geometry %s", got)
	}

	transparent := string(markSVG())
	app := string(appSVG())
	if strings.Contains(transparent, "<rect") {
		t.Fatalf("mark.svg has a plate: %s", transparent)
	}
	if !strings.Contains(transparent, `fill="currentColor"`) || !strings.Contains(transparent, path) {
		t.Fatalf("mark.svg is not a currentColor Z: %s", transparent)
	}
	if !strings.Contains(app, `<rect width="1024" height="1024" fill="#000"/>`) ||
		!strings.Contains(app, `fill="#fff"`) ||
		!strings.Contains(app, path) {
		t.Fatalf("app.svg is not a black tile with a white Z: %s", app)
	}
}

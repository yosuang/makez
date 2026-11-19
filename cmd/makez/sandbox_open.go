package main

import (
	"os"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/sandbox"
)

// openSandboxProvider is the production sandbox backend. Its defaults are the
// local OpenSandbox URL, installation, and runtime image. An API key and
// control key still come from the environment when the caller set them.
func openSandboxProvider() (core.SandboxProvider, error) {
	return sandbox.New(sandbox.Config{
		BaseURL:        env("MAKEZ_SANDBOX_URL", "http://127.0.0.1:18080"),
		APIKey:         os.Getenv("MAKEZ_SANDBOX_API_KEY"),
		InstallationID: env("MAKEZ_INSTALLATION_ID", "makez-local"),
		ControlKey:     os.Getenv("MAKEZ_CONTROL_KEY"),
		Image:          env("MAKEZ_RUNTIME_IMAGE", "makez-runtime:0.1"),
	})
}

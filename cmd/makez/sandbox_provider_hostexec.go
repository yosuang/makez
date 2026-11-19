//go:build makezhostexec

package main

import (
	"os"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/hostexec"
)

// sandboxProvider returns the host process fake when MAKEZ_TEST_HOST_EXEC is
// set. This file is excluded from the default build by the makezhostexec
// constraint. An empty value keeps OpenSandbox. A set but unusable directory
// is an error, so the process does not fall through to a sandbox API.
func sandboxProvider() (core.SandboxProvider, error) {
	dir := os.Getenv("MAKEZ_TEST_HOST_EXEC")
	if dir == "" {
		return openSandboxProvider()
	}
	return hostexec.Open(dir)
}

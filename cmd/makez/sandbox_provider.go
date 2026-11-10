//go:build !makezhostexec

package main

import "github.com/makez-dev/makez/internal/core"

// sandboxProvider returns OpenSandbox. This file is the default build.
// MAKEZ_TEST_HOST_EXEC is not read. The makezhostexec tag replaces this file
// and is how a test binary selects the host process fake. See go help buildconstraint.
func sandboxProvider() (core.SandboxProvider, error) {
	return openSandboxProvider()
}

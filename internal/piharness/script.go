package piharness

import _ "embed"

//go:embed run.py
var runnerSource string

// RunnerScript is the process Makez starts inside the sandbox. It is not Pi.
func RunnerScript() []byte { return []byte(runnerSource) }

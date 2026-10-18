//go:build !linux

package supervisor

import (
	"errors"
	"os/exec"
	"syscall"
)

func startAgent(*exec.Cmd) error        { return errors.New("sandbox execution requires Linux") }
func agentProcesses() ([]int, error)    { return nil, errors.New("sandbox execution requires Linux") }
func signalAgents(syscall.Signal) error { return errors.New("sandbox execution requires Linux") }

package supervisor

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/makez-dev/makez/internal/core"
)

func validateLaunch(r launchRequest) error {
	if len(r.Command.Argv) == 0 || len(r.Command.Argv) > 128 {
		return invalid("invalid argv")
	}
	for _, arg := range r.Command.Argv {
		if strings.ContainsRune(arg, 0) || len(arg) > 65536 {
			return invalid("invalid argv")
		}
	}
	if r.Command.Workdir != "" && r.Command.Workdir != "/workspace" {
		return invalid("runtime workdir must be /workspace")
	}
	for name, value := range r.Command.Env {
		if !validEnv(name, value) {
			return invalid("invalid command environment")
		}
	}
	if len(r.SecretRefs) != len(r.SecretValues) || len(r.SecretRefs) > 32 {
		return invalid("secret references require matching resolved values")
	}
	total := 0
	for name, ref := range r.SecretRefs {
		value, exists := r.SecretValues[name]
		_, collision := r.Command.Env[name]
		if !exists || collision || ref == "" || value == "" || !validEnv(name, value) || !utf8.ValidString(value) || len(value) > 65536 {
			return invalid("invalid secret injection mapping")
		}
		total += len(value)
	}
	if total > 128<<10 {
		return invalid("secret injection exceeds size limit")
	}
	return nil
}

func (s *Supervisor) Start(r launchRequest) (core.ProcessObservation, error) {
	id, err := identity(r.Key)
	if err != nil {
		return core.ProcessObservation{}, err
	}
	if err = validateLaunch(r); err != nil {
		return core.ProcessObservation{}, err
	}
	literals, err := secretLiterals(r.SecretValues)
	if err != nil {
		return core.ProcessObservation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := s.load(id)
	if err != nil {
		return core.ProcessObservation{}, err
	}
	requestHash := digest(r.StartRequest)
	if previous != nil {
		if previous.Closed {
			return core.ProcessObservation{}, &fault{core.Conflict, "launch key is closed"}
		}
		if previous.Hash != requestHash {
			return core.ProcessObservation{}, &fault{core.Conflict, "launch key request differs"}
		}
		return s.observation(id, previous), nil
	}
	remaining := time.Until(r.Deadline)
	if remaining <= 0 || remaining > 2*time.Hour {
		return core.ProcessObservation{}, invalid("execution deadline must be within two hours")
	}
	state := &receipt{State: "STARTING", Key: r.Key, Hash: requestHash, HasSecrets: len(r.SecretValues) > 0}
	if err = s.save(id, state); err != nil {
		return core.ProcessObservation{}, err
	}
	env := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/home/agent", "LANG": "C.UTF-8", "PYTHONUNBUFFERED": "1"}
	for k, v := range r.Command.Env {
		env[k] = v
	}
	for k, v := range r.SecretValues {
		env[k] = v
	}
	cmd := exec.Command(r.Command.Argv[0], r.Command.Argv[1:]...)
	cmd.Dir = s.workspace
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return core.ProcessObservation{}, err
	}
	defer stdoutWriter.Close()
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		stdout.Close()
		return core.ProcessObservation{}, err
	}
	defer stderrWriter.Close()
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	s.secrets[id] = literals
	if err = startAgent(cmd); err != nil {
		stdout.Close()
		stderr.Close()
		state.State, state.Closed = "STOPPED", true
		if err = s.save(id, state); err != nil {
			return core.ProcessObservation{}, err
		}
		return core.ProcessObservation{}, invalid("executable could not be started")
	}
	s.live[id] = cmd
	state.State, state.Started, state.Readers = "RUNNING", true, 2
	err = s.save(id, state)
	go s.readOutput(id, "stdout", stdout, literals)
	go s.readOutput(id, "stderr", stderr, literals)
	go s.monitor(id, cmd)
	time.AfterFunc(remaining, func() { _, _ = s.Stop(r.Key, "", 0) })
	if err != nil {
		return core.ProcessObservation{}, &fault{core.OutcomeUnknown, "process started but receipt persistence failed"}
	}
	return s.observation(id, state), nil
}

func (s *Supervisor) monitor(id string, cmd *exec.Cmd) {
	err := cmd.Wait()
	exit := &core.ProcessExit{FinishedAt: time.Now().UTC()}
	if err != nil {
		exit.Code = 1
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			exit.Code = ee.ExitCode()
			if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				exit.Code = 128 + int(status.Signal())
				exit.Signal = signalName(status.Signal())
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load(id)
	if err != nil || state == nil {
		return
	}
	state.Exit = exit
	if !state.Closed {
		state.State = "EXITED"
	}
	_ = s.save(id, state)
}

func stopSignal(name string) (syscall.Signal, error) {
	switch name {
	case "", "TERM", "SIGTERM":
		return syscall.SIGTERM, nil
	case "INT", "SIGINT":
		return syscall.SIGINT, nil
	case "KILL", "SIGKILL":
		return syscall.SIGKILL, nil
	default:
		return 0, invalid("unsupported stop signal")
	}
}

func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGKILL:
		return "SIGKILL"
	default:
		return signal.String()
	}
}

func (s *Supervisor) Stop(key core.OperationKey, name string, grace float64) (core.ProcessObservation, error) {
	id, err := identity(key)
	if err != nil {
		return core.ProcessObservation{}, err
	}
	signal, err := stopSignal(name)
	if err != nil || math.IsNaN(grace) || math.IsInf(grace, 0) {
		return core.ProcessObservation{}, invalid("invalid stop policy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load(id)
	if err != nil {
		return core.ProcessObservation{}, err
	}
	if state == nil {
		state = &receipt{Key: key, State: "NOT_STARTED"}
	}
	state.Closed = true
	if err = s.save(id, state); err != nil {
		return core.ProcessObservation{}, err
	}
	if err = signalAgents(signal); err != nil {
		return core.ProcessObservation{}, err
	}
	until := time.Now().Add(time.Duration(min(15, max(0, grace)) * float64(time.Second)))
	for {
		pids, err := agentProcesses()
		if err != nil {
			return core.ProcessObservation{}, err
		}
		if len(pids) == 0 || time.Now().After(until) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	until = time.Now().Add(3 * time.Second)
	for {
		pids, err := agentProcesses()
		if err != nil {
			return core.ProcessObservation{}, err
		}
		if len(pids) == 0 {
			break
		}
		if time.Now().After(until) {
			return core.ProcessObservation{}, &fault{core.OutcomeUnknown, "agent processes did not stop"}
		}
		if err = signalAgents(syscall.SIGKILL); err != nil {
			return core.ProcessObservation{}, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	state.State = "STOPPED"
	if err = s.save(id, state); err != nil {
		return core.ProcessObservation{}, err
	}
	return s.observation(id, state), nil
}

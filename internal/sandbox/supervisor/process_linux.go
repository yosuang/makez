package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func startAgent(cmd *exec.Cmd) error {
	if os.Geteuid() != 0 {
		return errors.New("supervisor requires root")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: &syscall.Credential{Uid: agentUID, Gid: agentUID, Groups: []uint32{}}}
	// no_new_privs must be set on the same OS thread that forks the child.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return cmd.Start()
}

func agentProcesses() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != agentUID {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 || len(data) <= end+2 {
			return nil, errors.New("invalid process status")
		}
		if data[end+2] != 'Z' {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func signalAgents(signal syscall.Signal) error {
	pids, err := agentProcesses()
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err = syscall.Kill(pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

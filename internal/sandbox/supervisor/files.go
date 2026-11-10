package supervisor

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"

	"github.com/makez-dev/makez/internal/core"
)

// releaseSentinel is created after the agent process exists. The runner waits
// for this new file before it sends a prompt. It is not an input the process
// has already read. The path matches piharness.ReleasePath; the supervisor does
// not import that package.
const releaseSentinel = "/workspace/pi-release"

// listAgentProcesses is the live uid 1000 scan. Tests replace it.
var listAgentProcesses = agentProcesses

// chownAgent gives the workspace file to the agent user. Tests replace it.
var chownAgent = func(file *os.File) error {
	return file.Chown(agentUID, agentUID)
}

func (s *Supervisor) parent(path string, create bool) (*os.Root, string, error) {
	path = strings.TrimPrefix(path, "/workspace/")
	parts := strings.Split(path, "/")
	if len(path) > 4096 {
		return nil, "", invalid("invalid file path")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return nil, "", invalid("file must be within /workspace")
		}
	}
	root, err := os.OpenRoot(s.workspace)
	if err != nil {
		return nil, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		if create {
			if err = root.Mkdir(part, 0755); err == nil {
				err = root.Lchown(part, agentUID, agentUID)
			} else if errors.Is(err, os.ErrExist) {
				err = nil
			}
			if err != nil {
				root.Close() //nolint:errcheck // Release a directory descriptor; traversal errors are handled separately.
				return nil, "", err
			}
		}
		// A descriptor opened with NOFOLLOW preserves the original no-symlink boundary.
		file, err := root.OpenFile(part, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			root.Close() //nolint:errcheck // Release a directory descriptor; traversal errors are handled separately.
			return nil, "", err
		}
		next, err := root.OpenRoot(part)
		if err == nil {
			before, e1 := file.Stat()
			after, e2 := next.Stat(".")
			if e1 != nil || e2 != nil || !os.SameFile(before, after) {
				next.Close() //nolint:errcheck // Discard the directory descriptor after validation failed.
				err = invalid("workspace directory changed")
			}
		}
		file.Close() //nolint:errcheck // Release a read-only descriptor; read and metadata errors are handled separately.
		root.Close() //nolint:errcheck // Release a directory descriptor; traversal errors are handled separately.
		if err != nil {
			return nil, "", err
		}
		root = next
	}
	return root, parts[len(parts)-1], nil
}

func (s *Supervisor) Read(path string) ([]byte, error) {
	root, name, err := s.parent(path, false)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // The directory handle owns no buffered file data.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // The workspace file is read-only; read errors are returned below.
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, invalid("artifact must be a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxFile {
		return nil, invalid("file exceeds 16 MiB")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secretContextLost {
		return nil, &fault{core.PermissionDenied, "secret inspection context is unavailable"}
	}
	for _, values := range s.secrets {
		for _, value := range values {
			if bytes.Contains(content, value) {
				return nil, &fault{core.PermissionDenied, "workspace file contains an injected secret"}
			}
		}
	}
	return content, nil
}

func (s *Supervisor) Write(path string, content []byte) error {
	if len(content) > maxFile {
		return invalid("file exceeds 16 MiB")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pids, err := listAgentProcesses()
	if err != nil {
		return err
	}
	// The runner is already uid 1000 and waits for a file that does not exist yet.
	// Creating that one file is the delivery handshake. Replacing any existing
	// workspace entry, including a second write of the sentinel, stays rejected.
	createOnly := false
	if len(pids) > 0 {
		exists, existsErr := workspaceEntryExists(s, path)
		if existsErr != nil {
			return existsErr
		}
		if path != releaseSentinel || exists {
			return &fault{core.Conflict, "input files cannot be replaced while an Agent runs"}
		}
		createOnly = true
	}
	root, name, err := s.parent(path, true)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // The directory handle owns no buffered file data.
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	temporary := ".makez-" + hex.EncodeToString(random[:])
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("cannot remove temporary workspace file")
		}
	}()
	_, err = file.Write(content)
	if err == nil {
		err = chownAgent(file)
	}
	if err == nil {
		err = file.Chmod(0644)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if createOnly {
		err = renameExclusive(root, temporary, name)
		if errors.Is(err, os.ErrExist) {
			return &fault{core.Conflict, "input files cannot be replaced while an Agent runs"}
		}
	} else {
		err = root.Rename(temporary, name)
	}
	if err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // The directory sync result is returned separately.
	return dir.Sync()
}

func workspaceEntryExists(s *Supervisor, path string) (bool, error) {
	root, name, err := s.parent(path, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer root.Close() //nolint:errcheck // The directory handle owns no buffered file data.
	_, err = root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Package artifact stores immutable results outside the sandbox lifetime.
package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/makez-dev/makez/internal/core"
)

// Limits on individual files and the combined artifacts of a run.
const (
	MaxFileBytes  = 16 << 20
	MaxTotalBytes = 64 << 20
	MaxFiles      = 64
)

// Record describes an immutable file archived from a run.
type Record struct {
	LogicalPath string
	StorageKey  string
	SHA256      string
	MediaType   string
	Size        int64
	Required    bool
}

// Store confines artifact access to an opened storage root.
type Store struct{ root *os.Root }

// New opens or creates the artifact storage directory.
func New(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &Store{root: r}, nil
}

// Close releases the storage root descriptor.
func (s *Store) Close() error { return s.root.Close() }

// ValidatePath accepts relative result paths or explicit /workspace paths.
// Remote readers must additionally reject symlink escapes within the sandbox.
func ValidatePath(value string) (string, error) {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", fmt.Errorf("invalid artifact path")
	}
	value = strings.TrimPrefix(value, "/workspace/")
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("artifact path must be inside /workspace")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." || part == "" {
			return "", fmt.Errorf("artifact path contains an unsafe component")
		}
	}
	return "/workspace/" + value, nil
}

func validRunID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// Open opens a validated artifact storage key for reading.
func (s *Store) Open(key string) (io.ReadCloser, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 || !validRunID(parts[0]) || len(parts[1]) != 64 {
		return nil, fmt.Errorf("invalid artifact storage key")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return nil, fmt.Errorf("invalid artifact digest: %w", err)
	}
	f, err := s.root.Open(key)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		f.Close() //nolint:errcheck // Discard the invalid read-only artifact without replacing its validation error.
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("invalid stored artifact")
	}
	return f, nil
}

// Archive copies candidate files into immutable storage within size limits.
func (s *Store) Archive(ctx context.Context, runID string, reader core.WorkspaceReader, candidates []core.ArtifactCandidate) ([]Record, error) {
	if !validRunID(runID) {
		return nil, fmt.Errorf("invalid run ID")
	}
	if len(candidates) > MaxFiles {
		return nil, fmt.Errorf("too many artifacts (maximum %d)", MaxFiles)
	}
	if err := s.root.MkdirAll(runID, 0700); err != nil {
		return nil, err
	}
	result := make([]Record, 0, len(candidates))
	seen := map[string]bool{}
	var total int64
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, err := ValidatePath(candidate.Path)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate artifact path %q", name)
		}
		seen[name] = true
		input, err := reader.Read(ctx, name)
		if err != nil {
			if !candidate.Required && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read artifact %q: %w", name, err)
		}
		record, copyErr := s.archiveOne(ctx, runID, name, input, min(MaxFileBytes, MaxTotalBytes-total))
		closeErr := input.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		record.MediaType, record.Required = candidate.MediaType, candidate.Required
		if record.MediaType == "" {
			record.MediaType = "application/octet-stream"
		}
		total += record.Size
		result = append(result, record)
	}
	return result, nil
}

func (s *Store) archiveOne(ctx context.Context, runID, logicalPath string, input io.Reader, limit int64) (Record, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Record{}, err
	}
	temp := path.Join(runID, ".tmp-"+hex.EncodeToString(nonce[:]))
	f, err := s.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Record{}, err
	}
	defer func() {
		if err := s.root.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cannot remove temporary artifact file")
		}
	}()
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextReader{ctx, input}, limit+1))
	if copyErr == nil && n > limit {
		copyErr = fmt.Errorf("artifact size limit exceeded: %q", logicalPath)
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		return Record{}, copyErr
	}
	if closeErr != nil {
		return Record{}, closeErr
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	key := path.Join(runID, digest)
	if err := s.root.Rename(temp, key); err != nil {
		return Record{}, err
	}
	directory, err := s.root.Open(runID)
	if err != nil {
		return Record{}, err
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err != nil {
		return Record{}, err
	}
	if closeErr != nil {
		return Record{}, closeErr
	}
	return Record{LogicalPath: logicalPath, StorageKey: key, SHA256: digest, Size: n}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

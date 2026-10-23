// Package secret resolves server-registered credentials only at execution time.
package secret

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Secret access errors that never include credential values.
var (
	ErrDenied      = errors.New("secret reference denied")
	ErrUnavailable = errors.New("secret unavailable")
	ErrInvalid     = errors.New("invalid secret configuration")
)

const (
	maxReferences = 16
	maxFileBytes  = 64 << 10
	maxValueBytes = 32 << 10
	maxTotalBytes = 64 << 10
)

var (
	refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}@[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// Source contains locations, never credential values. Only server configuration
// may register a source; an AgentAdapter binds an existing logical reference.
type Source struct {
	Env       string   `json:"env,omitempty"`
	File      string   `json:"file,omitempty"`
	JSONField []string `json:"json_field,omitempty"`
}

// Registry maps workspace IDs to versioned logical references such as api-key@1.
type Registry map[string]map[string]Source

// Resolver authorizes and resolves credentials from a private registry copy.
type Resolver struct{ registry Registry }

// New validates and copies the server’s secret registry.
func New(registry Registry) (*Resolver, error) {
	copied := make(Registry, len(registry))
	for workspace, sources := range registry {
		if workspace == "" || len(workspace) > 128 || strings.ContainsAny(workspace, "\x00\r\n") {
			return nil, ErrInvalid
		}
		copied[workspace] = maps.Clone(sources)
		for ref, source := range sources {
			if !refPattern.MatchString(ref) || (source.Env == "") == (source.File == "") || len(source.JSONField) > 16 {
				return nil, ErrInvalid
			}
			if source.Env != "" && (!envPattern.MatchString(source.Env) || len(source.JSONField) != 0) {
				return nil, ErrInvalid
			}
			if source.File != "" && (!filepath.IsAbs(source.File) || filepath.Clean(source.File) != source.File || strings.ContainsRune(source.File, 0)) {
				return nil, ErrInvalid
			}
			for _, field := range source.JSONField {
				if field == "" || len(field) > 256 || strings.ContainsRune(field, 0) {
					return nil, ErrInvalid
				}
			}
			source.JSONField = slices.Clone(source.JSONField)
			copied[workspace][ref] = source
		}
	}
	return &Resolver{registry: copied}, nil
}

// Check authorizes destination environment names and references without reading
// the registered source. Empty bindings require no secret permissions.
func (r *Resolver) Check(workspace string, refs map[string]string) error {
	if len(refs) > maxReferences {
		return ErrInvalid
	}
	for env, ref := range refs {
		if !allowedDestination(env) || !refPattern.MatchString(ref) {
			return ErrInvalid
		}
		if _, exists := r.registry[workspace][ref]; !exists {
			return ErrDenied
		}
	}
	return nil
}

func allowedDestination(name string) bool {
	if !envPattern.MatchString(name) || strings.HasPrefix(name, "MAKEZ_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return false
	}
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP", "IFS", "ENV", "BASH_ENV", "CDPATH", "SHELLOPTS", "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "NODE_OPTIONS", "NODE_PATH", "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "RUBYOPT", "RUBYLIB", "PERL5OPT", "PERL5LIB", "GODEBUG", "GOTOOLCHAIN", "GOPATH", "GOMODCACHE":
		return false
	}
	return true
}

// Resolve re-reads sources on every call so host-side rotation takes effect.
// The returned values must stay Activity-local and must never be logged or sent
// to Temporal. This module does not prevent an Agent from disclosing its inputs.
func (r *Resolver) Resolve(ctx context.Context, workspace string, refs map[string]string) (map[string]string, error) {
	if err := r.Check(workspace, refs); err != nil {
		return nil, err
	}
	resolved := make(map[string]string, len(refs))
	cache := make(map[string]string, len(refs))
	total := 0
	for env, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(ErrUnavailable, err)
		}
		value, found := cache[ref]
		if !found {
			var err error
			value, err = readSource(r.registry[workspace][ref])
			if err != nil {
				return nil, err
			}
			if value == "" || strings.TrimSpace(value) == "" || len(value) > maxValueBytes || strings.ContainsRune(value, 0) {
				return nil, ErrUnavailable
			}
			cache[ref] = value
		}
		total += len(value)
		if total > maxTotalBytes {
			return nil, ErrUnavailable
		}
		resolved[env] = value
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	return resolved, nil
}

func readSource(source Source) (string, error) {
	if source.Env != "" {
		value, exists := os.LookupEnv(source.Env)
		if !exists {
			return "", ErrUnavailable
		}
		return value, nil
	}
	// Root confines replacement races to the server-configured parent directory.
	root, err := os.OpenRoot(filepath.Dir(source.File))
	if err != nil {
		return "", ErrUnavailable
	}
	defer root.Close() //nolint:errcheck // Release the read-only secret directory handle.
	name := filepath.Base(source.File)
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > maxFileBytes {
		return "", ErrUnavailable
	}
	file, err := root.Open(name)
	if err != nil {
		return "", ErrUnavailable
	}
	defer file.Close() //nolint:errcheck // The secret file is read-only; read errors are handled below.
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0077 != 0 || after.Size() > maxFileBytes {
		return "", ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil || len(data) > maxFileBytes {
		return "", ErrUnavailable
	}
	defer clear(data)
	if len(source.JSONField) == 0 {
		return string(data), nil
	}
	var current json.RawMessage = data
	for _, field := range source.JSONField {
		var object map[string]json.RawMessage
		if json.Unmarshal(current, &object) != nil {
			return "", ErrUnavailable
		}
		var found bool
		current, found = object[field]
		if !found {
			return "", ErrUnavailable
		}
	}
	var value string
	if json.Unmarshal(current, &value) != nil {
		return "", ErrUnavailable
	}
	return value, nil
}

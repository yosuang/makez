// Package supervisor implements the sandbox's authenticated execution protocol.
// It has no dependency on an Agent implementation or sandbox backend.
package supervisor

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

const (
	maxFile   = 16 << 20
	maxOutput = 64 << 20
	agentUID  = 1000
)

type fault struct {
	Code    core.FailureCode `json:"code"`
	Message string           `json:"error"`
}

func (e *fault) Error() string     { return e.Message }
func invalid(message string) error { return &fault{core.InvalidInput, message} }

type receipt struct {
	State      string
	Key        core.OperationKey
	Hash       string
	Closed     bool
	Started    bool
	Readers    int
	HasSecrets bool
	OutputGap  bool
	Exit       *core.ProcessExit
}

// Supervisor owns process receipts, output logs, and workspace access.
type Supervisor struct {
	control           string
	workspace         string
	mu                sync.Mutex
	logMu             sync.Mutex
	live              map[string]*exec.Cmd
	secrets           map[string][][]byte
	secretContextLost bool
}

// New opens supervisor state and detects lost secret context after a restart.
func New(control, workspace string) (*Supervisor, error) {
	if err := os.MkdirAll(control, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(control, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workspace, 0755); err != nil {
		return nil, err
	}
	s := &Supervisor{control: control, workspace: workspace, live: map[string]*exec.Cmd{}, secrets: map[string][][]byte{}}
	files, err := os.ReadDir(control)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		state, err := s.load(strings.TrimSuffix(file.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if state != nil && state.HasSecrets {
			s.secretContextLost = true
		}
	}
	return s, nil
}

func digest(value any) string {
	data, _ := json.Marshal(value)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func identity(key core.OperationKey) (string, error) {
	if key.WorkspaceID == "" || key.TaskID == "" || key.RunID == "" || key.Step == "" {
		return "", invalid("invalid operation key")
	}
	return digest(key), nil
}

func (s *Supervisor) load(id string) (*receipt, error) {
	data, err := os.ReadFile(filepath.Join(s.control, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state receipt
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *Supervisor) save(id string, state *receipt) error {
	name := filepath.Join(s.control, id+".json")
	f, err := os.OpenFile(name+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(state)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name+".tmp", name); err != nil {
		return err
	}
	dir, err := os.Open(s.control)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // The directory sync result is returned separately.
	return dir.Sync()
}

func (s *Supervisor) observation(id string, state *receipt) core.ProcessObservation {
	o := core.ProcessObservation{State: core.ProcessNotStarted, ObservedAt: time.Now().UTC()}
	if state == nil {
		return o
	}
	o.State = core.ProcessState(state.State)
	if (state.State == "RUNNING" || state.State == "STARTING") && s.live[id] == nil {
		o.State = core.ProcessUnknown
	}
	if state.Started {
		o.Ref = &core.ProcessRef{ID: id, Key: state.Key}
	}
	o.Exit = state.Exit
	return o
}

// Process returns the current receipt for a keyed execution.
func (s *Supervisor) Process(key core.OperationKey) (core.ProcessObservation, error) {
	id, err := identity(key)
	if err != nil {
		return core.ProcessObservation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load(id)
	if err != nil {
		return core.ProcessObservation{}, err
	}
	return s.observation(id, state), nil
}

type launchRequest struct {
	core.StartRequest
	SecretValues map[string]string
}

type request struct {
	launchRequest
	Cursor       string
	MaxBytes     int
	GraceSeconds float64
	Signal       string
	Path         string
	Data         []byte
}

// Handler serves authenticated control operations.
func (s *Supervisor) Handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if len(token) < 32 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Makez-Control-Token")), []byte(token)) != 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 24<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input request
		var result any
		var err error
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
			err = invalid("invalid runtime request")
		} else {
			switch r.URL.Path {
			case "/start":
				result, err = s.Start(input.launchRequest)
			case "/process":
				result, err = s.Process(input.Key)
			case "/stop":
				result, err = s.Stop(input.Key, input.Signal, input.GraceSeconds)
			case "/output":
				result, err = s.Output(input.Key, input.Cursor, input.MaxBytes)
			case "/read":
				var data []byte
				data, err = s.Read(input.Path)
				result = struct{ Data []byte }{data}
			case "/write":
				err = s.Write(input.Path, input.Data)
				result = struct{}{}
			default:
				err = invalid("unknown runtime operation")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			f, ok := errors.AsType[*fault](err)
			if !ok {
				f = &fault{core.InvalidInput, "invalid request or unavailable workspace file"}
				if errors.Is(err, os.ErrNotExist) {
					f = &fault{core.ResourceLost, "workspace file does not exist"}
				}
			}
			_ = json.NewEncoder(w).Encode(f)
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Data any `json:"data"`
		}{result})
	})
}

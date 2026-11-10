package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/store/dbgen"
)

// ErrAttemptsExhausted means this run has already used its process generations.
var ErrAttemptsExhausted = errors.New("attempt limit reached")

// SessionIdentity selects one logical native session. It is not a conversation,
// an owner binding, or a retention policy.
type SessionIdentity struct {
	Scope           string
	BotID           string
	ConversationID  string
	DelegationRunID string
	HarnessID       string
	HarnessPin      string
}

// NativeSession is the durable correlation for one harness pin.
type NativeSession struct {
	ID              string
	Disposition     string
	PromptSHA256    string
	CheckpointBytes int64
}

// Attempt is one process generation of a run.
type Attempt struct {
	SessionID    string
	Generation   int64
	Step         string
	State        string
	Disposition  string
	PromptSHA256 string
}

var (
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pinHex       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	harnessID    = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	dispositions = map[string]bool{"none": true, "sending": true, "accepted": true, "settled": true, "rejected": true, "reconciled": true}
)

// EnsureNativeSession returns the session for this identity, creating it when needed.
func (s *Store) EnsureNativeSession(ctx context.Context, workspace string, id SessionIdentity) (NativeSession, error) {
	if !harnessID.MatchString(id.HarnessID) || !pinHex.MatchString(id.HarnessPin) {
		return NativeSession{}, ErrInvalidInput
	}
	switch id.Scope {
	case "personal":
		if !sessionLabel(id.BotID) || !sessionLabel(id.ConversationID) || id.DelegationRunID != "" {
			return NativeSession{}, ErrInvalidInput
		}
		row, err := dbgen.New(s.pool).InsertPersonalSession(ctx, dbgen.InsertPersonalSessionParams{
			ID: newID(), WorkspaceID: workspace, BotID: &id.BotID, ConversationID: &id.ConversationID,
			HarnessID: id.HarnessID, HarnessPin: id.HarnessPin,
		})
		if err != nil {
			return NativeSession{}, err
		}
		return NativeSession{ID: row.ID, Disposition: row.LastDisposition, PromptSHA256: row.LastPromptSha256, CheckpointBytes: row.CheckpointBytes}, nil
	case "worker":
		if !sessionLabel(id.DelegationRunID) || id.BotID != "" || id.ConversationID != "" {
			return NativeSession{}, ErrInvalidInput
		}
		row, err := dbgen.New(s.pool).InsertWorkerSession(ctx, dbgen.InsertWorkerSessionParams{
			ID: newID(), WorkspaceID: workspace, DelegationRunID: &id.DelegationRunID,
			HarnessID: id.HarnessID, HarnessPin: id.HarnessPin,
		})
		if err != nil {
			return NativeSession{}, err
		}
		return NativeSession{ID: row.ID, Disposition: row.LastDisposition, PromptSHA256: row.LastPromptSha256, CheckpointBytes: row.CheckpointBytes}, nil
	default:
		return NativeSession{}, ErrInvalidInput
	}
}

func sessionLabel(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsRune(value, 0) && strings.TrimSpace(value) == value
}

// OpenAttempt returns the current open process generation or the next one after a loss.
func (s *Store) OpenAttempt(ctx context.Context, workspace, runID, sessionID, promptHash string) (Attempt, error) {
	if !sha256Hex.MatchString(promptHash) || sessionID == "" {
		return Attempt{}, ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Attempt{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	q := dbgen.New(tx)
	if _, err = q.LockNativeSession(ctx, dbgen.LockNativeSessionParams{ID: sessionID, WorkspaceID: workspace}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return Attempt{}, err
	}
	latest, err := q.LockLatestAttempt(ctx, dbgen.LockLatestAttemptParams{WorkspaceID: workspace, RunID: runID})
	if errors.Is(err, pgx.ErrNoRows) {
		attempt := Attempt{SessionID: sessionID, Generation: 1, Step: "agent", State: "open", Disposition: "none", PromptSHA256: promptHash}
		if err = insertAttempt(ctx, q, workspace, runID, attempt); err != nil {
			return Attempt{}, err
		}
		return attempt, tx.Commit(ctx)
	}
	if err != nil {
		return Attempt{}, err
	}
	current := Attempt{SessionID: latest.SessionID, Generation: latest.Generation, Step: latest.Step, State: latest.State, Disposition: latest.Disposition, PromptSHA256: latest.PromptSha256}
	if current.State == "open" {
		if current.SessionID != sessionID || current.PromptSHA256 != promptHash {
			return Attempt{}, ErrConflict
		}
		return current, tx.Commit(ctx)
	}
	if current.State != "lost" {
		return Attempt{}, ErrConflict
	}
	if current.Generation >= 3 {
		return Attempt{}, ErrAttemptsExhausted
	}
	next := Attempt{SessionID: sessionID, Generation: current.Generation + 1, Step: fmt.Sprintf("agent:%d", current.Generation+1), State: "open", Disposition: "none", PromptSHA256: promptHash}
	if err = insertAttempt(ctx, q, workspace, runID, next); err != nil {
		return Attempt{}, err
	}
	return next, tx.Commit(ctx)
}

func insertAttempt(ctx context.Context, q *dbgen.Queries, workspace, runID string, attempt Attempt) error {
	rows, err := q.InsertAttempt(ctx, dbgen.InsertAttemptParams{
		WorkspaceID: workspace, RunID: runID, SessionID: attempt.SessionID,
		Generation: attempt.Generation, Step: attempt.Step, PromptSha256: attempt.PromptSHA256,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrConflict
	}
	return nil
}

// LoadCheckpoint returns a saved Pi session file. A missing file is an empty slice.
func (s *Store) LoadCheckpoint(ctx context.Context, workspace, sessionID string) ([]byte, error) {
	data, err := dbgen.New(s.pool).NativeCheckpoint(ctx, dbgen.NativeCheckpointParams{ID: sessionID, WorkspaceID: workspace})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

// SaveCheckpoint stores bytes Pi actually wrote. It does not invent a session file.
func (s *Store) SaveCheckpoint(ctx context.Context, workspace, sessionID string, data []byte) error {
	if len(data) == 0 || len(data) > 1<<20 {
		return ErrInvalidInput
	}
	rows, err := dbgen.New(s.pool).SaveNativeCheckpoint(ctx, dbgen.SaveNativeCheckpointParams{ID: sessionID, WorkspaceID: workspace, Checkpoint: data})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}

// SaveAttemptProgress records how far the native prompt got for this generation.
func (s *Store) SaveAttemptProgress(ctx context.Context, workspace, runID, sessionID string, generation int64, disposition, promptHash, state string) error {
	if !dispositions[disposition] || !sha256Hex.MatchString(promptHash) {
		return ErrInvalidInput
	}
	if state != "" && state != "open" && state != "lost" && state != "closed" {
		return ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	q := dbgen.New(tx)
	rows, err := q.SaveNativeDisposition(ctx, dbgen.SaveNativeDispositionParams{ID: sessionID, WorkspaceID: workspace, LastDisposition: disposition, LastPromptSha256: promptHash})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	rows, err = q.SaveAttemptOutcome(ctx, dbgen.SaveAttemptOutcomeParams{WorkspaceID: workspace, RunID: runID, Generation: generation, Disposition: disposition, State: state})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// LatestAttempt returns the newest generation for a run.
func (s *Store) LatestAttempt(ctx context.Context, workspace, runID string) (Attempt, error) {
	row, err := dbgen.New(s.pool).LockLatestAttempt(ctx, dbgen.LockLatestAttemptParams{WorkspaceID: workspace, RunID: runID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}
	return Attempt{SessionID: row.SessionID, Generation: row.Generation, Step: row.Step, State: row.State, Disposition: row.Disposition, PromptSHA256: row.PromptSha256}, nil
}

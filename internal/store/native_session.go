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

// ErrSessionBusy means another run holds this session's writer lease.
// Callers wait inside the existing task. This is not a public task status.
var ErrSessionBusy = errors.New("native session writer is held")

// ErrStaleWriter means this run is not the current writer, or the attempt
// generation is no longer the latest one. The command row is left unchanged.
var ErrStaleWriter = errors.New("stale native session writer")

// sessionLeaseSeconds is how long a writer keeps the session after the last
// renew. It is longer than the activity heartbeat so a live worker can extend
// it, and short enough that a dead worker does not block the session indefinitely.
const sessionLeaseSeconds int32 = 120

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
// Disposition and PromptSHA256 cache the latest command write. A prompt is sent or held from its own command row.
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
// It acquires this run's writer lease. A different run that already holds an unexpired lease gets ErrSessionBusy.
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
	// The row lease covers every worker. A process-local mutex does not.
	if err = claimWriter(ctx, q, workspace, sessionID, runID); err != nil {
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

// SaveCheckpoint stores bytes Pi actually wrote. The current writer is the only caller that can replace them.
func (s *Store) SaveCheckpoint(ctx context.Context, workspace, sessionID, runID string, data []byte) error {
	if len(data) == 0 || len(data) > 1<<20 || runID == "" {
		return ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	q := dbgen.New(tx)
	if err = requireWriter(ctx, q, workspace, sessionID, runID); err != nil {
		return err
	}
	rows, err := q.SaveNativeCheckpoint(ctx, dbgen.SaveNativeCheckpointParams{ID: sessionID, WorkspaceID: workspace, Checkpoint: data})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// AcquireSessionWriter takes or extends this run's lease. An unexpired lease held by another run returns ErrSessionBusy.
func (s *Store) AcquireSessionWriter(ctx context.Context, workspace, sessionID, runID string) error {
	if sessionID == "" || runID == "" {
		return ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	if err = claimWriter(ctx, dbgen.New(tx), workspace, sessionID, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReleaseSessionWriter clears the lease only when this run still holds it. Command rows stay.
func (s *Store) ReleaseSessionWriter(ctx context.Context, workspace, sessionID, runID string) error {
	if sessionID == "" || runID == "" {
		return ErrInvalidInput
	}
	holder := runID
	return dbgen.New(s.pool).ReleaseSessionWriter(ctx, dbgen.ReleaseSessionWriterParams{
		ID: sessionID, WorkspaceID: workspace, WriterRunID: &holder,
	})
}

// PromptDisposition returns the command row for this prompt. An unknown prompt is an empty string.
func (s *Store) PromptDisposition(ctx context.Context, workspace, sessionID, promptHash string) (string, error) {
	if sessionID == "" || !sha256Hex.MatchString(promptHash) {
		return "", ErrInvalidInput
	}
	row, err := dbgen.New(s.pool).GetNativeCommand(ctx, dbgen.GetNativeCommandParams{
		SessionID: sessionID, WorkspaceID: workspace, PromptSha256: promptHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.Disposition, nil
}

// HasUnknownPrompt reports whether a different prompt is still sending or accepted.
func (s *Store) HasUnknownPrompt(ctx context.Context, workspace, sessionID, promptHash string) (bool, error) {
	if sessionID == "" || !sha256Hex.MatchString(promptHash) {
		return false, ErrInvalidInput
	}
	return dbgen.New(s.pool).HasUnknownNativeCommand(ctx, dbgen.HasUnknownNativeCommandParams{
		SessionID: sessionID, WorkspaceID: workspace, PromptSha256: promptHash,
	})
}

// SaveAttemptProgress records one prompt's disposition for this generation.
// Another prompt's row is not updated. A writer who lost the lease, and an older generation, leave the row unchanged.
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
	if err = requireWriter(ctx, q, workspace, sessionID, runID); err != nil {
		return err
	}
	latest, err := q.LockLatestAttempt(ctx, dbgen.LockLatestAttemptParams{WorkspaceID: workspace, RunID: runID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if latest.SessionID != sessionID {
		return ErrConflict
	}
	if latest.Generation != generation {
		return ErrStaleWriter
	}
	if disposition == "none" {
		return commitAttemptState(ctx, tx, q, workspace, runID, generation, state)
	}
	current, err := q.GetNativeCommand(ctx, dbgen.GetNativeCommandParams{SessionID: sessionID, WorkspaceID: workspace, PromptSha256: promptHash})
	hasRow := true
	if errors.Is(err, pgx.ErrNoRows) {
		hasRow = false
		err = nil
	}
	if err != nil {
		return err
	}
	curDisp, curRun := "", ""
	var curGen int64
	if hasRow {
		curDisp, curRun, curGen = current.Disposition, current.RunID, current.Generation
	}
	if !allowCommand(hasRow, curDisp, disposition, curGen, generation, curRun, runID) {
		return commitAttemptState(ctx, tx, q, workspace, runID, generation, state)
	}
	if err = q.UpsertNativeCommand(ctx, dbgen.UpsertNativeCommandParams{
		SessionID: sessionID, PromptSha256: promptHash, Disposition: disposition,
		WorkspaceID: workspace, RunID: runID, Generation: generation,
	}); err != nil {
		return err
	}
	// last_disposition is a cache of this write. Replay decisions use the command row.
	rows, err := q.SaveNativeDisposition(ctx, dbgen.SaveNativeDispositionParams{
		ID: sessionID, WorkspaceID: workspace, LastDisposition: disposition, LastPromptSha256: promptHash,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	rows, err = q.SaveAttemptOutcome(ctx, dbgen.SaveAttemptOutcomeParams{
		WorkspaceID: workspace, RunID: runID, Generation: generation, Disposition: disposition, State: state,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func claimWriter(ctx context.Context, q *dbgen.Queries, workspace, sessionID, runID string) error {
	owner, err := q.LockSessionOwner(ctx, dbgen.LockSessionOwnerParams{ID: sessionID, WorkspaceID: workspace})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner.WriterCurrent != nil && *owner.WriterCurrent && owner.WriterRunID != nil && *owner.WriterRunID != runID {
		return ErrSessionBusy
	}
	holder := runID
	return q.ClaimSessionWriter(ctx, dbgen.ClaimSessionWriterParams{
		WriterRunID: &holder, LeaseSeconds: sessionLeaseSeconds, ID: sessionID, WorkspaceID: workspace,
	})
}

func requireWriter(ctx context.Context, q *dbgen.Queries, workspace, sessionID, runID string) error {
	owner, err := q.LockSessionOwner(ctx, dbgen.LockSessionOwnerParams{ID: sessionID, WorkspaceID: workspace})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner.WriterCurrent == nil || !*owner.WriterCurrent || owner.WriterRunID == nil || *owner.WriterRunID != runID {
		return ErrStaleWriter
	}
	return nil
}

func commitAttemptState(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, workspace, runID string, generation int64, state string) error {
	if state == "" {
		return nil
	}
	rows, err := q.SaveAttemptState(ctx, dbgen.SaveAttemptStateParams{
		State: state, WorkspaceID: workspace, RunID: runID, Generation: generation,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// allowCommand decides whether this write may change the prompt's command row.
// sending and accepted stay put when a different run observes them.
func allowCommand(hasRow bool, currentDisposition, next string, currentGen, nextGen int64, currentRun, nextRun string) bool {
	if next == "" || next == "none" {
		return false
	}
	if !hasRow {
		return true
	}
	if currentRun != nextRun && (currentDisposition == "sending" || currentDisposition == "accepted") {
		return false
	}
	if nextGen < currentGen {
		return false
	}
	if currentDisposition == next && currentRun == nextRun && currentGen == nextGen {
		return true
	}
	switch currentDisposition {
	case "sending":
		return next == "accepted" || next == "settled" || next == "rejected" || next == "reconciled"
	case "accepted":
		return next == "settled" || next == "rejected" || next == "reconciled"
	case "settled", "rejected", "reconciled":
		if next == "sending" || next == "accepted" {
			return nextRun != currentRun || nextGen > currentGen
		}
		return next == currentDisposition
	default:
		return false
	}
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

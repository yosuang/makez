package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
	"github.com/makez-dev/makez/internal/store"
)

// piLauncher is the host-owned Pi command. The task config cannot change it.
type piLauncher interface {
	PiLaunchSpec() (binary string, forward []string, offline bool)
}

type openedAttempt struct {
	Session store.NativeSession
	Attempt store.Attempt
	Mode    string
}

func (a *Activities) preparePi(ctx context.Context, t store.Task) error {
	if t.AgentID != piharness.AdapterID {
		return nil
	}
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return err
	}
	data, err := a.DB.LoadCheckpoint(ctx, t.WorkspaceID, session.ID)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > piharness.MaxCheckpoint {
		return fail("CHECKPOINT_TOO_LARGE", nil)
	}
	if t.Run == nil || t.Run.Sandbox == nil {
		return fail("RESOURCE_LOST", nil)
	}
	if err = a.Provider.WriteFile(ctx, *t.Run.Sandbox, piharness.CheckpointPath, bytes.NewReader(data), int64(len(data))); err != nil {
		return convert(err)
	}
	return nil
}

func (a *Activities) ensurePiSession(ctx context.Context, t store.Task) (store.NativeSession, error) {
	exec, err := piharness.ParseExecution(t.Config)
	if err != nil {
		return store.NativeSession{}, fail("INVALID_INPUT", err)
	}
	session, err := a.DB.EnsureNativeSession(ctx, t.WorkspaceID, store.SessionIdentity{
		Scope: exec.Scope, BotID: exec.BotID, ConversationID: exec.ConversationID,
		DelegationRunID: exec.DelegationRunID, HarnessID: piharness.AdapterID, HarnessPin: exec.HarnessPin,
	})
	if err != nil {
		return store.NativeSession{}, err
	}
	return session, nil
}

func (a *Activities) openPiAttempt(ctx context.Context, t store.Task) (openedAttempt, error) {
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return openedAttempt{}, err
	}
	attempt, err := a.DB.OpenAttempt(ctx, t.WorkspaceID, t.Run.ID, session.ID, piharness.PromptHash(t.Prompt))
	if errors.Is(err, store.ErrAttemptsExhausted) {
		return openedAttempt{}, fail("ATTEMPT_EXHAUSTED", err)
	}
	if err != nil {
		return openedAttempt{}, err
	}
	action := piharness.Decide(piharness.Disposition(session.Disposition), piharness.SameCommand(session.PromptSHA256, t.Prompt), piharness.HistoryUnread)
	mode := piharness.Mode(action)
	if action.ReadHistory && !piharness.SameCommand(session.PromptSHA256, t.Prompt) {
		// A different prompt is sent only after this process has read history.
		mode = "reconcile_then_send"
	}
	return openedAttempt{Session: session, Attempt: attempt, Mode: mode}, nil
}

func (a *Activities) writePiInstruction(ctx context.Context, t store.Task, opened openedAttempt) error {
	adapter, err := a.adapter(t)
	if err != nil {
		return err
	}
	launcher, ok := adapter.(piLauncher)
	if !ok {
		return fail("INVALID_AGENT", nil)
	}
	binary, forward, offline := launcher.PiLaunchSpec()
	seconds := int(time.Until(t.Deadline).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 5400 {
		seconds = 5400
	}
	body, err := piharness.BuildInstruction(piharness.Instruction{
		Binary: binary, Prompt: t.Prompt, Mode: opened.Mode, SessionDir: piharness.SessionDir,
		Checkpoint: piharness.CheckpointPath, DispositionPath: piharness.DispositionPath,
		HistoryPath: piharness.HistoryPath, ResultPath: piharness.ResultPath, ForwardEnv: forward,
		Offline: offline, PackageVersion: piharness.PackageVersion, SettleSeconds: seconds,
		RestoreCheckpoint: opened.Session.CheckpointBytes > 0,
	})
	if err != nil {
		return fail("INVALID_INPUT", err)
	}
	if t.Run.Sandbox == nil {
		return fail("RESOURCE_LOST", nil)
	}
	if err = a.Provider.WriteFile(ctx, *t.Run.Sandbox, piharness.InstructionPath, bytes.NewReader(body), int64(len(body))); err != nil {
		return convert(err)
	}
	return nil
}

func (a *Activities) finishPiAttempt(ctx context.Context, t store.Task, exit *core.ProcessExit) error {
	disposition, promptHash, err := a.readPiDisposition(ctx, t)
	if err != nil {
		return err
	}
	if disposition == "" {
		session, err := a.ensurePiSession(ctx, t)
		if err != nil {
			return err
		}
		disposition = session.Disposition
		promptHash = session.PromptSHA256
	}
	if disposition == "" {
		disposition = string(piharness.DispositionNone)
	}
	if promptHash == "" {
		promptHash = piharness.PromptHash(t.Prompt)
	}
	latest, err := a.DB.LatestAttempt(ctx, t.WorkspaceID, t.Run.ID)
	if err != nil {
		return err
	}
	retry := piRetry(exit, disposition)
	state := "closed"
	if retry {
		state = "lost"
	}
	if err = a.DB.SaveAttemptProgress(ctx, t.WorkspaceID, t.Run.ID, latest.SessionID, latest.Generation, disposition, promptHash, state); err != nil {
		return err
	}
	if !retry {
		return nil
	}
	if latest.Generation >= piharness.MaxAttempts {
		return fail("ATTEMPT_EXHAUSTED", nil)
	}
	return fail("ATTEMPT_LOST", nil)
}

func piRetry(exit *core.ProcessExit, disposition string) bool {
	switch piharness.Disposition(disposition) {
	case piharness.DispositionSettled, piharness.DispositionRejected, piharness.DispositionReconciled:
		return false
	}
	if exit.Code == 75 {
		return true
	}
	return exit.Signal != "" && (disposition == string(piharness.DispositionSending) || disposition == string(piharness.DispositionAccepted))
}

func (a *Activities) readPiDisposition(ctx context.Context, t store.Task) (string, string, error) {
	if t.Run == nil || t.Run.Sandbox == nil {
		return "", "", fail("RESOURCE_LOST", nil)
	}
	file, err := a.Provider.ReadFile(ctx, *t.Run.Sandbox, piharness.DispositionPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", nil
		}
		return "", "", convert(err)
	}
	defer file.Close() //nolint:errcheck // A short JSON file has no buffered write.
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", "", err
	}
	if len(data) > 4096 {
		return "", "", fail("INVALID_AGENT_OUTPUT", nil)
	}
	var body struct {
		Disposition  string `json:"disposition"`
		PromptSHA256 string `json:"prompt_sha256"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&body); err != nil {
		return "", "", fail("INVALID_AGENT_OUTPUT", err)
	}
	switch piharness.Disposition(body.Disposition) {
	case piharness.DispositionNone, piharness.DispositionSending, piharness.DispositionAccepted, piharness.DispositionSettled, piharness.DispositionRejected, piharness.DispositionReconciled:
	default:
		return "", "", fail("INVALID_AGENT_OUTPUT", nil)
	}
	return body.Disposition, body.PromptSHA256, nil
}

func (a *Activities) capturePiSession(ctx context.Context, t store.Task) error {
	if t.AgentID != piharness.AdapterID || t.Run == nil || t.Run.Sandbox == nil {
		return nil
	}
	disposition, promptHash, err := a.readPiDisposition(ctx, t)
	if err != nil {
		return err
	}
	latest, err := a.DB.LatestAttempt(ctx, t.WorkspaceID, t.Run.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if disposition == "" {
		disposition = latest.Disposition
		promptHash = latest.PromptSHA256
	}
	if promptHash == "" {
		promptHash = latest.PromptSHA256
	}
	if disposition != "" && promptHash != "" {
		if err = a.DB.SaveAttemptProgress(ctx, t.WorkspaceID, t.Run.ID, latest.SessionID, latest.Generation, disposition, promptHash, "closed"); err != nil {
			return err
		}
	}
	file, err := a.Provider.ReadFile(ctx, *t.Run.Sandbox, piharness.CheckpointPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return convert(err)
	}
	defer file.Close() //nolint:errcheck // The checkpoint is read once and then closed.
	data, err := io.ReadAll(io.LimitReader(file, piharness.MaxCheckpoint+1))
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > piharness.MaxCheckpoint {
		slog.WarnContext(ctx, "pi checkpoint was not stored", "bytes", len(data))
		return nil
	}
	return a.DB.SaveCheckpoint(ctx, t.WorkspaceID, latest.SessionID, data)
}

func processKey(t store.Task) core.OperationKey {
	if t.Run != nil && t.Run.Process != nil && t.Run.Process.Key.Step != "" {
		key := t.Run.Process.Key
		key.WorkspaceID = t.WorkspaceID
		key.TaskID = t.ID
		key.RunID = t.Run.ID
		return key
	}
	return key(t, "agent")
}

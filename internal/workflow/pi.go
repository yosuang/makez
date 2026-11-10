package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
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
	Session  store.NativeSession
	Attempt  store.Attempt
	Mode     string
	Prompt   store.NativePrompt
	Evidence piharness.Evidence
	Recovery piharness.Recovery
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
	hash := piharness.PromptHash(t.Prompt)
	attempt, err := a.DB.OpenAttempt(ctx, t.WorkspaceID, t.Run.ID, session.ID, hash)
	if errors.Is(err, store.ErrAttemptsExhausted) {
		return openedAttempt{}, fail("ATTEMPT_EXHAUSTED", err)
	}
	if err != nil {
		return openedAttempt{}, err
	}
	// The command row is the replay decision. The session's last disposition is only a cache.
	own, err := a.DB.NativePrompt(ctx, t.WorkspaceID, session.ID, hash)
	if err != nil {
		return openedAttempt{}, err
	}
	unknown, err := a.DB.HasUnknownPrompt(ctx, t.WorkspaceID, session.ID, hash)
	if err != nil {
		return openedAttempt{}, err
	}
	evidence, err := a.deliveryEvidence(ctx, t, own)
	if err != nil {
		return openedAttempt{}, err
	}
	recovery := piharness.Recover(evidence)
	action := piharness.Action{}
	if recovery.Send {
		action.SendPrompt = true
	} else if !recovery.Release {
		action = piharness.Decide(piharness.Disposition(own.Disposition), own.Found, piharness.HistoryUnread)
	}
	mode := piharness.Mode(action)
	if unknown && action.SendPrompt {
		// Another prompt is still in a process. Read before sending this one, and do not clear that row.
		mode = "reconcile_then_send"
	}
	return openedAttempt{Session: session, Attempt: attempt, Mode: mode, Prompt: own, Evidence: evidence, Recovery: recovery}, nil
}

// deliveryEvidence reads the owning run's sandbox. A missing release file and a missing
// pre-prompt disposition are local proof. They are not a Pi acceptance.
func (a *Activities) deliveryEvidence(ctx context.Context, t store.Task, prompt store.NativePrompt) (piharness.Evidence, error) {
	isOwner := t.Run != nil && prompt.Found && prompt.RunID == t.Run.ID
	ev := piharness.Evidence{Command: piharness.Disposition(prompt.Disposition), Started: prompt.Started, Owner: isOwner, FileKnown: true, ProcessKnown: true}
	if !prompt.Found || prompt.Disposition != string(piharness.DispositionSending) || !prompt.Started {
		return ev, nil
	}
	owner, err := a.DB.RunByID(ctx, t.WorkspaceID, prompt.RunID)
	if err != nil {
		ev.FileKnown, ev.ProcessKnown = false, false
		return ev, nil
	}
	ev.RunActive = runActive(owner.State)
	ev.ProcessLive, ev.ProcessKnown = a.processLiveness(ctx, owner)
	if owner.Sandbox == nil {
		ev.FileKnown = false
		return ev, nil
	}
	_, known, exists, err := a.sandboxFile(ctx, *owner.Sandbox, piharness.ReleasePath)
	if err != nil {
		return ev, err
	}
	if !known {
		ev.FileKnown = false
		return ev, nil
	}
	// The runner proceeds when the file exists, including an empty one. Content is not the proof.
	ev.ReleaseWritten = exists
	body, known, exists, err := a.sandboxFile(ctx, *owner.Sandbox, piharness.DispositionPath)
	if err != nil {
		return ev, err
	}
	if !known {
		ev.FileKnown = false
		return ev, nil
	}
	if !exists {
		return ev, nil
	}
	parsed, ok := parseDisposition(body)
	if !ok {
		ev.FileKnown = false
		return ev, nil
	}
	ev.FileDisposition = parsed
	return ev, nil
}

func runActive(state core.RunState) bool {
	switch state {
	case core.RunSucceeded, core.RunFailed, core.RunCanceled:
		return false
	default:
		return true
	}
}

func (a *Activities) processLiveness(ctx context.Context, run *store.Run) (live, known bool) {
	if run == nil || run.Process == nil || run.Sandbox == nil {
		return false, true
	}
	if run.Exit != nil {
		return false, true
	}
	obs, err := a.Provider.Process(ctx, *run.Sandbox, run.Process.Key)
	if err != nil {
		return false, false
	}
	switch obs.State {
	case core.ProcessExited, core.ProcessNotStarted:
		return false, true
	case core.ProcessRunning:
		return true, true
	case core.ProcessUnknown:
		return false, false
	default:
		// No affirmative receipt. A stored process with no exit is still treated as live.
		return true, true
	}
}

func (a *Activities) sandboxFile(ctx context.Context, sandbox core.SandboxRef, path string) (data []byte, known, exists bool, err error) {
	file, err := a.Provider.ReadFile(ctx, sandbox, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, false, nil
	}
	if err != nil {
		return nil, false, false, nil
	}
	defer file.Close() //nolint:errcheck // A short file has no buffered write.
	data, err = io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return nil, false, false, nil
	}
	return data, true, true, nil
}

func parseDisposition(data []byte) (piharness.Disposition, bool) {
	var body struct {
		Disposition  string `json:"disposition"`
		PromptSHA256 string `json:"prompt_sha256"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil {
		return "", false
	}
	switch piharness.Disposition(body.Disposition) {
	case piharness.DispositionNone, piharness.DispositionSending, piharness.DispositionAccepted, piharness.DispositionSettled, piharness.DispositionRejected, piharness.DispositionReconciled:
		return piharness.Disposition(body.Disposition), true
	default:
		return "", false
	}
}

// markPromptSending commits the command row before Provider.Start when this attempt will send.
// started stays false until the process exists. A crash before that still sends the prompt once.
// The mode chosen above is kept: reading the new row would turn this first send into a read.
// relaunchUnsentAttempt opens the next generation when this run's process died
// before the pre-prompt disposition flush. A live process is not started again.
func (a *Activities) relaunchUnsentAttempt(ctx context.Context, t store.Task, opened openedAttempt) (openedAttempt, error) {
	if !opened.Recovery.Send || !opened.Prompt.Started || t.Run == nil || opened.Prompt.RunID != t.Run.ID {
		return opened, nil
	}
	if t.Run.Process == nil || opened.Attempt.Step != t.Run.Process.Key.Step {
		return opened, nil
	}
	if !opened.Evidence.ProcessKnown || opened.Evidence.ProcessLive {
		return opened, nil
	}
	if opened.Attempt.Generation >= int64(piharness.MaxAttempts) {
		return openedAttempt{}, fail("ATTEMPT_EXHAUSTED", nil)
	}
	err := a.DB.SaveAttemptProgress(ctx, t.WorkspaceID, t.Run.ID, opened.Session.ID, opened.Attempt.Generation, string(piharness.DispositionSending), piharness.PromptHash(t.Prompt), "lost")
	if err != nil {
		return openedAttempt{}, err
	}
	return a.openPiAttempt(ctx, t)
}

func (a *Activities) markPromptSending(ctx context.Context, t store.Task, opened openedAttempt) error {
	if opened.Mode != "send" && opened.Mode != "reconcile_then_send" {
		return nil
	}
	hash := piharness.PromptHash(t.Prompt)
	if opened.Prompt.Found && opened.Prompt.Started {
		// The bytes were proved unsent. Move the row to this run and clear the local start.
		return a.DB.ReopenUnsentPrompt(ctx, t.WorkspaceID, t.Run.ID, opened.Session.ID, opened.Attempt.Generation, hash)
	}
	return a.DB.SaveAttemptProgress(ctx, t.WorkspaceID, t.Run.ID, opened.Session.ID, opened.Attempt.Generation, string(piharness.DispositionSending), hash, "")
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
	release := ""
	if opened.Mode == "send" || opened.Mode == "reconcile_then_send" {
		release = piharness.ReleasePath
	}
	body, err := piharness.BuildInstruction(piharness.Instruction{
		Binary: binary, Prompt: t.Prompt, Mode: opened.Mode, SessionDir: piharness.SessionDir,
		Checkpoint: piharness.CheckpointPath, DispositionPath: piharness.DispositionPath,
		HistoryPath: piharness.HistoryPath, ResultPath: piharness.ResultPath, ReleasePath: release,
		ForwardEnv: forward, Offline: offline, PackageVersion: piharness.PackageVersion, SettleSeconds: seconds,
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

// finishPromptDelivery marks the prompt started and then lets the runner write it.
// The mark is committed only after a process ref exists. The release file is second,
// so a crash before the process exists leaves the prompt sendable. The supervisor
// may create that absent file while the process runs. Replacing an existing file
// stays rejected.
func (a *Activities) finishPromptDelivery(ctx context.Context, t store.Task) error {
	if t.AgentID != piharness.AdapterID || t.Run == nil || t.Run.Process == nil || t.Run.Sandbox == nil {
		return nil
	}
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return err
	}
	hash := piharness.PromptHash(t.Prompt)
	prompt, err := a.DB.NativePrompt(ctx, t.WorkspaceID, session.ID, hash)
	if err != nil {
		return err
	}
	if !prompt.Found || prompt.Disposition != string(piharness.DispositionSending) || prompt.RunID != t.Run.ID {
		return nil
	}
	if !prompt.Started {
		err = a.DB.MarkPromptStarted(ctx, t.WorkspaceID, t.Run.ID, session.ID, piGeneration(t), hash)
		if err != nil {
			return err
		}
	}
	return a.writePiRelease(ctx, t)
}

func (a *Activities) writePiRelease(ctx context.Context, t store.Task) error {
	// A run that lost the fence must not release a process the next writer may also start.
	if err := a.renewPiSession(ctx, t); err != nil {
		return err
	}
	body := []byte("1\n")
	if err := a.Provider.WriteFile(ctx, *t.Run.Sandbox, piharness.ReleasePath, bytes.NewReader(body), int64(len(body))); err != nil {
		return convert(err)
	}
	return nil
}

func (a *Activities) finishPiAttempt(ctx context.Context, t store.Task, exit *core.ProcessExit) error {
	disposition, promptHash, err := a.readPiDisposition(ctx, t)
	if err != nil {
		return err
	}
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return err
	}
	if promptHash == "" {
		promptHash = piharness.PromptHash(t.Prompt)
	}
	if disposition == "" {
		disposition, err = a.DB.PromptDisposition(ctx, t.WorkspaceID, session.ID, promptHash)
		if err != nil {
			return err
		}
	}
	if disposition == "" {
		disposition = string(piharness.DispositionNone)
	}
	generation := piGeneration(t)
	retry := piRetry(exit, disposition)
	state := "closed"
	if retry {
		state = "lost"
	}
	err = a.DB.SaveAttemptProgress(ctx, t.WorkspaceID, t.Run.ID, session.ID, generation, disposition, promptHash, state)
	if errors.Is(err, store.ErrStaleWriter) {
		// A newer attempt or another run owns the session. Do not relaunch this prompt.
		return nil
	}
	if err != nil {
		return err
	}
	if !retry {
		return nil
	}
	if generation >= piharness.MaxAttempts {
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
	if err := a.renewPiSession(ctx, t); err != nil {
		if errors.Is(err, store.ErrSessionBusy) {
			slog.WarnContext(ctx, "skipped pi session capture because another run holds the writer lease")
			return nil
		}
		return err
	}
	disposition, promptHash, err := a.readPiDisposition(ctx, t)
	if err != nil {
		return err
	}
	_, err = a.DB.LatestAttempt(ctx, t.WorkspaceID, t.Run.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return err
	}
	if promptHash == "" {
		promptHash = piharness.PromptHash(t.Prompt)
	}
	if disposition == "" {
		disposition, err = a.DB.PromptDisposition(ctx, t.WorkspaceID, session.ID, promptHash)
		if err != nil {
			return err
		}
	}
	if disposition != "" && promptHash != "" {
		err = a.DB.SaveAttemptProgress(ctx, t.WorkspaceID, t.Run.ID, session.ID, piGeneration(t), disposition, promptHash, "closed")
		if errors.Is(err, store.ErrStaleWriter) {
			slog.WarnContext(ctx, "skipped stale pi session write")
			return nil
		}
		if err != nil {
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
	err = a.DB.SaveCheckpoint(ctx, t.WorkspaceID, session.ID, t.Run.ID, data)
	if errors.Is(err, store.ErrStaleWriter) {
		slog.WarnContext(ctx, "skipped stale pi checkpoint write")
		return nil
	}
	return err
}

func (a *Activities) renewPiSession(ctx context.Context, t store.Task) error {
	if t.AgentID != piharness.AdapterID || t.Run == nil {
		return nil
	}
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return err
	}
	return a.DB.AcquireSessionWriter(ctx, t.WorkspaceID, session.ID, t.Run.ID)
}

func (a *Activities) releasePiSession(ctx context.Context, t store.Task) error {
	if t.AgentID != piharness.AdapterID || t.Run == nil {
		return nil
	}
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return err
	}
	return a.DB.ReleaseSessionWriter(ctx, t.WorkspaceID, session.ID, t.Run.ID)
}

func (a *Activities) trackPiSession(ctx context.Context, t store.Task) error {
	err := a.renewPiSession(ctx, t)
	if errors.Is(err, store.ErrSessionBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	return a.persistPiDisposition(ctx, t)
}

func (a *Activities) persistRunningPi(ctx context.Context, t store.Task) error {
	disposition, _, err := a.readPiDisposition(ctx, t)
	if err != nil {
		return err
	}
	if disposition == "" || disposition == string(piharness.DispositionNone) {
		err = a.renewPiSession(ctx, t)
		if errors.Is(err, store.ErrSessionBusy) {
			return nil
		}
		return err
	}
	if err = a.renewPiSession(ctx, t); err != nil {
		return err
	}
	return a.persistPiDisposition(ctx, t)
}

func (a *Activities) persistPiDisposition(ctx context.Context, t store.Task) error {
	disposition, promptHash, err := a.readPiDisposition(ctx, t)
	if err != nil {
		return err
	}
	if disposition == "" || disposition == string(piharness.DispositionNone) || promptHash == "" || t.Run == nil {
		return nil
	}
	session, err := a.ensurePiSession(ctx, t)
	if err != nil {
		return err
	}
	err = a.DB.SaveAttemptProgress(ctx, t.WorkspaceID, t.Run.ID, session.ID, piGeneration(t), disposition, promptHash, "")
	if errors.Is(err, store.ErrStaleWriter) {
		return nil
	}
	return err
}

func piGeneration(t store.Task) int64 {
	step := "agent"
	if t.Run != nil && t.Run.Process != nil && t.Run.Process.Key.Step != "" {
		step = t.Run.Process.Key.Step
	}
	if step == "agent" {
		return 1
	}
	rest, ok := strings.CutPrefix(step, "agent:")
	if !ok {
		return 1
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n < 1 || n > int64(piharness.MaxAttempts) {
		return 1
	}
	return n
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

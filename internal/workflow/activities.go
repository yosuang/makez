package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

type Activities struct {
	DB        *store.Store
	Provider  core.SandboxProvider
	Agents    map[string]core.AgentAdapter
	Artifacts *artifact.Store
	Secrets   core.SecretResolver
}

func findApplicationError(err error, target **temporal.ApplicationError) bool {
	return errors.As(err, target)
}
func fail(code string, err error) error {
	return temporal.NewNonRetryableApplicationError(code, code, err)
}
func convert(err error) error {
	var p core.ProviderError
	if errors.As(err, &p) && p.Code() != core.Transient {
		return fail(string(p.Code()), err)
	}
	return err
}
func key(t store.Task, step string) core.OperationKey {
	return core.OperationKey{WorkspaceID: t.WorkspaceID, TaskID: t.ID, RunID: t.Run.ID, Step: step}
}
func ptr[T any](v T) *T { return &v }
func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func (a *Activities) task(ctx context.Context, in Input) (store.Task, error) {
	return a.DB.Get(ctx, in.WorkspaceID, in.TaskID)
}
func (a *Activities) adapter(t store.Task) (core.AgentAdapter, error) {
	v, ok := a.Agents[t.AgentID+"@"+t.AgentVersion]
	if !ok {
		return nil, fail("INVALID_AGENT", nil)
	}
	return v, nil
}
func (a *Activities) plan(ctx context.Context, t store.Task) (core.AgentPlan, error) {
	if t.Run == nil {
		return core.AgentPlan{}, fail("RUN_MISSING", nil)
	}
	if t.Run.Plan != nil {
		return *t.Run.Plan, nil
	}
	v, e := a.adapter(t)
	if e != nil {
		return core.AgentPlan{}, e
	}
	p, e := v.Plan(core.AgentInput{TaskID: t.ID, RunID: t.Run.ID, Prompt: t.Prompt, Config: t.Config})
	if e != nil {
		return p, fail("INVALID_INPUT", e)
	}
	return a.DB.FreezePlan(ctx, t.Run.ID, p)
}
func active(t store.Task) error {
	if t.CancelRequested {
		return fail("CANCEL_REQUESTED", nil)
	}
	if time.Now().After(t.Deadline) {
		return fail("DEADLINE_EXCEEDED", nil)
	}
	return nil
}
func terminal(t core.TaskState) bool {
	return t == core.TaskSucceeded || t == core.TaskFailed || t == core.TaskCanceled
}
func (a *Activities) Initialize(ctx context.Context, in Input) (Initialized, error) {
	t, e := a.DB.InitializeRun(ctx, in.WorkspaceID, in.TaskID, a.Provider.BackendID())
	if e != nil {
		return Initialized{}, e
	}
	return Initialized{Done: terminal(t.State), Deadline: t.Deadline}, nil
}
func (a *Activities) Prepare(ctx context.Context, in Input) error {
	t, e := a.task(ctx, in)
	if e != nil {
		return e
	}
	if e = active(t); e != nil {
		return e
	}
	adapter, e := a.adapter(t)
	if e != nil {
		return e
	}
	caps, e := a.Provider.Capabilities(ctx, t.ProfileID, t.ProfileVersion)
	if e != nil {
		return convert(e)
	}
	for _, want := range adapter.Requirements() {
		if !slices.Contains(caps.Supported, want) {
			return fail("UNSUPPORTED_CAPABILITY", fmt.Errorf("missing %s", want))
		}
	}
	plan, e := a.plan(ctx, t)
	if e != nil {
		return e
	}
	if len(plan.SecretRefs) > 0 {
		if !slices.Contains(caps.Supported, core.SecretInjection) {
			return fail("UNSUPPORTED_CAPABILITY", fmt.Errorf("missing %s", core.SecretInjection))
		}
		if _, e = a.resolveSecrets(ctx, t.WorkspaceID, plan.SecretRefs); e != nil {
			return e
		}
	}
	if t.Run.Sandbox == nil {
		req := core.SandboxRequest{Key: key(t, "sandbox"), RuntimeProfileID: t.ProfileID, RuntimeProfileVersion: t.ProfileVersion, ExpiresAt: t.Deadline.Add(15 * time.Minute), Limits: core.ResourceLimits{MilliCPU: 1000, MemoryBytes: 512 << 20}}
		_, claimed, e := a.DB.BeginOperation(ctx, t.Run.ID, "sandbox", hash(req))
		if e != nil {
			return e
		}
		var ref core.SandboxRef
		if claimed {
			ref, e = a.Provider.Ensure(ctx, req)
			if e != nil {
				var p core.ProviderError
				if errors.As(e, &p) && p.Code() != core.OutcomeUnknown && p.Code() != core.Transient {
					return convert(e)
				}
			}
		}
		if ref.ID == "" {
			until := time.Now().Add(90 * time.Second)
			for time.Now().Before(until) {
				obs, err := a.Provider.Inspect(ctx, key(t, "sandbox"))
				if err == nil && obs.State == core.SandboxReady && len(obs.Matches) == 1 {
					ref = obs.Matches[0]
					break
				}
				if err == nil && len(obs.Matches) > 1 {
					return fail("SANDBOX_AMBIGUOUS", nil)
				}
				if err = wait(ctx, time.Second); err != nil {
					return err
				}
			}
			if ref.ID == "" {
				return fail("SANDBOX_OUTCOME_UNKNOWN", e)
			}
		}
		if e = a.DB.UpdateRun(ctx, t.Run.ID, store.RunUpdate{Sandbox: &ref}); e != nil {
			return e
		}
		if e = a.DB.ConfirmOperation(ctx, t.Run.ID, "sandbox"); e != nil {
			return e
		}
		t.Run.Sandbox = &ref
	}
	// A retried Prepare only rewrites immutable input before any launch.
	for _, f := range plan.Files {
		if e = a.Provider.WriteFile(ctx, *t.Run.Sandbox, f.Path, bytes.NewReader(f.Data), int64(len(f.Data))); e != nil {
			var pe core.ProviderError
			if errors.As(e, &pe) && (pe.Code() == core.OutcomeUnknown || pe.Code() == core.Transient) {
				return e
			}
			return convert(e)
		}
	}
	return a.DB.UpdateRun(ctx, t.Run.ID, store.RunUpdate{State: ptr(core.RunStarting)})
}
func (a *Activities) Launch(ctx context.Context, in Input) error {
	t, e := a.task(ctx, in)
	if e != nil {
		return e
	}
	if e = active(t); e != nil {
		return e
	}
	if t.Run.Process != nil {
		return nil
	}
	p, e := a.plan(ctx, t)
	if e != nil {
		return e
	}
	if t.Run.Sandbox == nil {
		return fail("RESOURCE_LOST", nil)
	}
	req := core.StartRequest{Key: key(t, "agent"), Command: p.Command, SecretRefs: p.SecretRefs, Deadline: t.Deadline}
	// An existing launch intent is reconciled even when credentials were revoked.
	phase, e := a.DB.OperationPhase(ctx, t.Run.ID, "agent")
	if e != nil {
		return e
	}
	if phase == "" {
		req.SecretValues, e = a.resolveSecrets(ctx, t.WorkspaceID, p.SecretRefs)
		if e != nil {
			return e
		}
	}
	_, claimed, e := a.DB.BeginOperation(ctx, t.Run.ID, "agent", hash(req))
	if e != nil {
		return e
	}
	var ref core.ProcessRef
	if claimed {
		ref, e = a.Provider.Start(ctx, *t.Run.Sandbox, req)
		if e != nil {
			var p core.ProviderError
			if errors.As(e, &p) && p.Code() != core.OutcomeUnknown && p.Code() != core.Transient {
				return convert(e)
			}
		}
	}
	if ref.ID == "" {
		until := time.Now().Add(30 * time.Second)
		for time.Now().Before(until) {
			o, err := a.Provider.Process(ctx, *t.Run.Sandbox, key(t, "agent"))
			if err == nil && o.Ref != nil && (o.State == core.ProcessRunning || o.State == core.ProcessExited) {
				ref = *o.Ref
				break
			}
			if err = wait(ctx, time.Second); err != nil {
				return err
			}
		}
		if ref.ID == "" {
			return fail("START_OUTCOME_UNKNOWN", e)
		}
	}
	if e = a.DB.UpdateRun(ctx, t.Run.ID, store.RunUpdate{Process: &ref, State: ptr(core.RunRunning)}); e != nil {
		return e
	}
	return a.DB.ConfirmOperation(ctx, t.Run.ID, "agent")
}
func (a *Activities) Observe(ctx context.Context, in Input) error {
	t, e := a.task(ctx, in)
	if e != nil {
		return e
	}
	ad, e := a.adapter(t)
	if e != nil {
		return e
	}
	if t.Run.Process == nil || t.Run.Sandbox == nil {
		return fail("PROCESS_MISSING", nil)
	}
	epoch, e := a.DB.ClaimObserver(ctx, t.Run.ID)
	if e != nil {
		return e
	}
	for {
		activity.RecordHeartbeat(ctx, t.Run.Cursor)
		// The database is authoritative even while the cancellation outbox is pending.
		latest, err := a.task(ctx, in)
		if err != nil {
			return err
		}
		if err = active(latest); err != nil {
			return err
		}
		if e = a.drainPage(ctx, t, ad, epoch); e != nil {
			return e
		}
		obs, e := a.Provider.Process(ctx, *t.Run.Sandbox, key(t, "agent"))
		if e != nil {
			return convert(e)
		}
		if obs.Exit != nil {
			return a.DB.UpdateRun(ctx, t.Run.ID, store.RunUpdate{Exit: obs.Exit})
		}
		if obs.State != core.ProcessRunning {
			return fail("PROCESS_OUTCOME_UNKNOWN", nil)
		}
		if e = wait(ctx, time.Second); e != nil {
			return e
		}
	}
}
func (a *Activities) drainPage(ctx context.Context, t store.Task, ad core.AgentAdapter, epoch int64) error {
	page, e := a.Provider.Output(ctx, *t.Run.Process, t.Run.Cursor, 64<<10)
	if e != nil {
		return convert(e)
	}
	decoded, e := ad.Decode(page, t.Run.ParserState)
	if e != nil {
		return fail("INVALID_AGENT_OUTPUT", e)
	}
	if e = a.DB.AppendOutput(ctx, t.Run.ID, epoch, page.NextCursor, decoded.NextState, decoded.Events); e != nil {
		return e
	}
	t.Run.Cursor = page.NextCursor
	t.Run.ParserState = decoded.NextState
	return nil
}

type reader struct {
	p core.SandboxProvider
	s core.SandboxRef
}

func (r reader) Read(ctx context.Context, p string) (io.ReadCloser, error) {
	return r.p.ReadFile(ctx, r.s, p)
}
func (a *Activities) Finalize(ctx context.Context, in FinishInput) error {
	t, e := a.task(ctx, in.Input)
	if e != nil {
		return e
	}
	if terminal(t.State) {
		return nil
	}
	if t.Run == nil {
		return a.DB.Publish(ctx, in.WorkspaceID, in.TaskID, core.TaskFailed, "INITIALIZE_FAILED", true, core.CleanupNotRequired, nil)
	}
	// Keep heartbeats alive during bounded archive/delete operations and retries.
	beatCtx, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				activity.RecordHeartbeat(beatCtx, "finalizing")
			case <-beatCtx.Done():
				return
			}
		}
	}()
	if t.Run.Result != nil {
		cleanup := core.CleanupPending
		delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := a.deleteForRun(delCtx, t)
		cancel()
		if err == nil {
			cleanup = core.CleanupDone
		}
		return a.DB.Publish(ctx, in.WorkspaceID, in.TaskID, t.Run.Result.State, t.Run.Result.Reason, t.Run.VerifiedStopped, cleanup, t.Run.Result.Artifacts)
	}
	reason := in.Reason
	if t.CancelRequested {
		reason = "CANCEL_REQUESTED"
	}
	verified := false
	cleanup := core.CleanupPending
	if t.Run.State != core.RunFinalizing {
		if e = a.DB.UpdateRun(ctx, t.Run.ID, store.RunUpdate{State: ptr(core.RunStopping)}); e != nil {
			return e
		}
	}
	if t.Run.Sandbox != nil {
		policy := core.StopPolicy{Signal: "SIGTERM", GracePeriod: 15 * time.Second}
		if p, err := a.plan(ctx, t); err == nil {
			policy = p.Stop
		}
		stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		stopErr := a.Provider.Stop(stopCtx, *t.Run.Sandbox, key(t, "agent"), policy)
		cancel()
		verified = stopErr == nil
		if !verified {
			delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			delErr := a.deleteForRun(delCtx, t)
			cancel()
			if delErr == nil {
				verified = true
				cleanup = core.CleanupDone
			}
		}
	} else {
		// No launch can occur without the sandbox ref persisted first.
		verified = true
	}
	if !verified {
		reason = "STOP_UNCONFIRMED"
	}
	if e = a.DB.UpdateRun(ctx, t.Run.ID, store.RunUpdate{State: ptr(core.RunFinalizing), VerifiedStopped: &verified}); e != nil {
		return e
	}
	var artifacts []store.Artifact
	state := core.TaskFailed
	if verified && t.Run.Sandbox != nil && cleanup != core.CleanupDone {
		ad, adErr := a.adapter(t)
		if adErr == nil && t.Run.Process != nil {
			epoch, err := a.DB.ClaimObserver(ctx, t.Run.ID)
			if err != nil {
				return err
			}
			// Stop closes the process group before the final drain, including descendants.
			for i := 0; i < 256; i++ {
				page, err := a.Provider.Output(ctx, *t.Run.Process, t.Run.Cursor, 64<<10)
				if err != nil {
					if reason == "" {
						reason = "OUTPUT_GAP"
					}
					break
				}
				d, err := ad.Decode(page, t.Run.ParserState)
				if err != nil {
					if reason == "" {
						reason = "INVALID_AGENT_OUTPUT"
					}
					break
				}
				if err = a.DB.AppendOutput(ctx, t.Run.ID, epoch, page.NextCursor, d.NextState, d.Events); err != nil {
					return err
				}
				t.Run.Cursor = page.NextCursor
				t.Run.ParserState = d.NextState
				if page.EOF {
					break
				}
				if len(page.Records) == 0 || i == 255 {
					if reason == "" {
						reason = "OUTPUT_LIMIT"
					}
					break
				}
			}
		}
		if reason == "" && t.Run.Exit != nil && adErr == nil {
			result, err := ad.Collect(ctx, reader{a.Provider, *t.Run.Sandbox}, *t.Run.Exit)
			if err != nil || !result.Succeeded {
				reason = "AGENT_FAILED"
			} else {
				records, err := a.Artifacts.Archive(ctx, t.Run.ID, reader{a.Provider, *t.Run.Sandbox}, result.Artifacts)
				if err != nil {
					if activity.GetInfo(ctx).Attempt < 5 {
						return err
					} // Preserve the sole copy during bounded retries.
					reason = "ARCHIVE_FAILED"
				}
				for _, r := range records {
					artifacts = append(artifacts, store.Artifact{RunID: t.Run.ID, Path: strings.TrimPrefix(r.LogicalPath, "/workspace/"), StorageKey: r.StorageKey, SHA256: r.SHA256, MediaType: r.MediaType, Size: r.Size})
				}
				if reason == "" {
					state = core.TaskSucceeded
				}
			}
		}
	}
	if reason == "CANCEL_REQUESTED" && verified {
		state = core.TaskCanceled
	}
	if state == core.TaskFailed && reason == "" {
		reason = "RESULT_UNAVAILABLE"
	}
	if e = a.DB.UpdateRun(ctx, t.Run.ID, store.RunUpdate{Result: &store.PreparedResult{State: state, Reason: reason, Artifacts: artifacts}}); e != nil {
		return e
	}
	if cleanup != core.CleanupDone {
		deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := a.deleteForRun(deleteCtx, t)
		cancel()
		if err == nil {
			cleanup = core.CleanupDone
		}
	}
	return a.DB.Publish(ctx, in.WorkspaceID, in.TaskID, state, reason, verified, cleanup, artifacts)
}
func (a *Activities) Reap(ctx context.Context, in Input) error {
	t, e := a.task(ctx, in)
	if e != nil {
		return e
	}
	if t.Run == nil {
		return nil
	}
	if e = a.deleteForRun(ctx, t); e != nil {
		return e
	}
	return a.DB.SetCleanup(ctx, t.Run.ID, core.CleanupDone)
}
func (a *Activities) BlockCleanup(ctx context.Context, in Input) error {
	t, e := a.task(ctx, in)
	if e != nil {
		return e
	}
	if t.Run == nil {
		return nil
	}
	return a.DB.SetCleanup(ctx, t.Run.ID, core.CleanupBlocked)
}
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A timed-out create can arrive after an empty list. Keep reconciliation pending
// while that intent is unresolved; an empty list alone is not a completion receipt.
func (a *Activities) deleteForRun(ctx context.Context, t store.Task) error {
	if err := a.Provider.Delete(ctx, key(t, "sandbox")); err != nil {
		if a.DB.Metrics != nil {
			a.DB.Metrics.CleanupFailures.Inc()
		}
		slog.WarnContext(ctx, "sandbox cleanup failed", "task_id", t.ID, "run_id", t.Run.ID, "error_type", fmt.Sprintf("%T", err))
		return err
	}
	if t.Run.Sandbox == nil {
		phase, err := a.DB.OperationPhase(ctx, t.Run.ID, "sandbox")
		if err != nil {
			return err
		}
		if phase != "" {
			return errors.New("sandbox create has no confirmed receipt; cleanup requires reconciliation")
		}
	}
	return nil
}

func (a *Activities) resolveSecrets(ctx context.Context, workspace string, refs map[string]string) (map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if a.Secrets == nil {
		return nil, fail("AUTH_NOT_CONFIGURED", nil)
	}
	values, err := a.Secrets.Resolve(ctx, workspace, refs)
	if err != nil {
		return nil, fail("AUTH_UNAVAILABLE", nil)
	}
	return values, nil
}

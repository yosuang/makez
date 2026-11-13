package store

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// TestPublishArtifactChecks uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Rejecting an artifact is not a claim that a file was stored once.
// This is not exactly-once for an external action and not Pi acceptance.
func TestPublishArtifactChecks(t *testing.T) {
	fx, clock := openArtifactPublish(t)
	requirePublishableArtifact(t, acceptedArtifact())
	requireOneStoredArtifact(t, fx, clock)

	// An absolute path is the first rejection. The database would keep it.
	metadata := "invalid artifact metadata, task not SUCCEEDED, 0 artifacts"
	rejected := []struct {
		name   string
		edit   func(Artifact) Artifact
		reject func(error) bool
		want   string
	}{
		{name: "absolute path", edit: func(a Artifact) Artifact { a.Path = "/workspace/result.txt"; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "empty path", edit: func(a Artifact) Artifact { a.Path = ""; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "..", edit: func(a Artifact) Artifact { a.Path = ".."; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "unclean path", edit: func(a Artifact) Artifact { a.Path = "out/./result.txt"; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "../ path", edit: func(a Artifact) Artifact { a.Path = "../result.txt"; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: ".", edit: func(a Artifact) Artifact { a.Path = "."; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "backslash", edit: func(a Artifact) Artifact { a.Path = "out\\result.txt"; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "NUL", edit: func(a Artifact) Artifact { a.Path = "out/\x00result.txt"; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "empty storage key", edit: func(a Artifact) Artifact { a.StorageKey = ""; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "negative size", edit: func(a Artifact) Artifact { a.Size = -1; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "short sha256", edit: func(a Artifact) Artifact { a.SHA256 = strings.Repeat("ab", 31) + "a"; return a }, reject: invalidArtifactMetadata, want: metadata},
		{name: "sha256 not lowercase hex", edit: func(a Artifact) Artifact { a.SHA256 = strings.Repeat("A", 64); return a }, reject: digestConstraint, want: "makez_artifacts_digest rejection, task not SUCCEEDED, 0 artifacts"},
	}
	requireArtifactShapes(t, rejected)
	for _, tc := range rejected {
		requireRejectedArtifact(t, fx, clock, tc.name, []Artifact{tc.edit(acceptedArtifact())}, tc.reject, tc.want)
	}

	other := openPublishRun(t, fx, core.RunFinalizing, clock)
	foreign := acceptedArtifact()
	requirePublishableArtifact(t, foreign)
	foreign.RunID = other.Run.ID
	if foreign.RunID == "" {
		t.Fatal("foreign run id is empty")
	}
	requireRejectedArtifact(t, fx, clock, "foreign run", []Artifact{foreign}, func(err error) bool {
		return errors.Is(err, ErrConflict)
	}, "ErrConflict, task not SUCCEEDED, 0 artifacts")
	if n := len(taskArtifacts(t, fx.db, other.ID)); n != 0 {
		t.Fatalf("other run stored %d artifacts", n)
	}

	// Distinct ids keep the primary key out of this rejection.
	// UNIQUE(run_id, path) is what rolls the publish back.
	left, right := acceptedArtifact(), acceptedArtifact()
	left.ID, right.ID = newID(), newID()
	left.Path, right.Path = "out/same.txt", "out/same.txt"
	left.StorageKey, right.StorageKey = "objects/left", "objects/right"
	right.SHA256 = strings.Repeat("cd", 32)
	requirePublishableArtifact(t, left)
	requirePublishableArtifact(t, right)
	if left.ID == "" || left.ID == right.ID || left.Path != right.Path || path.Clean(left.Path) != left.Path {
		t.Fatal("duplicate fixture does not have two ids and one clean path")
	}
	requireRejectedArtifact(t, fx, clock, "duplicate path", []Artifact{left, right}, duplicateArtifactPath, "unique path rejection, task not SUCCEEDED, 0 artifacts")
}

func openArtifactPublish(t *testing.T) (runInitFixture, publishClock) {
	t.Helper()
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "artifact-" + newID()
	token := "artifact-token-" + newID()
	if err := db.RegisterMachines(ctx, map[string]string{token: workspace}); err != nil {
		t.Fatal(err)
	}
	principal, bound, err := db.AuthenticateMachine(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if bound != workspace || principal.Kind != "machine" || principal.ID == "" {
		t.Fatalf("principal = %+v workspace %s", principal, bound)
	}
	fx := runInitFixture{db: db, ctx: WithPrincipal(ctx, principal), workspace: workspace}
	now := time.Now().UTC().Truncate(time.Microsecond)
	finished := now.Add(-2 * time.Minute)
	// Deadline is after the recorded exit, so a verified success can be stored as SUCCEEDED.
	clock := publishClock{
		exit:     &core.ProcessExit{FinishedAt: finished},
		deadline: finished.Add(time.Minute),
		afterNow: false,
	}
	return fx, clock
}

func requireOneStoredArtifact(t *testing.T, fx runInitFixture, clock publishClock) {
	t.Helper()
	task := openPublishRun(t, fx, core.RunFinalizing, clock)
	requireFinishClock(t, task, clock, true)
	before := readPublishCounts(t, fx.db, task.ID)
	requirePublishBaseline(t, before, core.RunFinalizing)
	if n := len(taskArtifacts(t, fx.db, task.ID)); n != 0 {
		t.Fatalf("artifacts before publish = %d", n)
	}
	artifact := acceptedArtifact()
	publish := func() error {
		return publishSucceeded(fx, task.ID, []Artifact{artifact})
	}
	err := publish()
	after := readPublishCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	rows := taskArtifacts(t, fx.db, task.ID)
	if err != nil || after.task != string(core.TaskSucceeded) || after.reason != "DONE" || after.run != string(core.RunSucceeded) || after.cleanup != string(core.CleanupNotRequired) || after.cleanups != 0 || after.runs != 1 || after.tasks != 1 || after.runID != before.runID || after.cancel || len(rows) != 1 || rows[0].path != artifact.Path || rows[0].digest != artifact.SHA256 || rows[0].key != artifact.StorageKey || rows[0].size != artifact.Size || rows[0].runID != before.runID || !sameTaskView(stored, after) {
		t.Fatalf("valid publish = %v task %s reason %q run %s %s START_CLEANUP %d runs %d, want SUCCEEDED, 1 artifact path %q digest %s, 0 START_CLEANUP, 1 run", err, after.task, after.reason, after.run, artifactOutcome(rows), after.cleanups, after.runs, artifact.Path, artifact.SHA256)
	}
	replayErr := publish()
	replay := readPublishCounts(t, fx.db, task.ID)
	replayRows := taskArtifacts(t, fx.db, task.ID)
	if replayErr != nil || replay != after || len(replayRows) != 1 || replay.runs != 1 || replay.runID != before.runID || replayRows[0] != rows[0] {
		t.Fatalf("second publish = %v task %s %s runs %d run %s, want nil, 1 artifact, 1 run %s", replayErr, replay.task, artifactOutcome(replayRows), replay.runs, replay.runID, before.runID)
	}
}

func requireRejectedArtifact(t *testing.T, fx runInitFixture, clock publishClock, name string, artifacts []Artifact, reject func(error) bool, want string) {
	t.Helper()
	task := openPublishRun(t, fx, core.RunFinalizing, clock)
	requireFinishClock(t, task, clock, true)
	for _, artifact := range artifacts {
		if artifact.RunID != "" && artifact.RunID == task.Run.ID {
			t.Fatalf("%s uses the publishing run id", name)
		}
	}
	before := readPublishCounts(t, fx.db, task.ID)
	requirePublishBaseline(t, before, core.RunFinalizing)
	err := publishSucceeded(fx, task.ID, artifacts)
	after := readPublishCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	rows := taskArtifacts(t, fx.db, task.ID)
	if !reject(err) || after != before || len(rows) != 0 || storedSucceeded(stored, after) || !sameTaskView(stored, after) {
		t.Fatalf("%s publish = %v task %s reason %q run %s %s START_CLEANUP %d runs %d, want %s", name, err, after.task, after.reason, after.run, artifactOutcome(rows), after.cleanups, after.runs, want)
	}
}

func publishSucceeded(fx runInitFixture, id string, artifacts []Artifact) error {
	return fx.db.Publish(fx.ctx, fx.workspace, id, core.TaskSucceeded, "DONE", true, core.CleanupNotRequired, artifacts)
}

func acceptedArtifact() Artifact {
	return Artifact{
		Path:       "out/result.txt",
		StorageKey: "objects/result",
		SHA256:     strings.Repeat("ab", 32),
		MediaType:  "text/plain",
		Size:       0,
	}
}

func requirePublishableArtifact(t *testing.T, artifact Artifact) {
	t.Helper()
	if artifact.Path == "" || path.IsAbs(artifact.Path) || path.Clean(artifact.Path) != artifact.Path || strings.HasPrefix(artifact.Path, "../") || artifact.Path == ".." || artifact.Path == "." || strings.ContainsAny(artifact.Path, "\\\x00") || artifact.StorageKey == "" || artifact.Size < 0 || !lowercaseSHA256(artifact.SHA256) {
		t.Fatalf("artifact would be rejected: path %q key %q size %d digest %q", artifact.Path, artifact.StorageKey, artifact.Size, artifact.SHA256)
	}
}

func requireArtifactShapes(t *testing.T, cases []struct {
	name   string
	edit   func(Artifact) Artifact
	reject func(error) bool
	want   string
}) {
	t.Helper()
	base := acceptedArtifact()
	seen := map[string]bool{}
	for _, tc := range cases {
		if seen[tc.name] {
			t.Fatalf("duplicate case %s", tc.name)
		}
		seen[tc.name] = true
		edited := tc.edit(base)
		switch tc.name {
		case "absolute path":
			if !path.IsAbs(edited.Path) || path.Clean(edited.Path) != edited.Path {
				t.Fatalf("absolute fixture %q", edited.Path)
			}
		case "empty path":
			if edited.Path != "" {
				t.Fatal("empty path fixture is not empty")
			}
		case "..":
			if edited.Path != ".." || path.Clean(edited.Path) != edited.Path || strings.HasPrefix(edited.Path, "../") {
				t.Fatalf("`..` fixture %q", edited.Path)
			}
		case "unclean path":
			if path.Clean(edited.Path) == edited.Path || path.IsAbs(edited.Path) {
				t.Fatalf("unclean fixture %q", edited.Path)
			}
		case "../ path":
			if !strings.HasPrefix(edited.Path, "../") || path.Clean(edited.Path) != edited.Path || path.IsAbs(edited.Path) {
				t.Fatalf("parent fixture %q", edited.Path)
			}
		case ".":
			if edited.Path != "." || path.Clean(edited.Path) != edited.Path {
				t.Fatalf("dot fixture %q", edited.Path)
			}
		case "backslash":
			if !strings.Contains(edited.Path, `\`) || strings.Contains(edited.Path, "\x00") || path.Clean(edited.Path) != edited.Path {
				t.Fatalf("backslash fixture %q", edited.Path)
			}
		case "NUL":
			if !strings.Contains(edited.Path, "\x00") || strings.Contains(edited.Path, `\`) {
				t.Fatal("NUL fixture lost its NUL")
			}
		case "empty storage key":
			if edited.StorageKey != "" || edited.Path != base.Path {
				t.Fatal("empty storage key fixture changed another field")
			}
		case "negative size":
			if edited.Size >= 0 || edited.Path != base.Path {
				t.Fatalf("negative size fixture %d", edited.Size)
			}
		case "short sha256":
			if len(edited.SHA256) >= 64 || edited.Path != base.Path {
				t.Fatalf("short digest length %d", len(edited.SHA256))
			}
		case "sha256 not lowercase hex":
			if len(edited.SHA256) != 64 || lowercaseSHA256(edited.SHA256) || edited.Path != base.Path {
				t.Fatalf("uppercase digest %q", edited.SHA256)
			}
		default:
			t.Fatalf("unchecked artifact case %s", tc.name)
		}
	}
}

func invalidArtifactMetadata(err error) bool {
	return err != nil && err.Error() == "invalid artifact metadata"
}

func digestConstraint(err error) bool {
	return err != nil && !errors.Is(err, ErrConflict) && err.Error() != "invalid artifact metadata" && strings.Contains(err.Error(), "makez_artifacts_digest")
}

func duplicateArtifactPath(err error) bool {
	return err != nil && strings.Contains(err.Error(), "makez_artifacts_run_id_path_key")
}

func lowercaseSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

type artifactRow struct {
	path   string
	digest string
	key    string
	runID  string
	size   int64
}

func taskArtifacts(t *testing.T, db *Store, taskID string) []artifactRow {
	t.Helper()
	rows, err := db.pool.Query(t.Context(), `
		SELECT a.path, a.sha256, a.storage_key, a.size, a.run_id
		FROM makez_artifacts a
		JOIN makez_runs r ON r.id = a.run_id AND r.workspace_id = a.workspace_id
		WHERE r.task_id = $1
		ORDER BY a.id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []artifactRow
	for rows.Next() {
		var row artifactRow
		if err = rows.Scan(&row.path, &row.digest, &row.key, &row.size, &row.runID); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func artifactOutcome(rows []artifactRow) string {
	switch len(rows) {
	case 0:
		return "0 artifacts"
	case 1:
		return fmt.Sprintf("1 artifact path %q digest %s", rows[0].path, rows[0].digest)
	default:
		paths := make([]string, len(rows))
		for i, row := range rows {
			paths[i] = fmt.Sprintf("%q", row.path)
		}
		return fmt.Sprintf("%d artifacts paths %s", len(rows), strings.Join(paths, ","))
	}
}

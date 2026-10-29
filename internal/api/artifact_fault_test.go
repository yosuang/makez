package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

func archivedTask(t *testing.T, f *apiFixture) (store.Task, []store.Artifact) {
	t.Helper()
	task := f.submit(t, "artifact-fault")
	task, err := f.db.InitializeRun(t.Context(), task.WorkspaceID, task.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []core.RunState{core.RunStopping, core.RunFinalizing} {
		if err = f.db.UpdateRun(t.Context(), task.Run.ID, store.RunUpdate{State: new(state)}); err != nil {
			t.Fatal(err)
		}
	}
	files, err := f.files.Archive(t.Context(), task.Run.ID, stringReader("verified artifact bytes"), []core.ArtifactCandidate{{Path: "result.txt", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	a := files[0]
	return task, []store.Artifact{{Path: "result.txt", StorageKey: a.StorageKey, SHA256: a.SHA256, Size: a.Size, MediaType: "text/plain"}}
}

func TestArtifactDatabaseCommitFailureDoesNotPublishSuccess(t *testing.T) {
	f := apiTestServer(t)
	task, metadata := archivedTask(t, f)
	conn, err := pgx.Connect(t.Context(), f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck // Fixture connection.
	if _, err = conn.Exec(t.Context(), `CREATE FUNCTION fail_artifact_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
CREATE CONSTRAINT TRIGGER fail_artifact_commit AFTER INSERT ON makez_artifacts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_artifact_commit()`); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Publish(t.Context(), task.WorkspaceID, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, metadata); err == nil {
		t.Fatal("commit fault silently succeeded")
	}
	current, err := f.db.Get(t.Context(), task.WorkspaceID, task.ID)
	if err != nil || current.State == core.TaskSucceeded {
		t.Fatal("failed commit published task success", err)
	}
	rows, err := f.db.Artifacts(t.Context(), task.WorkspaceID, task.ID)
	if err != nil || len(rows) != 0 {
		t.Fatal("uncommitted metadata downloadable", err)
	}
	file, err := f.files.OpenVerified(metadata[0].StorageKey, metadata[0].SHA256, metadata[0].Size)
	if err != nil {
		t.Fatal("published file was destroyed after commit failure", err)
	}
	_ = file.Close()
	if _, err = conn.Exec(t.Context(), `DROP TRIGGER fail_artifact_commit ON makez_artifacts`); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Publish(t.Context(), task.WorkspaceID, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, metadata); err != nil {
		t.Fatal("publication retry failed", err)
	}
	rows, err = f.db.Artifacts(t.Context(), task.WorkspaceID, task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatal("retry metadata count", err)
	}
	response := f.request(t, "GET", "/v1/artifacts/"+rows[0].ID+"/content", "", "", f.token)
	if response.Code != 200 || response.Body.String() != "verified artifact bytes" {
		t.Fatalf("retry download status %d", response.Code)
	}
}

func TestArtifactDownloadRejectsMissingAndCorruptFiles(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			f := apiTestServer(t)
			task, metadata := archivedTask(t, f)
			if err := f.db.Publish(t.Context(), task.WorkspaceID, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, metadata); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.artifactDir, metadata[0].StorageKey)
			var err error
			if scenario == "missing" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("tampered artifact bytes"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := f.db.Artifacts(t.Context(), task.WorkspaceID, task.ID)
			if err != nil || len(rows) != 1 {
				t.Fatal(err)
			}
			response := f.request(t, "GET", "/v1/artifacts/"+rows[0].ID+"/content", "", "", f.token)
			if response.Code != 500 || response.Header().Get("Content-Disposition") != "" || strings.Contains(response.Body.String(), "artifact bytes") {
				t.Fatalf("unsafe download status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

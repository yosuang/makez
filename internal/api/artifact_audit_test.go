package api

import (
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/reconcile"
	"github.com/makez-dev/makez/internal/store"
)

func TestArtifactAuditReportsMissingAndOrphansWithoutDeletion(t *testing.T) {
	f := apiTestServer(t)
	for _, scenario := range []string{"available", "missing"} {
		task := f.submit(t, scenario)
		task, err := f.db.InitializeRun(t.Context(), task.WorkspaceID, task.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		metadata := store.Artifact{Path: "result.txt", StorageKey: task.Run.ID + "/" + strings.Repeat("a", 64), SHA256: strings.Repeat("a", 64), Size: 1, MediaType: "text/plain"}
		if scenario == "available" {
			records, err := f.files.Archive(t.Context(), task.Run.ID, browserWorkspace{}, []core.ArtifactCandidate{{Path: "result.txt", Required: true}})
			if err != nil {
				t.Fatal(err)
			}
			metadata.StorageKey, metadata.SHA256, metadata.Size = records[0].StorageKey, records[0].SHA256, records[0].Size
		}
		for _, state := range []core.RunState{core.RunStopping, core.RunFinalizing} {
			if err = f.db.UpdateRun(t.Context(), task.Run.ID, store.RunUpdate{State: new(state)}); err != nil {
				t.Fatal(err)
			}
		}
		if err = f.db.Publish(t.Context(), task.WorkspaceID, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, []store.Artifact{metadata}); err != nil {
			t.Fatal(err)
		}
	}
	orphan, err := f.files.Archive(t.Context(), "uncommitted-run", browserWorkspace{}, []core.ArtifactCandidate{{Path: "result.txt", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconcile.Artifacts(t.Context(), f.db, f.files)
	if err != nil || report.Available != 1 || report.Missing != 1 || report.OrphanCandidates != 1 {
		t.Fatalf("integrity report: %+v %v", report, err)
	}
	file, err := f.files.Open(orphan[0].StorageKey)
	if err != nil {
		t.Fatal("audit removed an uncommitted object", err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

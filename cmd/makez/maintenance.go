package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/reconcile"

	"github.com/makez-dev/makez/internal/store"
)

// Maintenance commands are explicit operator actions; normal API and worker startup
// never performs migrations, task replay, or historical identity repair.
func maintenance(mode string, args []string) error {
	dsn := os.Getenv("MAKEZ_DATABASE_URL")
	if dsn == "" {
		return errors.New("MAKEZ_DATABASE_URL is required for maintenance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.CheckSchema(ctx); err != nil {
		return err
	}
	switch mode {
	case "verify-artifacts":
		if len(args) != 0 {
			return errors.New("usage: makez verify-artifacts")
		}
		files, err := artifact.New(env("MAKEZ_ARTIFACT_DIR", ".local/artifacts"))
		if err != nil {
			return err
		}
		defer files.Close() //nolint:errcheck // Read-only archive scan.
		report, err := reconcile.Artifacts(ctx, db, files)
		if err != nil {
			return err
		}
		fmt.Printf("available=%d missing=%d corrupt=%d orphan_candidates=%d\n", report.Available, report.Missing, report.Corrupt, report.OrphanCandidates)
		if report.Missing+report.Corrupt+report.OrphanCandidates > 0 {
			return errors.New("artifact integrity requires operator review; no files were changed")
		}
		return nil
	case "revoke-machine":
		if len(args) != 2 {
			return errors.New("usage: makez revoke-machine WORKSPACE_ID MACHINE_ID")
		}
		return db.RevokeMachine(ctx, args[0], args[1])
	case "repair-bootstrap":
		if len(args) != 1 {
			return errors.New("usage: makez repair-bootstrap USER_ID")
		}
		return db.RepairBootstrap(ctx, args[0])
	case "replay-outbox", "replay-authz-outbox":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: makez %s WORKSPACE_ID EVENT_ID [REASON]", mode)
		}
		queue := "task"
		if mode == "replay-authz-outbox" {
			queue = "authorization"
		}
		reason := "explicit operator replay"
		if len(args) == 3 {
			reason = args[2]
		}
		return db.ReplayDeadLetter(ctx, args[0], args[1], queue, reason)
	case "outbox-status":
		if len(args) != 1 {
			return errors.New("usage: makez outbox-status WORKSPACE_ID")
		}
		report, err := db.OutboxStatus(ctx, args[0])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	default:
		return errors.New("unsupported maintenance command")
	}
}

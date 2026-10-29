// Package reconcile provides read-only checks across durable metadata and files.
package reconcile

import (
	"context"
	"errors"
	"io/fs"

	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/store"
)

// ArtifactReport is an observation only. An orphan candidate may still be in the
// publication-to-database window and must never trigger automatic deletion.
type ArtifactReport struct{ Available, Missing, Corrupt, OrphanCandidates int64 }

// Artifacts verifies published objects and reports unmatched final names without GC.
func Artifacts(ctx context.Context, db *store.Store, files *artifact.Store) (ArtifactReport, error) {
	var report ArtifactReport
	after := ""
	for {
		rows, err := db.ArtifactAuditPage(ctx, after)
		if err != nil {
			return report, err
		}
		for _, row := range rows {
			after = row.ID
			file, err := files.OpenVerified(row.StorageKey, row.SHA256, row.Size)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				report.Missing++
			case err != nil:
				report.Corrupt++
			default:
				report.Available++
				if err = file.Close(); err != nil {
					return report, err
				}
			}
		}
		if len(rows) < 100 {
			break
		}
	}
	err := files.WalkPublished(ctx, func(key string) error {
		exists, err := db.ArtifactKeyExists(ctx, key)
		if err != nil {
			return err
		}
		if !exists {
			report.OrphanCandidates++
		}
		return nil
	})
	return report, err
}

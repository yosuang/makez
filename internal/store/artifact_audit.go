package store

import (
	"context"

	"github.com/makez-dev/makez/internal/store/dbgen"
)

// ArtifactAuditPage is a bounded, read-only operator scan, not a user API.
func (s *Store) ArtifactAuditPage(ctx context.Context, after string) ([]Artifact, error) {
	rows, err := dbgen.New(s.pool).ArtifactAuditPage(ctx, after)
	if err != nil {
		return nil, err
	}
	result := make([]Artifact, 0, len(rows))
	for _, r := range rows {
		result = append(result, artifactFromRow(r))
	}
	return result, nil
}

// ArtifactKeyExists checks whether a published object has any metadata reference.
func (s *Store) ArtifactKeyExists(ctx context.Context, key string) (bool, error) {
	return dbgen.New(s.pool).ArtifactKeyExists(ctx, key)
}

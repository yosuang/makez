package store

import "testing"

// TestAcceptedPromptCanSettle uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// The same run stays the writer. An accepted prompt settles when that
// run writes settled.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestAcceptedPromptCanSettle(t *testing.T) {
	sameRunMovesAcceptedTo(t, "settled")
}

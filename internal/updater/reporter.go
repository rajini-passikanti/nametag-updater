package updater

import (
	"context"

	"nametag-updater/internal/manifest"
)

// EventKind identifies an update lifecycle event.
type EventKind string

const (
	EventInstalled  EventKind = "installed"
	EventRejected   EventKind = "rejected"
	EventFailed     EventKind = "failed"
	EventConfirmed  EventKind = "confirmed"
	EventRolledBack EventKind = "rolled_back"
)

// Event is reported for every significant update outcome.
type Event struct {
	Kind EventKind
	From manifest.Version
	To   manifest.Version
	Err  error
}

// Reporter receives update events, for example to send them to a telemetry
// endpoint so that a bad rollout can be detected and halted. Events are
// already logged; a Reporter is for shipping them elsewhere.
type Reporter interface {
	Report(ctx context.Context, e Event)
}

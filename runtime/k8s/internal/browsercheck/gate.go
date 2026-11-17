package browsercheck

import (
	"context"
	"time"
)

// Gate is implemented by the original deployment claim's durable journal.
// A successful send fence permits exactly one CREATE; errors never permit a
// replay or adoption of an object later found under the expected name.
type Gate interface {
	CleanupDeadline() time.Time
	CleanupContext() context.Context
	BeforeCreate(context.Context, string, string) error
	AbortBeforeSend(context.Context, string, string) error
	AcknowledgePod(context.Context, string, string, string) error
	RecordEvidence(context.Context, Evidence) error
}

// CleanupGate authorizes only containment of a durably acknowledged resource.
// Its fresh controller lease cannot create Pods, health or deployment evidence.
type CleanupGate interface {
	CheckCleanup(context.Context) error
	RecordCleanup(context.Context) error
}

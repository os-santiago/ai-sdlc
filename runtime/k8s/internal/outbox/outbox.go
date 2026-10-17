// Package outbox implements the transactional outbox pattern (ADR-0002:
// "PostgreSQL stores durable workflow state and an outbox") that closes
// ADR-0004's "wiring to a real event bus" gap without adding a message
// broker (Kafka/NATS/Redis) as a new runtime dependency — see
// docs/architecture/adr/0005-runtime-k8s-event-bus-and-engine-invocation.md.
//
// Append writes an event/command record in the same database transaction
// as the producing component's own state write (internal/store.DB.WithTx),
// so a crash between "decision made" and "event recorded" is impossible.
// Relay polls unpublished records and delivers them over HTTP to the next
// component in the pipeline, using each component's own durable upsert as
// the idempotency boundary — redelivery is always safe.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Record is one outbox row: a fact (event) or a request (command) produced
// by a component, addressed by EventType to a destination in a Relay's
// routing table.
type Record struct {
	ID            string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       any
	CorrelationID string
}

// Append writes rec to the outbox table. Call it inside the same
// store.DB.WithTx transaction as the component's own state write — never
// on its own, or the atomicity guarantee this package exists for is lost.
func Append(ctx context.Context, q store.Querier, rec Record) error {
	payload, err := json.Marshal(rec.Payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	id := rec.ID
	if id == "" {
		id = store.NewID()
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO outbox (id, aggregate_type, aggregate_id, event_type, payload, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, rec.AggregateType, rec.AggregateID, rec.EventType, payload, rec.CorrelationID)
	if err != nil {
		return fmt.Errorf("append outbox record: %w", err)
	}
	return nil
}

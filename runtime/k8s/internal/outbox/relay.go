package outbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// DefaultRoutes mirrors ADR-0004/ADR-0005's four-stage event trail,
// addressed via localhost: deploy/kustomize/base/deployment.yaml
// co-locates all five containers (admission-controller, orchestrator,
// worker, release-manager, relay) in one pod sharing one network
// namespace (ADR-0001: "Initially co-locate them in a pod, preserving the
// option to split deployments without changing behavior"), so relay
// reaches its siblings over the loopback interface — no NetworkPolicy
// egress rule is needed for this traffic, and no Service hop either.
// If a deployment splits components into separate Deployments, override
// the affected entries via AI_SDLC_RELAY_ROUTES (a JSON object;
// components/relay/cmd/main.go merges it over these defaults) instead of
// changing this file.
var DefaultRoutes = map[string]string{
	"IssueAdmissionDecided.v1":        "http://localhost:8082/v1/plans/events/issue-admission-decided",
	"WorkItemReady.v1":                "http://localhost:8083/v1/attempts/dispatch",
	"ImplementationAttemptDecided.v1": "http://localhost:8082/v1/plans/events/implementation-attempt-decided",
	"ReleaseGateDecided.v1":           "http://localhost:8082/v1/plans/events/release-gate-decided",
}

// Relay polls the outbox table and delivers unpublished records to the
// routes table, over HTTP, with no message broker in between (ADR-0005).
type Relay struct {
	DB     *store.DB
	Routes map[string]string
	Client *http.Client
	Logger *slog.Logger

	// BatchSize bounds how many rows one PollOnce call claims. Defaults to
	// 20 when zero.
	BatchSize int
}

type outboxRow struct {
	ID            string
	EventType     string
	Payload       json.RawMessage
	CorrelationID string
}

// PollOnce claims up to BatchSize unpublished rows (SELECT ... FOR UPDATE
// SKIP LOCKED, safe with multiple Relay replicas), delivers each to its
// routed destination, and marks delivered rows published. It returns how
// many rows it successfully delivered. A delivery failure (unknown route,
// network error, non-2xx response) leaves that row unpublished for the
// next poll — this runtime never silently drops a decision (ADR-0003).
func (r *Relay) PollOnce(ctx context.Context) (int, error) {
	batchSize := r.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}
	logger := r.logger()

	var delivered int
	err := r.DB.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, event_type, payload, correlation_id
			FROM outbox
			WHERE published_at IS NULL
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		`, batchSize)
		if err != nil {
			return fmt.Errorf("select outbox batch: %w", err)
		}
		claimed := make([]outboxRow, 0, batchSize)
		for rows.Next() {
			var row outboxRow
			if err := rows.Scan(&row.ID, &row.EventType, &row.Payload, &row.CorrelationID); err != nil {
				rows.Close()
				return fmt.Errorf("scan outbox row: %w", err)
			}
			claimed = append(claimed, row)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate outbox batch: %w", err)
		}
		rows.Close()

		for _, row := range claimed {
			if err := r.deliver(ctx, row); err != nil {
				logger.Warn("outbox delivery failed; will retry on next poll",
					"eventType", row.EventType, "id", row.ID, "correlationId", row.CorrelationID, "error", err)
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE outbox SET published_at = now() WHERE id = $1`, row.ID); err != nil {
				return fmt.Errorf("mark outbox row %s published: %w", row.ID, err)
			}
			delivered++
		}
		return nil
	})
	return delivered, err
}

func (r *Relay) deliver(ctx context.Context, row outboxRow) error {
	url, ok := r.Routes[row.EventType]
	if !ok {
		return fmt.Errorf("no route configured for event type %q", row.EventType)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(row.Payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", row.ID)

	response, err := r.client().Do(request)
	if err != nil {
		return fmt.Errorf("deliver to %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("deliver to %s: unexpected status %d", url, response.StatusCode)
	}
	return nil
}

func (r *Relay) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (r *Relay) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Run polls on interval until ctx is cancelled. components/relay/cmd/main.go
// is the only caller; tests use PollOnce directly for determinism.
func (r *Relay) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.PollOnce(ctx); err != nil {
				r.logger().Error("outbox poll failed", "error", err)
			}
		}
	}
}

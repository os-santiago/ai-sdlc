package delivery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type Relay struct {
	DB       *store.DB
	Identity authority.Identity
	// Routes are trusted operation-to-URL configuration, never event data.
	Routes map[string]string
	Client *http.Client
}

// PollOnce holds at most one row lock and a bounded ten-second HTTP exchange.
// Receivers acknowledge durable commands promptly; execution happens elsewhere.
// Failed deliveries stop after eight attempts and retain the original fact.
func (relay Relay) PollOnce(ctx context.Context, p authority.Policy, now time.Time) (bool, error) {
	if relay.DB == nil || relay.Identity.Role != "relay" {
		return false, ErrUnauthorized
	}
	if err := p.Validate(); err != nil || p.Freeze {
		return false, ErrUnauthorized
	}
	delivered := false
	err := relay.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.AssertComponent(ctx, tx, "relay"); err != nil {
			return ErrUnauthorized
		}
		var producer, id, target, operation string
		var raw, payload []byte
		var attempts int
		err := tx.QueryRowContext(ctx, `SELECT producer,event_id,target,operation,receipt,payload,attempts FROM authorized_outbox
			WHERE published_at IS NULL AND NOT exhausted AND next_attempt_at <= $1
			ORDER BY next_attempt_at,created_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&producer, &id, &target, &operation, &raw, &payload, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return ErrStorage
		}
		var receipt authority.Receipt
		failure := "authority_denied"
		if err := authority.DecodeStrict(raw, &receipt); err == nil && receipt.Producer == producer && receipt.ID == id && p.VerifyReceipt(receipt, target, operation, payload) == nil {
			failure = relay.send(ctx, p, receipt, payload, now)
		}
		attempts++
		if failure == "" {
			_, err = tx.ExecContext(ctx, `UPDATE authorized_outbox SET published_at=$1,attempts=$2,last_error='' WHERE producer=$3 AND event_id=$4`, now, attempts, producer, id)
			delivered = true
		} else {
			delay := time.Duration(1<<attempts) * time.Second
			_, err = tx.ExecContext(ctx, `UPDATE authorized_outbox SET attempts=$1,next_attempt_at=$2,exhausted=$3,last_error=$4 WHERE producer=$5 AND event_id=$6`, attempts, now.Add(delay), attempts >= 8, failure, producer, id)
		}
		if err != nil {
			return ErrStorage
		}
		return nil
	})
	return delivered && err == nil, err
}

func (relay Relay) send(ctx context.Context, p authority.Policy, receipt authority.Receipt, payload []byte, now time.Time) string {
	endpoint, ok := relay.Routes[receipt.Operation]
	u, err := url.Parse(endpoint)
	if !ok || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
		return "route_invalid"
	}
	transport, err := relay.Identity.Carry(p, receipt, http.MethodPost, u.Path, now)
	if err != nil {
		return "identity_invalid"
	}
	raw, err := json.Marshal(Envelope{Receipt: receipt, Transport: transport, Payload: payload})
	if err != nil {
		return "envelope_invalid"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "route_invalid"
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	if relay.Client != nil {
		client = *relay.Client
	}
	if client.Timeout <= 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	// Never forward signed facts to an unreviewed redirect destination.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "transport_failed"
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "receiver_rejected"
	}
	ackRaw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(ackRaw) > 2*1024*1024 {
		return "acknowledgement_invalid"
	}
	var ack Acknowledged
	if err := authority.DecodeStrict(ackRaw, &ack); err != nil {
		return "acknowledgement_invalid"
	}
	if err := p.VerifyAcknowledgement(ack.Acknowledgement, receipt, transport, ack.Result); err != nil {
		return "acknowledgement_invalid"
	}
	return ""
}

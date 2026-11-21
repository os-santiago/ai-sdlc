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

// retryDelays spaces a command's eight delivery attempts (the schema's bound)
// over about 26 minutes: quick retries first, then long ones, so a target that
// is still starting (for example a model gateway benchmarking its upstream)
// still receives it. Commands carry their own expiry.
var retryDelays = [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Minute, 5 * time.Minute, 10 * time.Minute, 10 * time.Minute}

type Relay struct {
	DB       *store.DB
	Identity authority.Identity
	// Routes are trusted operation-to-URL configuration, never event data.
	Routes map[string]string
	Client *http.Client
}

type pending struct {
	producer, id, target, operation, lease string
	raw, payload                           []byte
	attempts                               int
}

// reserve commits the attempt and lease before any I/O. A crash consumes its
// budget and another replica may reclaim only after the lease expires.
func (relay Relay) reserve(ctx context.Context, p authority.Policy, now time.Time) (*pending, error) {
	if relay.DB == nil || relay.Identity.Role != "relay" {
		return nil, ErrUnauthorized
	}
	if err := relay.Identity.Validate(p); err != nil || p.Freeze {
		return nil, ErrUnauthorized
	}
	var item *pending
	err := relay.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.AssertComponent(ctx, tx, "relay"); err != nil {
			return ErrUnauthorized
		}
		row := pending{}
		err := tx.QueryRowContext(ctx, `SELECT producer,event_id,target,operation,receipt,payload,attempts FROM authorized_outbox
   WHERE published_at IS NULL AND NOT exhausted AND next_attempt_at <= $1 AND (lease_until IS NULL OR lease_until <= $1)
   ORDER BY next_attempt_at,created_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&row.producer, &row.id, &row.target, &row.operation, &row.raw, &row.payload, &row.attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return ErrStorage
		}
		row.attempts++
		row.lease = store.NewID()
		_, err = tx.ExecContext(ctx, `UPDATE authorized_outbox SET attempts=$1,exhausted=$2,lease_token=$3,lease_until=$4,next_attempt_at=$4,last_error='delivery_reserved' WHERE producer=$5 AND event_id=$6`, row.attempts, row.attempts >= 8, row.lease, now.Add(30*time.Second), row.producer, row.id)
		if err != nil {
			return ErrStorage
		}
		item = &row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}

// PollOnce reserves one durable attempt, performs at most ten seconds of HTTP,
// then conditionally records the result for that lease. No transaction spans
// external I/O. Eight reservations bound attempts even across process crashes.
func (relay Relay) PollOnce(ctx context.Context, p authority.Policy, now time.Time) (bool, error) {
	return relay.poll(ctx, p, nil, now)
}

// PollLoops reserves once under the shared freeze guard, then selects exactly
// one reviewed policy from the finite operation. Never call PollOnce twice with
// different policies: that would consume attempts for valid foreign-loop work.
func (relay Relay) PollLoops(ctx context.Context, inner, outer authority.Policy, now time.Time) (bool, error) {
	if authority.ValidateLoopPolicies(inner, outer) != nil || inner.Freeze || outer.Freeze || relay.Identity.Validate(outer) != nil {
		return false, ErrUnauthorized
	}
	return relay.poll(ctx, inner, &outer, now)
}

func (relay Relay) poll(ctx context.Context, p authority.Policy, outer *authority.Policy, now time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	started := time.Now()
	row, err := relay.reserve(ctx, p, now)
	if err != nil || row == nil {
		return false, err
	}
	if outer != nil && authority.OuterOperation(row.operation) {
		p = *outer
	}
	var receipt authority.Receipt
	failure := "authority_denied"
	if err := authority.DecodeStrict(row.raw, &receipt); err == nil && receipt.Producer == row.producer && receipt.ID == row.id && p.VerifyReceipt(receipt, row.target, row.operation, row.payload) == nil {
		failure = relay.send(ctx, p, receipt, row.payload, now)
	}
	return relay.finish(ctx, *row, failure, now.Add(time.Since(started)))
}

func (relay Relay) finish(ctx context.Context, row pending, failure string, now time.Time) (bool, error) {
	delivered := false
	err := relay.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.AssertComponent(ctx, tx, "relay"); err != nil {
			return ErrUnauthorized
		}
		var result sql.Result
		var err error
		if failure == "" {
			result, err = tx.ExecContext(ctx, `UPDATE authorized_outbox SET published_at=$1,exhausted=false,last_error='',lease_token=NULL,lease_until=NULL WHERE producer=$2 AND event_id=$3 AND lease_token=$4 AND lease_until > $1`, now, row.producer, row.id, row.lease)
		} else {
			delay := retryDelays[len(retryDelays)-1]
			if row.attempts >= 1 && row.attempts <= len(retryDelays) {
				delay = retryDelays[row.attempts-1]
			}
			result, err = tx.ExecContext(ctx, `UPDATE authorized_outbox SET next_attempt_at=$1,last_error=$2,lease_token=NULL,lease_until=NULL WHERE producer=$3 AND event_id=$4 AND lease_token=$5 AND lease_until > $6`, now.Add(delay), failure, row.producer, row.id, row.lease, now)
		}
		if err != nil {
			return ErrStorage
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return ErrStorage
		}
		delivered = failure == "" && changed == 1
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

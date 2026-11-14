// Package delivery provides the transactional signed inbox/outbox. It does not
// run generated code or perform external effects inside a database transaction.
package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

var (
	ErrUnauthorized = errors.New("delivery authority denied")
	ErrReplay       = errors.New("transport nonce already consumed")
	ErrConflict     = errors.New("event identity reused with different content")
	ErrStorage      = errors.New("delivery storage unavailable")
)

type Envelope struct {
	Receipt   authority.Receipt   `json:"receipt"`
	Transport authority.Transport `json:"transport"`
	Payload   []byte              `json:"payload"`
}

// Append seals and stores exact payload bytes in the caller's state transaction.
// Producers may append but cannot mutate an already recorded fact.
func Append(ctx context.Context, tx *sql.Tx, p authority.Policy, identity authority.Identity, id, operation string, payload []byte) error {
	if tx == nil {
		return ErrStorage
	}
	if err := store.AssertComponent(ctx, tx, identity.Role); err != nil {
		return ErrUnauthorized
	}
	r, err := identity.Seal(p, id, operation, payload)
	if err != nil {
		return ErrUnauthorized
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return ErrUnauthorized
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(producer,event_id) DO NOTHING`, r.Producer, r.ID, r.Target, r.Operation, raw, payload)
	if err != nil {
		return ErrStorage
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ErrStorage
	}
	if n == 1 {
		return nil
	}
	var previousRaw, previousPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT receipt,payload FROM authorized_outbox WHERE producer=$1 AND event_id=$2`, r.Producer, r.ID).Scan(&previousRaw, &previousPayload); err != nil {
		return ErrStorage
	}
	var previous authority.Receipt
	if err := authority.DecodeStrict(previousRaw, &previous); err != nil {
		return ErrStorage
	}
	canonical, _ := json.Marshal(previous)
	if authority.Digest(canonical) != authority.Digest(raw) || authority.Digest(previousPayload) != r.PayloadDigest {
		return ErrConflict
	}
	return nil
}

// Effect must validate business payload semantics and ownership, then use only
// this transaction for state and outgoing events. External effects belong to a
// durable command/lease reconciler, not to this callback or the HTTP request.
type Effect func(context.Context, *sql.Tx, authority.Receipt, []byte) ([]byte, error)

type Inbox struct {
	DB     *store.DB
	Target string
}

// Accept atomically consumes a nonce, deduplicates the durable receipt, applies
// the effect and saves the response. A crash before commit rolls everything
// back; redelivery with a fresh transport nonce returns the committed response.
func (in Inbox) Accept(ctx context.Context, p authority.Policy, envelope Envelope, operation, method, path string, now time.Time, effect Effect) ([]byte, bool, error) {
	if in.DB == nil || effect == nil {
		return nil, false, ErrStorage
	}
	if err := p.VerifyDelivery(envelope.Transport, envelope.Receipt, in.Target, operation, method, path, envelope.Payload, now); err != nil {
		return nil, false, ErrUnauthorized
	}
	r := envelope.Receipt
	transport := envelope.Transport
	raw, _ := json.Marshal(r)
	digest := authority.Digest(raw)
	var response []byte
	var duplicate bool
	err := in.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.AssertComponent(ctx, tx, in.Target); err != nil {
			return ErrUnauthorized
		}
		// Expired nonces can be removed because transport verification above
		// rejects them. No unexpired nonce is removed during retries.
		// Match VerifyTransport's whole-second clock boundary: a nonce remains
		// live for the entire final second in which its transport is accepted.
		if _, err := tx.ExecContext(ctx, `DELETE FROM authorized_nonces WHERE target=$1 AND valid_until < $2`, in.Target, time.Unix(now.Unix(), 0)); err != nil {
			return ErrStorage
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO authorized_nonces(target,caller,key_id,nonce,valid_until)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, in.Target, transport.Caller, transport.KeyID, transport.Nonce, time.Unix(transport.IssuedAt, 0).Add(300*time.Second))
		if err != nil {
			return ErrStorage
		}
		n, err := inserted.RowsAffected()
		if err != nil {
			return ErrStorage
		}
		if n == 0 {
			return ErrReplay
		}
		inserted, err = tx.ExecContext(ctx, `INSERT INTO authorized_inbox(target,producer,event_id,receipt_digest)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, in.Target, r.Producer, r.ID, digest)
		if err != nil {
			return ErrStorage
		}
		n, err = inserted.RowsAffected()
		if err != nil {
			return ErrStorage
		}
		if n == 0 {
			var previous string
			if err := tx.QueryRowContext(ctx, `SELECT receipt_digest,result FROM authorized_inbox WHERE target=$1 AND producer=$2 AND event_id=$3 FOR UPDATE`, in.Target, r.Producer, r.ID).Scan(&previous, &response); err != nil {
				return ErrStorage
			}
			if previous != digest {
				return ErrConflict
			}
			if len(response) == 0 {
				return ErrStorage
			}
			duplicate = true
			return nil
		}
		response, err = effect(ctx, tx, r, envelope.Payload)
		if err != nil {
			return err
		}
		if len(response) == 0 || len(response) > 1024*1024 || !json.Valid(response) {
			return errors.New("effect returned an invalid bounded JSON response")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE authorized_inbox SET result=$1 WHERE target=$2 AND producer=$3 AND event_id=$4`, response, in.Target, r.Producer, r.ID); err != nil {
			return ErrStorage
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return response, duplicate, nil
}

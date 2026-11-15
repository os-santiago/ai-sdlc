// Package intake converts authoritative GitHub snapshots into signed receipts.
// The poller has no external work-submission endpoint and never runs an engine.
package intake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type Source interface {
	Issues(context.Context, time.Time, int) ([]forge.Issue, error)
	Issue(context.Context, int) (forge.Issue, error)
	LabelActor(context.Context, int, string) (forge.Actor, int64, error)
	BaseSHA(context.Context, string) (string, error)
}
type Poller struct {
	DB        *store.DB
	Config    Config
	Source    Source
	Authority func() (authority.Policy, authority.Identity, error)
}
type lease struct {
	token    string
	since    sql.NullTime
	failures int
}

func (p Poller) current() (authority.Policy, authority.Identity, error) {
	if p.DB == nil || p.Source == nil || p.Authority == nil {
		return authority.Policy{}, authority.Identity{}, errors.New("intake dependencies required")
	}
	policy, identity, err := p.Authority()
	if err != nil {
		return policy, identity, err
	}
	if p.Config.Validate(policy.Repository) != nil || identity.Role != "intake" || identity.Validate(policy) != nil || policy.Allows("intake", "admission-controller", "admit-issue", p.Config.Repository, policy.Environment) != nil {
		return policy, identity, errors.New("intake authority denied")
	}
	return policy, identity, nil
}

func (p Poller) reserve(ctx context.Context, now time.Time) (*lease, error) {
	var selected *lease
	err := p.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.AssertComponent(ctx, tx, "intake"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO intake_cursors(repository,config_digest,next_poll) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, p.Config.Repository, p.Config.Digest(), now); err != nil {
			return err
		}
		var item lease
		err := tx.QueryRowContext(ctx, `SELECT watermark,failures FROM intake_cursors WHERE repository=$1 AND config_digest=$2 AND next_poll <= $3 AND (lease_until IS NULL OR lease_until <= $3) FOR UPDATE SKIP LOCKED`, p.Config.Repository, p.Config.Digest(), now).Scan(&item.since, &item.failures)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		item.token = store.NewID()
		if _, err = tx.ExecContext(ctx, `UPDATE intake_cursors SET lease_token=$1,lease_until=$2,next_poll=$2 WHERE repository=$3 AND config_digest=$4`, item.token, now.Add(120*time.Second), p.Config.Repository, p.Config.Digest()); err != nil {
			return err
		}
		selected = &item
		return nil
	})
	if err != nil {
		return nil, errors.New("intake cursor unavailable")
	}
	return selected, nil
}

// PollOnce bounds a complete scan to 90 seconds and retains its watermark on
// any partial failure. Committed revisions deduplicate a restarted scan. A
// two-minute overlap plus a scan-start watermark tolerates reordered updates.
func (p Poller) PollOnce(ctx context.Context, now time.Time) (int, error) {
	if _, _, err := p.current(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	reserved, err := p.reserve(ctx, now)
	if err != nil || reserved == nil {
		return 0, err
	}
	count, scanErr := p.scan(ctx, *reserved)
	// Finish uses its own short timeout so a timed-out scan still persists
	// bounded backoff. The lease predicate prevents a stale replica finishing.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	delay := time.Duration(p.Config.PollSeconds) * time.Second
	failures := 0
	if scanErr != nil {
		failures = reserved.failures + 1
		if failures > 20 {
			failures = 20
		}
		exponent := failures
		if exponent > 6 {
			exponent = 6
		}
		delay = time.Duration(1<<exponent) * 30 * time.Second
		if delay > time.Hour {
			delay = time.Hour
		}
		var api *forge.APIError
		if errors.As(scanErr, &api) && api.RetryAfter > delay {
			delay = api.RetryAfter
		}
	}
	result, finishErr := p.DB.Pool.ExecContext(finishCtx, `UPDATE intake_cursors SET watermark=CASE WHEN $1 THEN $2 ELSE watermark END,next_poll=$3,failures=$4,lease_token=NULL,lease_until=NULL WHERE repository=$5 AND config_digest=$6 AND lease_token=$7 AND lease_until>now()`, scanErr == nil, now, time.Now().Add(delay), failures, p.Config.Repository, p.Config.Digest(), reserved.token)
	if finishErr != nil {
		return count, errors.New("intake cursor completion failed")
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return count, errors.New("intake cursor lease lost")
	}
	return count, scanErr
}

func (p Poller) scan(ctx context.Context, reserved lease) (int, error) {
	since := time.Time{}
	if reserved.since.Valid {
		since = reserved.since.Time.Add(-2 * time.Minute)
	}
	count := 0
	for page := 1; page <= p.Config.MaxPages; page++ {
		issues, err := p.Source.Issues(ctx, since, page)
		if err != nil {
			return count, err
		}
		for _, listed := range issues {
			if listed.IsPR() {
				continue
			}
			var tracked bool
			if err := p.DB.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intake_revisions WHERE repository=$1 AND issue_number=$2)`, p.Config.Repository, listed.Number).Scan(&tracked); err != nil {
				return count, errors.New("intake revision lookup failed")
			}
			if !tracked && (!listed.HasLabel(p.Config.TriggerLabel) || listed.State != "open") {
				continue
			}
			snapshot, err := p.snapshot(ctx, listed.Number)
			if err != nil {
				return count, err
			}
			if !snapshot.Eligible && !tracked {
				continue
			}
			created, err := p.record(ctx, snapshot)
			if err != nil {
				return count, err
			}
			if created {
				count++
			}
		}
		if len(issues) < 100 {
			return count, nil
		}
	}
	return count, errors.New("intake pagination bound exceeded; watermark retained")
}

func (p Poller) snapshot(ctx context.Context, number int) (Snapshot, error) {
	first, err := p.Source.Issue(ctx, number)
	if err != nil {
		return Snapshot{}, err
	}
	actor, event, err := p.Source.LabelActor(ctx, number, p.Config.TriggerLabel)
	if err != nil {
		return Snapshot{}, err
	}
	last, err := p.Source.Issue(ctx, number)
	if err != nil {
		return Snapshot{}, err
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(last)
	if authority.Digest(a) != authority.Digest(b) {
		return Snapshot{}, errors.New("issue changed during authorization read")
	}
	snapshot := Snapshot{Version: 1, Repository: p.Config.Repository, Issue: last, ConfigDigest: p.Config.Digest(), AuthorizedActor: actor, LabelEventID: event}
	snapshot.Eligible = last.State == "open" && last.HasLabel(p.Config.TriggerLabel) && p.Config.AllowsActor(actor) && event > 0
	if snapshot.Eligible {
		snapshot.BaseSHA, err = p.Source.BaseSHA(ctx, p.Config.BaseBranch)
		if err != nil {
			return Snapshot{}, err
		}
	}
	snapshot.Revision = snapshot.ContentRevision()
	return snapshot, snapshot.Validate(p.Config)
}

func (p Poller) record(ctx context.Context, snapshot Snapshot) (bool, error) {
	policy, identity, err := p.current()
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return false, errors.New("intake serialization failed")
	}
	created := false
	err = p.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.AssertComponent(ctx, tx, "intake"); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO intake_revisions(repository,issue_number,revision,config_digest,event_id,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(repository,issue_number,revision,config_digest) DO NOTHING`, snapshot.Repository, snapshot.Issue.Number, snapshot.Revision, snapshot.ConfigDigest, snapshot.ID(), raw)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if err := delivery.Append(ctx, tx, policy, identity, snapshot.ID(), "admit-issue", raw); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return false, errors.New("intake atomic persistence failed")
	}
	return created, nil
}

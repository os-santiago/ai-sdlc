// Package intake converts authoritative GitHub snapshots into signed receipts.
// The poller has no external work-submission endpoint and never runs an engine.
package intake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
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
// complete reconciliation in immutable creation order avoids permanently
// excluding old issues displaced by concurrent edits or deletions. Watermark
// records successful scan time; it is deliberately not a filter on issue age.
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
		return count, scanFailure("cursor-completion", errors.New("intake cursor completion failed"))
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return count, scanFailure("cursor-lease", errors.New("intake cursor lease lost"))
	}
	return count, scanErr
}

func (p Poller) scan(ctx context.Context, reserved lease) (int, error) {
	count := 0
	for page := 1; page <= p.Config.MaxPages; page++ {
		issues, err := p.Source.Issues(ctx, time.Time{}, page)
		if err != nil {
			return count, scanFailure("list", err)
		}
		for _, listed := range issues {
			if listed.IsPR() {
				continue
			}
			var tracked bool
			if err := p.DB.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intake_revisions WHERE repository=$1 AND issue_number=$2)`, p.Config.Repository, listed.Number).Scan(&tracked); err != nil {
				return count, scanFailure("revision-lookup", errors.New("intake revision lookup failed"))
			}
			if !tracked && (!listed.HasLabel(p.Config.TriggerLabel) || listed.State != "open") {
				continue
			}
			if tracked && (!listed.HasLabel(p.Config.TriggerLabel) || listed.State != "open") {
				unchanged, err := p.unchangedIneligible(ctx, listed)
				if err != nil {
					return count, scanFailure("ineligible-check", err)
				}
				if unchanged {
					continue
				}
			}
			// Capture the start of the actual authoritative reads, not a later
			// database commit time that could follow a long row-lock wait.
			observedAt := time.Now().UTC()
			snapshot, err := p.snapshot(ctx, listed.Number)
			if err != nil {
				return count, scanFailure("snapshot", err)
			}
			if !snapshot.Eligible && !tracked {
				continue
			}
			created, err := p.record(ctx, snapshot, reserved.token, observedAt)
			if err != nil {
				return count, scanFailure("record", err)
			}
			if created {
				count++
			}
		}
		if len(issues) < 100 {
			return count, nil
		}
	}
	return count, scanFailure("pagination", errors.New("intake pagination bound exceeded; watermark retained"))
}

// unchangedIneligible reports whether a tracked issue that cannot be eligible
// (the listing shows it closed or without the trigger label) is recorded as
// an ineligible head under the current configuration and authority revision,
// with the same request content the listing shows: title, body, state,
// labels and author. Such an issue has no work any stage would accept, so
// re-reading it would only spend API calls and lengthen every scan. Each scan
// re-read every tracked issue, so it grew with the number of issues ever
// delivered until observations of active work aged past the evidence window
// (2 x pollSeconds). An open, labeled issue is never skipped: re-labeling by
// an authorized actor leaves its labels unchanged but can make it eligible.
func (p Poller) unchangedIneligible(ctx context.Context, listed forge.Issue) (bool, error) {
	policy, _, err := p.current()
	if err != nil {
		return false, err
	}
	var raw []byte
	err = p.DB.Pool.QueryRowContext(ctx, `SELECT r.payload FROM intake_heads h JOIN intake_revisions r ON r.repository=h.repository AND r.issue_number=h.issue_number AND r.generation=h.generation WHERE h.repository=$1 AND h.issue_number=$2 AND r.config_digest=$3`, p.Config.Repository, listed.Number, p.Config.Digest()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("intake head lookup failed")
	}
	var last Snapshot
	if json.Unmarshal(raw, &last) != nil {
		return false, nil
	}
	return !last.Eligible && last.AuthorityRevision == policy.Revision() && sameRequest(last.Issue, listed), nil
}

// sameRequest compares the request content a listing returns with a recorded
// issue. updated_at is deliberately ignored: comments and cross-references
// advance it without changing the request.
func sameRequest(a, b forge.Issue) bool {
	if a.Number != b.Number || a.Title != b.Title || a.Body != b.Body || a.State != b.State || a.User.ID != b.User.ID || len(a.Labels) != len(b.Labels) {
		return false
	}
	labels := map[string]int{}
	for _, l := range a.Labels {
		labels[l.Name]++
	}
	for _, l := range b.Labels {
		labels[l.Name]--
	}
	for _, n := range labels {
		if n != 0 {
			return false
		}
	}
	return true
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
	return snapshot, snapshot.validateContent(p.Config)
}

func (p Poller) record(ctx context.Context, snapshot Snapshot, leaseToken string, observedAt time.Time) (bool, error) {
	if observedAt.IsZero() || observedAt.After(time.Now().Add(5*time.Second)) {
		return false, errors.New("invalid issue observation time")
	}
	policy, identity, err := p.current()
	if err != nil {
		return false, err
	}
	if err := snapshot.validateContent(p.Config); err != nil {
		return false, err
	}
	snapshot.AuthorityRevision = policy.Revision()
	created := false
	err = p.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.AssertComponent(ctx, tx, "intake"); err != nil {
			return err
		}
		var held string
		if err := tx.QueryRowContext(ctx, `SELECT lease_token FROM intake_cursors WHERE repository=$1 AND config_digest=$2 AND lease_token=$3 AND lease_until>now() FOR SHARE`, p.Config.Repository, p.Config.Digest(), leaseToken).Scan(&held); err != nil {
			return errors.New("intake lease lost")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO intake_heads(repository,issue_number) VALUES($1,$2) ON CONFLICT DO NOTHING`, snapshot.Repository, snapshot.Issue.Number); err != nil {
			return err
		}
		var previous string
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT content_key,generation FROM intake_heads WHERE repository=$1 AND issue_number=$2 FOR UPDATE`, snapshot.Repository, snapshot.Issue.Number).Scan(&previous, &generation); err != nil {
			return err
		}
		contentKey := authority.Digest([]byte(snapshot.Revision + "\x00" + strconv.FormatBool(snapshot.Eligible) + "\x00" + snapshot.ConfigDigest + "\x00" + snapshot.AuthorityRevision))
		observe := func(generation int64) error {
			result, err := tx.ExecContext(ctx, `INSERT INTO intake_observations(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at)
				SELECT $1,$2,$3,$4,$5,$6,$7 FROM intake_cursors
				WHERE repository=$1 AND config_digest=$4 AND lease_token=$8 AND lease_until>clock_timestamp()
				ON CONFLICT(repository,issue_number) DO UPDATE
				SET (generation,config_digest,authority_revision,content_key,observed_at) =
				(EXCLUDED.generation,EXCLUDED.config_digest,EXCLUDED.authority_revision,EXCLUDED.content_key,EXCLUDED.observed_at)`,
				snapshot.Repository, snapshot.Issue.Number, generation, snapshot.ConfigDigest, snapshot.AuthorityRevision, contentKey, observedAt, leaseToken)
			if err != nil {
				return errors.New("issue observation persistence failed")
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return errors.New("issue observation lease expired")
			}
			return nil
		}
		if previous == contentKey {
			return observe(generation)
		}
		snapshot.Generation = generation + 1
		if err := snapshot.Validate(p.Config); err != nil {
			return err
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO intake_revisions(repository,issue_number,revision,generation,config_digest,event_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, snapshot.Repository, snapshot.Issue.Number, snapshot.Revision, snapshot.Generation, snapshot.ConfigDigest, snapshot.ID(), raw); err != nil {
			return err
		}
		if err := delivery.Append(ctx, tx, policy, identity, snapshot.ID(), "admit-issue", raw); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE intake_heads SET content_key=$1,generation=$2 WHERE repository=$3 AND issue_number=$4`, contentKey, snapshot.Generation, snapshot.Repository, snapshot.Issue.Number); err != nil {
			return err
		}
		created = true
		return observe(snapshot.Generation)
	})
	if err != nil {
		return false, errors.New("intake atomic persistence failed")
	}
	return created, nil
}

// scanError names the bounded scan step that failed, so an incomplete scan's
// log line says where it stopped (evaluation battery 29: two incomplete scans
// in two hours each aged the intake evidence past its window, and the log
// named neither cause). It unwraps to the original error, so callers and the
// Retry-After backoff see the same error as before.
type scanError struct {
	step  string
	cause error
}

func (e scanError) Error() string { return "intake scan stopped at " + e.step }
func (e scanError) Unwrap() error { return e.cause }

func scanFailure(step string, err error) error { return scanError{step: step, cause: err} }

// ScanStep returns a bounded description of why a scan was incomplete: its
// step, a GitHub status class when the forge refused, and "timeout" when the
// scan's 90-second bound ended. It never includes a response body, a URL or
// an issue's content.
func ScanStep(err error) string {
	if err == nil {
		return ""
	}
	step := "other"
	var failed scanError
	if errors.As(err, &failed) {
		step = failed.step
	}
	var api *forge.APIError
	switch {
	case errors.As(err, &api):
		class := "other"
		switch {
		case api.Status == 403 || api.Status == 429:
			class = strconv.Itoa(api.Status)
		case api.Status >= 500 && api.Status <= 599:
			class = "5xx"
		case api.Status >= 400 && api.Status <= 499:
			class = "4xx"
		}
		step += "-status-" + class
	case errors.Is(err, context.DeadlineExceeded):
		step += "-timeout"
	}
	return step
}

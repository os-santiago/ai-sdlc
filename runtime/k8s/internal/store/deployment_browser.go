package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func browserRequestID(c DeploymentClaim, phase string) string {
	raw, _ := json.Marshal([]string{"deployment-browser/v1", c.ID, c.Token, phase})
	return "browser-" + authority.Digest(raw)
}
func snapshotDigest(snapshot releasing.Snapshot) string {
	raw, _ := json.Marshal(snapshot)
	return authority.Digest(raw)
}

// BrowserRequest derives one fixed recipe under the original claim. It does
// not write intent or permit Kubernetes access. The send gate repeats bindings.
func BrowserRequest(c DeploymentClaim, profile browsercheck.Profile, phase string, snapshot releasing.Snapshot, rollout releasing.Rollout, until time.Time) (browsercheck.Request, error) {
	now := time.Now()
	if profile.Validate() != nil || !deploymentID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || (phase != "baseline" && phase != "advance" && phase != "rollback") {
		return browsercheck.Request{}, ErrExecution
	}
	end := now.Add(browsercheck.MaxDuration)
	for _, limit := range []time.Time{until, c.Until} {
		if limit.Before(end) {
			end = limit
		}
	}
	if end.Sub(now) < 10*time.Second {
		return browsercheck.Request{}, ErrExecution
	}
	expectation := profile.BaselineExpectation
	if phase == "advance" {
		expectation = profile.CandidateExpectation
	}
	parts := strings.Split(rollout.ImageID, "@")
	if len(parts) != 2 {
		return browsercheck.Request{}, ErrExecution
	}
	r := browsercheck.Request{Version: browsercheck.RequestVersion, RequestID: browserRequestID(c, phase), TargetSHA256: snapshot.TargetSHA256, SnapshotSHA256: snapshotDigest(snapshot), ImageDigest: parts[1], IssuedAt: now.UnixMilli(), ExpiresAt: end.UnixMilli(), Expectation: expectation}
	if r.Validate(now) != nil {
		return browsercheck.Request{}, ErrExecution
	}
	return r, nil
}

// BrowserGate owns no transport and cannot execute a user-supplied command.
// The adapter compiles the fixed Pod and supplies its complete content digest.
type BrowserGate struct {
	CleanupParent context.Context
	Journal       *DeploymentJournal
	Claim         DeploymentClaim
	Phase         string
	Snapshot      releasing.Snapshot
	Rollout       releasing.Rollout
	Request       browsercheck.Request
}

func (g BrowserGate) check(ctx context.Context, tx *sql.Tx) (releasing.Configuration, error) {
	if g.Journal == nil || (g.Phase != "baseline" && g.Phase != "advance" && g.Phase != "rollback") {
		return releasing.Configuration{}, ErrExecution
	}
	cfg, input, e := g.Journal.check(ctx, tx, g.Claim, g.Phase == "rollback")
	if e != nil {
		return cfg, e
	}
	r, s, p := g.Request, g.Snapshot, g.Rollout
	now := time.Now()
	expected := cfg.Target.BaselineImage
	runtime := cfg.Target.BaselineRuntimeDigest
	expectation := cfg.Browser.BaselineExpectation
	if g.Phase == "advance" {
		expected = input.Promotion.Plan.Image
		runtime = strings.TrimPrefix(expected, building.Registry+"@")
		expectation = cfg.Browser.CandidateExpectation
	}
	if r.Validate(now) != nil || r.RequestID != browserRequestID(g.Claim, g.Phase) || r.TargetSHA256 != cfg.Target.Digest() || r.SnapshotSHA256 != snapshotDigest(s) || r.ImageDigest != runtime || r.Expectation != expectation || r.ExpiresAt > g.Claim.Until.UnixMilli() || r.IssuedAt < input.Command.IssuedAt || s.TargetSHA256 != cfg.Target.Digest() || s.DeploymentUID != cfg.Target.DeploymentUID || s.Image != expected || (g.Phase == "baseline" && !freshSnapshot(s, now)) || p.DeploymentUID != s.DeploymentUID || p.Generation != s.Generation || p.TargetSHA256 != s.TargetSHA256 || p.Image != s.Image || p.ServiceUID != cfg.Target.ServiceUID || p.ObservedAt < s.ObservedAt || p.ObservedAt > r.IssuedAt || !strings.HasSuffix(p.ImageID, "@"+runtime) {
		return cfg, ErrExecution
	}
	if g.Phase == "baseline" {
		var effects bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_intents WHERE request_id=$1)`, g.Claim.ID).Scan(&effects) != nil || effects {
			return cfg, ErrExecution
		}
	} else {
		receipt, e := readDeploymentReceipt(ctx, tx, g.Claim, g.Phase, cfg, input)
		if e != nil || receipt.After != s {
			return cfg, ErrExecution
		}
		if g.Phase == "advance" {
			var rollback bool
			if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_intents WHERE request_id=$1 AND kind='rollback')`, g.Claim.ID).Scan(&rollback) != nil || rollback {
				return cfg, ErrExecution
			}
		}
	}
	return cfg, nil
}
func (g BrowserGate) BeforeCreate(ctx context.Context, podDigest string) error {
	if g.Journal == nil || g.Journal.DB == nil || !workspacebundle.ValidDigest(podDigest) {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := g.Journal.WithTx(ctx, func(tx *sql.Tx) error {
		cfg, e := g.check(ctx, tx)
		if e != nil {
			return e
		}
		raw, _ := json.Marshal(g.Request)
		snapshot, _ := json.Marshal(g.Snapshot)
		rollout, _ := json.Marshal(g.Rollout)
		_, e = tx.ExecContext(ctx, `INSERT INTO deployment_browser_intents(id,deployment_id,phase,claim_token,request,request_digest,snapshot,rollout,profile_digest,pod_digest,expires_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, g.Request.RequestID, g.Claim.ID, g.Phase, g.Claim.Token, raw, g.Request.Digest(), snapshot, rollout, cfg.Browser.Digest(), podDigest, time.UnixMilli(g.Request.ExpiresAt))
		return e
	})
	if e != nil {
		return ErrExecution
	}
	e = g.Journal.WithTx(ctx, func(tx *sql.Tx) error {
		if _, e := g.check(ctx, tx); e != nil {
			return e
		}
		if _, e := g.readIntent(ctx, tx, podDigest); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO deployment_browser_sends(id,claim_token)VALUES($1,$2)`, g.Request.RequestID, g.Claim.Token)
		return e
	})
	if e != nil {
		return ErrExecution
	}
	return g.Journal.WithTx(ctx, func(tx *sql.Tx) error { _, e := g.check(ctx, tx); return e })
}
func (g BrowserGate) readIntent(ctx context.Context, q Querier, podDigest string) (time.Time, error) {
	var raw, snapshot, rollout []byte
	var hash, profile, compiled string
	var expiry, created time.Time
	if q.QueryRowContext(ctx, `SELECT request,request_digest,snapshot,rollout,profile_digest,pod_digest,expires_at,created_at FROM deployment_browser_intents WHERE id=$1 AND deployment_id=$2 AND phase=$3 AND claim_token=$4`, g.Request.RequestID, g.Claim.ID, g.Phase, g.Claim.Token).Scan(&raw, &hash, &snapshot, &rollout, &profile, &compiled, &expiry, &created) != nil {
		return created, ErrExecution
	}
	var r browsercheck.Request
	var s releasing.Snapshot
	var p releasing.Rollout
	cfg, _, e := deploymentArchived(ctx, q, g.Claim)
	if e != nil || authority.DecodeStrict(raw, &r) != nil || authority.DecodeStrict(snapshot, &s) != nil || authority.DecodeStrict(rollout, &p) != nil || r != g.Request || s != g.Snapshot || p != g.Rollout || hash != authority.Digest(raw) || hash != r.Digest() || profile != cfg.Browser.Digest() || !workspacebundle.ValidDigest(compiled) || (podDigest != "" && compiled != podDigest) || !expiry.Equal(time.UnixMilli(r.ExpiresAt)) || created.UnixMilli() < r.IssuedAt || !created.Before(expiry) {
		return created, ErrExecution
	}
	return created, nil
}
func (g BrowserGate) AcknowledgePod(ctx context.Context, id, podUID string) error {
	if g.Journal == nil || g.Journal.DB == nil || id != g.Request.RequestID || !lifecycleUUID.MatchString(podUID) {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return g.Journal.WithTx(ctx, func(tx *sql.Tx) error {
		created, e := g.readIntent(ctx, tx, "")
		if e != nil {
			return e
		}
		var sent time.Time
		if tx.QueryRowContext(ctx, `SELECT created_at FROM deployment_browser_sends WHERE id=$1 AND claim_token=$2`, id, g.Claim.Token).Scan(&sent) != nil || sent.Before(created) || !sent.Before(time.UnixMilli(g.Request.ExpiresAt)) || time.Now().UnixMilli() >= g.Request.ExpiresAt {
			return ErrExecution
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO deployment_browser_pods(id,pod_uid)VALUES($1,$2)`, id, podUID)
		return e
	})
}
func (g BrowserGate) RecordEvidence(ctx context.Context, evidence browsercheck.Evidence) error {
	if g.Journal == nil || g.Journal.DB == nil {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return g.Journal.WithTx(ctx, func(tx *sql.Tx) error {
		cfg, e := g.check(ctx, tx)
		if e != nil {
			return e
		}
		if _, e = g.readIntent(ctx, tx, ""); e != nil {
			return e
		}
		var podUID string
		var ack time.Time
		if tx.QueryRowContext(ctx, `SELECT pod_uid::text,recorded_at FROM deployment_browser_pods WHERE id=$1`, g.Request.RequestID).Scan(&podUID, &ack) != nil || evidence.PodUID != podUID || evidence.Request != g.Request || evidence.Validate(cfg.Browser, time.Now()) != nil || evidence.DeletedAt < ack.UnixMilli() {
			return ErrExecution
		}
		raw, _ := json.Marshal(evidence)
		var at time.Time
		if tx.QueryRowContext(ctx, `INSERT INTO deployment_browser_evidence(id,evidence,digest)VALUES($1,$2,$3) RETURNING recorded_at`, g.Request.RequestID, raw, evidence.Digest()).Scan(&at) != nil || evidence.Validate(cfg.Browser, at) != nil {
			return ErrExecution
		}
		if _, e = g.check(ctx, tx); e != nil {
			return e
		}
		return evidence.Validate(cfg.Browser, time.Now())
	})
}

// Cleanup authority is bounded by the original claim; it cannot renew smoke or deployment.
func (g BrowserGate) CleanupDeadline() time.Time { return g.Claim.Until }

func (g BrowserGate) CleanupContext() context.Context { return g.CleanupParent }

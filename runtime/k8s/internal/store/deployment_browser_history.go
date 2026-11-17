package store

import (
	"context"
	"database/sql"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"strings"
	"time"
)

func (j *DeploymentJournal) ReadDeploymentContainmentInput(ctx context.Context, c DeploymentClaim) (releasing.Configuration, DeploymentInput, error) {
	var cfg releasing.Configuration
	var input DeploymentInput
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.WithTx(ctx, func(tx *sql.Tx) error { var e error; cfg, input, e = j.check(ctx, tx, c, true); return e })
	if e != nil {
		return releasing.Configuration{}, DeploymentInput{}, ErrExecution
	}
	return cfg, input, nil
}

type browserRecord struct {
	Evidence   browsercheck.Evidence
	Snapshot   releasing.Snapshot
	Rollout    releasing.Rollout
	RecordedAt time.Time
}

// Caller has already authenticated the complete original deployment command and
// its role. History is validated at original recording time, never re-authorized.
func readBrowserRecord(ctx context.Context, q Querier, c DeploymentClaim, phase string, cfg releasing.Configuration, input DeploymentInput) (browserRecord, error) {
	var record browserRecord
	if phase != "baseline" && phase != "advance" && phase != "rollback" {
		return record, ErrExecution
	}
	var requestRaw, snapshotRaw, rolloutRaw, evidenceRaw []byte
	var requestHash, profileHash, podHash, podUID, evidenceHash string
	var created, sent, ack, expiry time.Time
	id := browserRequestID(c, phase)
	e := q.QueryRowContext(ctx, `SELECT i.request,i.request_digest,i.snapshot,i.rollout,i.profile_digest,i.pod_digest,i.created_at,i.expires_at,s.created_at,p.pod_uid::text,p.recorded_at,e.evidence,e.digest,e.recorded_at FROM deployment_browser_intents i JOIN deployment_browser_sends s ON s.id=i.id AND s.claim_token=i.claim_token JOIN deployment_browser_pods p ON p.id=i.id AND NOT p.not_sent JOIN deployment_browser_evidence e ON e.id=i.id WHERE i.id=$1 AND i.deployment_id=$2 AND i.phase=$3 AND i.claim_token=$4 AND e.recorded_at<=clock_timestamp()`, id, c.ID, phase, c.Token).Scan(&requestRaw, &requestHash, &snapshotRaw, &rolloutRaw, &profileHash, &podHash, &created, &expiry, &sent, &podUID, &ack, &evidenceRaw, &evidenceHash, &record.RecordedAt)
	var request browsercheck.Request
	if e != nil || authority.DecodeStrict(requestRaw, &request) != nil || authority.DecodeStrict(snapshotRaw, &record.Snapshot) != nil || authority.DecodeStrict(rolloutRaw, &record.Rollout) != nil || authority.DecodeStrict(evidenceRaw, &record.Evidence) != nil {
		return browserRecord{}, ErrExecution
	}
	ev, s, p := record.Evidence, record.Snapshot, record.Rollout
	expected, runtime, expectation := cfg.Target.BaselineImage, cfg.Target.BaselineRuntimeDigest, cfg.Browser.BaselineExpectation
	if phase == "advance" {
		expected = input.Promotion.Plan.Image
		runtime = strings.TrimPrefix(expected, building.Registry+"@")
		expectation = cfg.Browser.CandidateExpectation
	}
	if request.RequestID != id || request.Validate(created) != nil || request.IssuedAt < input.Command.IssuedAt || request.ExpiresAt > c.Until.UnixMilli() || requestHash != authority.Digest(requestRaw) || requestHash != request.Digest() || profileHash != cfg.Browser.Digest() || !workspacebundle.ValidDigest(podHash) || !expiry.Equal(time.UnixMilli(request.ExpiresAt)) || created.UnixMilli() < request.IssuedAt || sent.Before(created) || ack.Before(sent) || record.RecordedAt.Before(ack) || ev.Request != request || ev.PodUID != podUID || evidenceHash != authority.Digest(evidenceRaw) || evidenceHash != ev.Digest() || ev.Validate(cfg.Browser, record.RecordedAt) != nil || ev.Result.StartedAt < sent.UnixMilli() || ev.DeletedAt < ack.UnixMilli() || request.TargetSHA256 != cfg.Target.Digest() || request.SnapshotSHA256 != snapshotDigest(s) || request.ImageDigest != runtime || request.Expectation != expectation || s.TargetSHA256 != cfg.Target.Digest() || s.DeploymentUID != cfg.Target.DeploymentUID || s.Image != expected || p.DeploymentUID != s.DeploymentUID || p.Generation != s.Generation || p.Image != s.Image || p.TargetSHA256 != s.TargetSHA256 || p.ServiceUID != cfg.Target.ServiceUID || p.ObservedAt < s.ObservedAt || p.ObservedAt > request.IssuedAt || !strings.HasSuffix(p.ImageID, "@"+runtime) {
		return browserRecord{}, ErrExecution
	}
	return record, nil
}

func validateBrowserBaseline(ctx context.Context, q Querier, c DeploymentClaim, cfg releasing.Configuration, input DeploymentInput, before releasing.Snapshot, at time.Time) error {
	record, e := readBrowserRecord(ctx, q, c, "baseline", cfg, input)
	// A fresh read may change only the observation timestamp; deployment resource
	// version/generation/image and original target must remain exactly the same.
	expected := record.Snapshot
	expected.ObservedAt = before.ObservedAt
	if e != nil || expected != before || record.RecordedAt.After(at) || before.ObservedAt < record.Snapshot.ObservedAt {
		return ErrExecution
	}
	return nil
}
func validateDeploymentBrowser(ctx context.Context, q Querier, c DeploymentClaim, cfg releasing.Configuration, input DeploymentInput, receipt releasing.Receipt, health releasing.Health, at time.Time) error {
	record, e := readBrowserRecord(ctx, q, c, effectKind(receipt.Rollback), cfg, input)
	route := record.Rollout
	route.ObservedAt = health.Rollout.ObservedAt
	if at.UnixMilli() >= health.Browser.Request.ExpiresAt || at.UnixMilli() >= health.StartedAt+browsercheck.MaxDuration.Milliseconds() {
		return ErrExecution
	}
	if e != nil || deployment.ValidateBrowserHealth(cfg.Browser, cfg.Target, receipt, health) != nil || record.Snapshot != receipt.After || record.Evidence.Digest() != health.Browser.Digest() || route != health.Rollout || record.RecordedAt.After(at) || record.RecordedAt.UnixMilli() > health.ObservedAt || health.StartedAt <= 0 || health.StartedAt > record.Rollout.ObservedAt || health.Browser.Request.IssuedAt < health.StartedAt || health.Browser.Request.ExpiresAt > health.StartedAt+browsercheck.MaxDuration.Milliseconds() || health.ObservedAt-health.StartedAt >= browsercheck.MaxDuration.Milliseconds() || health.ObservedAt < health.Browser.DeletedAt {
		return ErrExecution
	}
	advance, _, sent, e := readDeploymentIntent(ctx, q, c, "advance")
	if e != nil || validateBrowserBaseline(ctx, q, c, cfg, input, advance, sent) != nil {
		return ErrExecution
	}
	return nil
}

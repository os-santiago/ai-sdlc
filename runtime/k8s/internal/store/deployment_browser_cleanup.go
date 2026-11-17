package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"strings"
	"time"
)

// BrowserCleanup is resource containment, never deployment authority. The
// controller may acquire a new finite cleanup context after the smoke expires;
// only the Pod UID already acknowledged in the original journal can be removed.
type BrowserCleanup struct {
	Claim    DeploymentClaim
	Phase    string
	Request  browsercheck.Request
	Snapshot releasing.Snapshot
	Rollout  releasing.Rollout
	Profile  browsercheck.Profile
	Target   releasing.Target
	PodUID   string
}

func (c BrowserCleanup) digest() string { raw, _ := json.Marshal(c); return authority.Digest(raw) }

func readBrowserCleanup(ctx context.Context, q Querier, id string) (BrowserCleanup, error) {
	var c BrowserCleanup
	if AssertComponent(ctx, q, "deployer") != nil {
		return c, ErrExecution
	}
	var request, snapshot, rollout []byte
	var ack, sent time.Time
	e := q.QueryRowContext(ctx, `SELECT d.id,i.claim_token::text,d.expires_at,i.phase,i.request,i.snapshot,i.rollout,p.pod_uid::text,p.recorded_at,s.created_at FROM deployment_browser_intents i JOIN deployment_requests d ON d.id=i.deployment_id AND d.claim_token=i.claim_token JOIN deployment_browser_sends s ON s.id=i.id AND s.claim_token=i.claim_token JOIN deployment_browser_pods p ON p.id=i.id WHERE i.id=$1 AND i.expires_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM deployment_browser_evidence e WHERE e.id=i.id) AND NOT EXISTS(SELECT 1 FROM deployment_browser_cleanups c WHERE c.id=i.id)`, id).Scan(&c.Claim.ID, &c.Claim.Token, &c.Claim.Until, &c.Phase, &request, &snapshot, &rollout, &c.PodUID, &ack, &sent)
	if e != nil || authority.DecodeStrict(request, &c.Request) != nil || authority.DecodeStrict(snapshot, &c.Snapshot) != nil || authority.DecodeStrict(rollout, &c.Rollout) != nil || c.Request.RequestID != id || id != browserRequestID(c.Claim, c.Phase) || !lifecycleUUID.MatchString(c.PodUID) {
		return BrowserCleanup{}, ErrExecution
	}
	cfg, input, e := deploymentArchived(ctx, q, c.Claim)
	if e != nil {
		return BrowserCleanup{}, e
	}
	c.Profile, c.Target = cfg.Browser, cfg.Target
	g := BrowserGate{Claim: c.Claim, Phase: c.Phase, Request: c.Request, Snapshot: c.Snapshot, Rollout: c.Rollout}
	created, e := g.readIntent(ctx, q, "")
	if e != nil || sent.Before(created) || ack.Before(sent) || ack.UnixMilli() >= c.Request.ExpiresAt || c.Request.Validate(created) != nil || c.Request.ExpiresAt > c.Claim.Until.UnixMilli() || c.Request.IssuedAt < input.Command.IssuedAt || c.Request.TargetSHA256 != cfg.Target.Digest() || c.Request.SnapshotSHA256 != snapshotDigest(c.Snapshot) || c.Snapshot.DeploymentUID != cfg.Target.DeploymentUID || c.Snapshot.TargetSHA256 != cfg.Target.Digest() {
		return BrowserCleanup{}, ErrExecution
	}
	image, runtime, expectation := cfg.Target.BaselineImage, cfg.Target.BaselineRuntimeDigest, cfg.Browser.BaselineExpectation
	if c.Phase == "advance" {
		image = input.Promotion.Plan.Image
		runtime = strings.TrimPrefix(image, building.Registry+"@")
		expectation = cfg.Browser.CandidateExpectation
	}
	if c.Request.Expectation != expectation || c.Request.ImageDigest != runtime || c.Snapshot.Image != image || c.Rollout.Image != image || !strings.HasSuffix(c.Rollout.ImageID, "@"+runtime) || c.Rollout.DeploymentUID != c.Snapshot.DeploymentUID || c.Rollout.Generation != c.Snapshot.Generation || c.Rollout.TargetSHA256 != cfg.Target.Digest() || c.Rollout.ServiceUID != cfg.Target.ServiceUID {
		return BrowserCleanup{}, ErrExecution
	}
	if c.Phase != "baseline" {
		receipt, e := readDeploymentReceipt(ctx, q, c.Claim, c.Phase, cfg, input)
		if e != nil || receipt.After != c.Snapshot {
			return BrowserCleanup{}, ErrExecution
		}
	}
	return c, nil
}
func (j *DeploymentJournal) NextBrowserCleanup(ctx context.Context) (BrowserCleanup, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if j == nil || j.DB == nil || AssertComponent(ctx, j.Pool, "deployer") != nil {
		return BrowserCleanup{}, ErrExecution
	}
	var id string
	e := j.Pool.QueryRowContext(ctx, `SELECT i.id FROM deployment_browser_intents i JOIN deployment_browser_pods p ON p.id=i.id WHERE i.expires_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM deployment_browser_evidence e WHERE e.id=i.id) AND NOT EXISTS(SELECT 1 FROM deployment_browser_cleanups c WHERE c.id=i.id) ORDER BY i.created_at,i.id LIMIT 1`).Scan(&id)
	if e == sql.ErrNoRows {
		return BrowserCleanup{}, nil
	}
	if e != nil {
		return BrowserCleanup{}, ErrExecution
	}
	return readBrowserCleanup(ctx, j.Pool, id)
}

// Every call inherits the newly acquired controller session and a single ten
// second deadline. This never extends the original command or browser request.
type BrowserCleanupGate struct {
	Journal   *DeploymentJournal
	Original  BrowserCleanup
	StartedAt time.Time
	Until     time.Time
}

func (g BrowserCleanupGate) CheckCleanup(ctx context.Context) error {
	if g.Journal == nil || g.Journal.DB == nil || ctx.Err() != nil || g.StartedAt.IsZero() || g.StartedAt.After(time.Now()) || !g.Until.After(time.Now()) || g.Until.Sub(g.StartedAt) > browsercheck.CleanupBudget {
		return ErrExecution
	}
	until, ok := ctx.Deadline()
	if !ok || until.After(g.Until) {
		return ErrExecution
	}
	original, e := readBrowserCleanup(ctx, g.Journal.Pool, g.Original.Request.RequestID)
	if e != nil || original.digest() != g.Original.digest() {
		return ErrExecution
	}
	if g.Journal.browserCleanupIdentity(ctx, original) != nil {
		return ErrExecution
	}
	return nil
}
func (g BrowserCleanupGate) RecordCleanup(ctx context.Context) error {
	if g.CheckCleanup(ctx) != nil {
		return ErrExecution
	}
	return g.Journal.WithTx(ctx, func(tx *sql.Tx) error {
		original, e := readBrowserCleanup(ctx, tx, g.Original.Request.RequestID)
		if e != nil || original.digest() != g.Original.digest() {
			return ErrExecution
		}
		var at time.Time
		if tx.QueryRowContext(ctx, `INSERT INTO deployment_browser_cleanups(id,pod_uid)VALUES($1,$2) RETURNING recorded_at`, original.Request.RequestID, original.PodUID).Scan(&at) != nil || !at.Before(g.Until) || !time.Now().Before(g.Until) || ctx.Err() != nil {
			return ErrExecution
		}
		// Configuration is checked independently of advance freeze/old claim expiry.
		if g.Journal.browserCleanupIdentity(ctx, original) != nil || !time.Now().Before(g.Until) {
			return ErrExecution
		}
		return nil
	})
}

func (j *DeploymentJournal) browserCleanupIdentity(ctx context.Context, original BrowserCleanup) error {
	var target releasing.Target
	var profile browsercheck.Profile
	var e error
	if j.cleanupTarget != nil {
		target, e = j.cleanupTarget()
	} else {
		var cfg releasing.Configuration
		cfg, e = j.configuration()
		target = cfg.Target
	}
	if e != nil || ctx.Err() != nil || target.Digest() != original.Target.Digest() || target.Validate(time.UnixMilli(target.ApprovedAt)) != nil {
		return ErrExecution
	}
	if j.browser != nil {
		profile, e = j.browser()
	} else {
		var cfg releasing.Configuration
		cfg, e = j.configuration()
		profile = cfg.Browser
	}
	if e != nil || ctx.Err() != nil || profile.Digest() != original.Profile.Digest() || profile.Validate() != nil {
		return ErrExecution
	}
	return nil
}

// PendingBrowserCleanup prevents a newer claim racing an original browser
// that is still within its observation window. Unknown CREATE acknowledgements
// are separately quarantined; this query cannot adopt them by name.
func (j *DeploymentJournal) PendingBrowserCleanup(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if j == nil || j.DB == nil || AssertComponent(ctx, j.Pool, "deployer") != nil {
		return false, ErrExecution
	}
	var pending bool
	e := j.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_browser_intents i JOIN deployment_browser_sends s ON s.id=i.id WHERE NOT EXISTS(SELECT 1 FROM deployment_browser_evidence e WHERE e.id=i.id) AND NOT EXISTS(SELECT 1 FROM deployment_browser_cleanups c WHERE c.id=i.id))`).Scan(&pending)
	return pending, e
}

func (j *DeploymentJournal) UnacknowledgedBrowserCreate(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if j == nil || j.DB == nil || AssertComponent(ctx, j.Pool, "deployer") != nil {
		return false, ErrExecution
	}
	var unresolved bool
	e := j.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_browser_intents i JOIN deployment_browser_sends s ON s.id=i.id WHERE NOT EXISTS(SELECT 1 FROM deployment_browser_pods p WHERE p.id=i.id))`).Scan(&unresolved)
	return unresolved, e
}

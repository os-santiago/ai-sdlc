package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"regexp"
	"time"
)

var deploymentID = regexp.MustCompile(`^deploy-[a-f0-9]{64}$`)

type DeploymentJournal struct {
	*DB
	load          func() (releasing.Configuration, error)
	target        func() (releasing.Target, error)
	cleanupTarget func() (releasing.Target, error)
	browser       func() (browsercheck.Profile, error)
}

func NewDeploymentJournal(db *DB, load func() (releasing.Configuration, error)) *DeploymentJournal {
	return &DeploymentJournal{DB: db, load: load}
}

type DeploymentInput struct {
	Command    deployment.Command
	Promotion  PromotionRecord
	Provenance ProvenanceRecord
	Build      BuiltRecord
}
type DeploymentClaim struct {
	ID, Token string
	Until     time.Time
}

func (j *DeploymentJournal) configuration() (releasing.Configuration, error) {
	var out releasing.Configuration
	if j == nil || j.DB == nil || j.load == nil {
		return out, ErrExecution
	}
	cfg, e := j.load()
	if e != nil {
		return out, ErrExecution
	}
	raw, e := json.Marshal(cfg)
	if e != nil || len(raw) > 327680 || authority.DecodeStrict(raw, &out) != nil {
		return releasing.Configuration{}, ErrExecution
	}
	return out, nil
}
func (j *DeploymentJournal) current(raw, signed []byte) (releasing.Configuration, deployment.Command, error) {
	cfg, e := j.configuration()
	var c deployment.Command
	var receipt authority.Receipt
	if e != nil || len(signed) == 0 || len(signed) > 4096 || authority.DecodeStrict(signed, &receipt) != nil {
		return cfg, c, ErrExecution
	}
	c, e = deployment.VerifyCommand(cfg, receipt, raw, time.Now())
	if e != nil {
		return cfg, c, ErrExecution
	}
	return cfg, c, nil
}
func deploymentHistory(ctx context.Context, q Querier, cfg releasing.Configuration, c deployment.Command) (DeploymentInput, error) {
	r := DeploymentInput{Command: c}
	var e error
	r.Promotion, e = readPromotionRecord(ctx, q, c.PromotionID, "deployer")
	if e != nil {
		return DeploymentInput{}, ErrExecution
	}
	r.Provenance, e = readProvenanceRecord(ctx, q, r.Promotion.Command.ProvenanceID, "deployer")
	if e != nil {
		return DeploymentInput{}, ErrExecution
	}
	r.Build, e = readBuiltRecord(ctx, q, r.Provenance.Command.BuildID, "deployer")
	if e != nil || c.BindRecords(cfg, r.Promotion, r.Provenance, r.Build, time.Now()) != nil {
		return DeploymentInput{}, ErrExecution
	}
	return r, nil
}
func (j *DeploymentJournal) stillCurrent(ctx context.Context, r DeploymentInput) error {
	cfg, e := j.configuration()
	if e != nil || ctx.Err() != nil || r.Command.BindRecords(cfg, r.Promotion, r.Provenance, r.Build, time.Now()) != nil {
		return ErrExecution
	}
	return nil
}
func (j *DeploymentJournal) fresh(ctx context.Context, q Querier, cfg releasing.Configuration, c deployment.Command) (DeploymentInput, error) {
	r, e := deploymentHistory(ctx, q, cfg, c)
	if e != nil || freshBuildInput(ctx, q, cfg.Provenance.Build, r.Build.Command, "deployer") != nil || j.stillCurrent(ctx, r) != nil {
		return DeploymentInput{}, ErrExecution
	}
	return r, nil
}
func (j *DeploymentJournal) QueueDeploymentTx(ctx context.Context, tx *sql.Tx, raw, signed []byte) error {
	if tx == nil || AssertComponent(ctx, tx, "deployer") != nil {
		return ErrExecution
	}
	cfg, c, e := j.current(raw, signed)
	if e != nil {
		return e
	}
	if c.ExpiresAt-time.Now().UnixMilli() < releasing.MinimumEffectBudget.Milliseconds() {
		return ErrExecution
	}
	if _, e = j.fresh(ctx, tx, cfg, c); e != nil {
		return e
	}
	config, _ := json.Marshal(cfg)
	result, e := tx.ExecContext(ctx, `INSERT INTO deployment_requests(id,promotion_id,payload_digest,payload,receipt,configuration,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7 WHERE $7>=clock_timestamp()+interval '14 minutes' AND $7<=clock_timestamp()+interval '15 minutes'`, c.ID(), c.PromotionID, authority.Digest(raw), raw, signed, config, time.UnixMilli(c.ExpiresAt))
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	cfg, c, e = j.current(raw, signed)
	if e != nil {
		return e
	}
	_, e = j.fresh(ctx, tx, cfg, c)
	return e
}
func (j *DeploymentJournal) QueueDeployment(ctx context.Context, raw, signed []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.WithTx(ctx, func(tx *sql.Tx) error { return j.QueueDeploymentTx(ctx, tx, raw, signed) })
}

// check owns the row lock. Containment uses authenticated original authority and
// unchanged, unexpired installed destination; current advance freezes/issue
// withdrawal cannot prevent a bounded rollback of our acknowledged generation.
func (j *DeploymentJournal) check(ctx context.Context, tx *sql.Tx, c DeploymentClaim, containment bool) (releasing.Configuration, DeploymentInput, error) {
	var cfg releasing.Configuration
	var r DeploymentInput
	if !deploymentID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || AssertComponent(ctx, tx, "deployer") != nil {
		return cfg, r, ErrExecution
	}
	var raw, signed, original []byte
	var digest string
	var until time.Time
	if tx.QueryRowContext(ctx, `SELECT payload,receipt,configuration,payload_digest,expires_at FROM deployment_requests WHERE id=$1 AND claim_token=$2 AND state='claimed' FOR UPDATE`, c.ID, c.Token).Scan(&raw, &signed, &original, &digest, &until) != nil || authority.Digest(raw) != digest || !until.Equal(c.Until) {
		return cfg, r, ErrExecution
	}
	var command deployment.Command
	var e error
	if containment {
		var receipt authority.Receipt
		if authority.DecodeStrict(original, &cfg) != nil || authority.DecodeStrict(signed, &receipt) != nil {
			return cfg, r, ErrExecution
		}
		command, e = deployment.VerifyCommand(cfg, receipt, raw, time.Now())
	} else {
		cfg, command, e = j.current(raw, signed)
	}
	if e != nil || command.ID() != c.ID || !until.Equal(time.UnixMilli(command.ExpiresAt)) {
		return cfg, r, ErrExecution
	}
	var alive bool
	if tx.QueryRowContext(ctx, `SELECT $1>clock_timestamp()`, until).Scan(&alive) != nil || !alive {
		return cfg, r, ErrExecution
	}
	if containment {
		r, e = deploymentHistory(ctx, tx, cfg, command)
		if e == nil {
			e = j.containmentCurrent(ctx, cfg, command)
		}
	} else {
		r, e = j.fresh(ctx, tx, cfg, command)
	}
	return cfg, r, e
}

// The independent installed target is sufficient only for containment already
// authorized by the original durable command and acknowledged advance.
func NewDeploymentJournalWithTarget(db *DB, load func() (releasing.Configuration, error), target func() (releasing.Target, error), browser func() (browsercheck.Profile, error)) *DeploymentJournal {
	return &DeploymentJournal{DB: db, load: load, target: target, browser: browser}
}
func NewDeploymentJournalWithCleanupIdentity(db *DB, load func() (releasing.Configuration, error), target func() (releasing.Target, error), browser func() (browsercheck.Profile, error), cleanupTarget func() (releasing.Target, error)) *DeploymentJournal {
	j := NewDeploymentJournalWithTarget(db, load, target, browser)
	j.cleanupTarget = cleanupTarget
	return j
}

func (j *DeploymentJournal) containmentCurrent(ctx context.Context, original releasing.Configuration, c deployment.Command) error {
	var target releasing.Target
	var e error
	if j.target != nil {
		target, e = j.target()
	} else {
		var cfg releasing.Configuration
		cfg, e = j.configuration()
		target = cfg.Target
	}
	now := time.Now()
	if e != nil || ctx.Err() != nil || target.Digest() != original.Target.Digest() || target.Validate(now) != nil || c.ExpiresAt <= now.UnixMilli() {
		return ErrExecution
	}
	var profile browsercheck.Profile
	if j.browser != nil {
		profile, e = j.browser()
	} else {
		var cfg releasing.Configuration
		cfg, e = j.configuration()
		profile = cfg.Browser
	}
	if e != nil || profile.Validate() != nil || profile.Digest() != original.Browser.Digest() || ctx.Err() != nil {
		return ErrExecution
	}
	return nil
}

func (j *DeploymentJournal) ClaimDeployment(ctx context.Context, id string) (DeploymentClaim, error) {
	c := DeploymentClaim{ID: id, Token: NewID()}
	if !deploymentID.MatchString(id) {
		return DeploymentClaim{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "deployer") != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE deployment_requests SET state='claimed',claim_token=$2 WHERE id=$1 AND state='queued' AND expires_at>=clock_timestamp()+interval '14 minutes' RETURNING expires_at`, id, c.Token).Scan(&c.Until) != nil {
			return ErrExecution
		}
		_, _, e := j.check(ctx, tx, c, false)
		return e
	})
	if e != nil {
		return DeploymentClaim{}, ErrExecution
	}
	if _, _, e = j.ReadDeploymentInput(ctx, c); e != nil {
		return DeploymentClaim{}, ErrExecution
	}
	return c, nil
}
func (j *DeploymentJournal) ReadDeploymentInput(ctx context.Context, c DeploymentClaim) (releasing.Configuration, DeploymentInput, error) {
	var cfg releasing.Configuration
	var r DeploymentInput
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.WithTx(ctx, func(tx *sql.Tx) error { var e error; cfg, r, e = j.check(ctx, tx, c, false); return e })
	if e != nil || j.stillCurrent(ctx, r) != nil {
		return releasing.Configuration{}, DeploymentInput{}, ErrExecution
	}
	return cfg, r, nil
}
func effectKind(rollback bool) string {
	if rollback {
		return "rollback"
	}
	return "advance"
}
func freshSnapshot(s releasing.Snapshot, now time.Time) bool {
	return s.ObservedAt > 0 && s.ObservedAt <= now.Add(5*time.Second).UnixMilli() && s.ObservedAt >= now.Add(-30*time.Second).UnixMilli()
}

// expectedEffect binds rollback to the original durable ACK, never image equality.
func (j *DeploymentJournal) expectedEffect(ctx context.Context, q Querier, c DeploymentClaim, cfg releasing.Configuration, r DeploymentInput, before releasing.Snapshot, rollback bool) (string, error) {
	if !freshSnapshot(before, time.Now()) {
		return "", ErrExecution
	}
	image := r.Promotion.Plan.Image
	if !rollback {
		if validateBrowserBaseline(ctx, q, c, cfg, r, before, time.Now()) != nil {
			return "", ErrExecution
		}
		if before.Image != cfg.Target.BaselineImage || c.Until.Sub(time.Now()) < releasing.MinimumEffectBudget {
			return "", ErrExecution
		}
	} else {
		receipt, e := readDeploymentReceipt(ctx, q, c, "advance", cfg, r)
		if e != nil || before.DeploymentUID != receipt.After.DeploymentUID || before.TargetSHA256 != receipt.After.TargetSHA256 || before.Generation != receipt.After.Generation || before.Image != receipt.After.Image {
			return "", ErrExecution
		}
		image = cfg.Target.BaselineImage
	}
	raw, e := deployment.CompilePatch(cfg.Target, before, image, time.Now())
	if e != nil {
		return "", ErrExecution
	}
	return authority.Digest(raw), nil
}
func (j *DeploymentJournal) BeginDeploymentEffect(ctx context.Context, c DeploymentClaim, before releasing.Snapshot, rollback bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.WithTx(ctx, func(tx *sql.Tx) error {
		cfg, r, e := j.check(ctx, tx, c, rollback)
		if e != nil {
			return e
		}
		hash, e := j.expectedEffect(ctx, tx, c, cfg, r, before, rollback)
		if e != nil {
			return e
		}
		raw, _ := json.Marshal(before)
		if _, e = tx.ExecContext(ctx, `INSERT INTO deployment_intents(request_id,kind,claim_token,before_snapshot,request_digest)VALUES($1,$2,$3,$4,$5)`, c.ID, effectKind(rollback), c.Token, raw, hash); e != nil {
			return ErrExecution
		}
		cfg, r, e = j.check(ctx, tx, c, rollback)
		if e != nil {
			return e
		}
		_, e = j.expectedEffect(ctx, tx, c, cfg, r, before, rollback)
		return e
	})
}

// DeploymentGate consumes a durable one-use send fence. Commit acknowledgement
// loss is denial, even if the fence committed. A fresh process cannot resend it.
type DeploymentGate struct {
	Journal *DeploymentJournal
	Claim   DeploymentClaim
}

func (g DeploymentGate) CheckConsoleEffect(ctx context.Context, before releasing.Snapshot, hash string, rollback bool) error {
	j, c := g.Journal, g.Claim
	if j == nil {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var cfg releasing.Configuration
	var r DeploymentInput
	e := j.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		cfg, r, e = j.check(ctx, tx, c, rollback)
		if e != nil {
			return e
		}
		want, e := j.expectedEffect(ctx, tx, c, cfg, r, before, rollback)
		if e != nil || want != hash {
			return ErrExecution
		}
		var raw []byte
		var saved string
		if tx.QueryRowContext(ctx, `SELECT before_snapshot,request_digest FROM deployment_intents WHERE request_id=$1 AND kind=$2 AND claim_token=$3`, c.ID, effectKind(rollback), c.Token).Scan(&raw, &saved) != nil {
			return ErrExecution
		}
		var original releasing.Snapshot
		if authority.DecodeStrict(raw, &original) != nil || original != before || saved != hash {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO deployment_sends(request_id,kind,claim_token)VALUES($1,$2,$3)`, c.ID, effectKind(rollback), c.Token); e != nil {
			return ErrExecution
		}
		cfg, r, e = j.check(ctx, tx, c, rollback)
		if e != nil {
			return e
		}
		_, e = j.expectedEffect(ctx, tx, c, cfg, r, before, rollback)
		return e
	})
	if e != nil {
		return ErrExecution
	}
	// COMMIT itself can wait. Re-read current issue eligibility and claim state
	// after the one-use fence is durable, before exposing permission to send.
	e = j.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		cfg, r, e = j.check(ctx, tx, c, rollback)
		if e != nil {
			return e
		}
		_, e = j.expectedEffect(ctx, tx, c, cfg, r, before, rollback)
		return e
	})
	if e != nil {
		return ErrExecution
	}
	if rollback {
		return j.containmentCurrent(ctx, cfg, r.Command)
	}
	if c.Until.Sub(time.Now()) < releasing.MinimumEffectBudget {
		return ErrExecution
	}
	return j.stillCurrent(ctx, r)
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"regexp"
	"time"
)

var terminalID = regexp.MustCompile(`^complete-[a-f0-9]{64}$`)

type TerminalJournal struct {
	*DB
	load func() (releasing.Configuration, error)
}
type TerminalClaim struct {
	ID, Token string
	Until     time.Time
}
type TerminalInput struct {
	Configuration releasing.Configuration
	Command       releasecompletion.Command
	Plan          releasecompletion.Plan
	Upstream      CompletionInput
}

func NewTerminalJournal(db *DB, load func() (releasing.Configuration, error)) *TerminalJournal {
	return &TerminalJournal{db, load}
}
func (j *TerminalJournal) configuration() (releasing.Configuration, error) {
	if j == nil || j.DB == nil || j.load == nil {
		return releasing.Configuration{}, ErrExecution
	}
	cfg, e := j.load()
	if e != nil {
		return releasing.Configuration{}, ErrExecution
	}
	raw, e := json.Marshal(cfg)
	var out releasing.Configuration
	if e != nil || len(raw) > 327680 || authority.DecodeStrict(raw, &out) != nil {
		return out, ErrExecution
	}
	return out, nil
}
func terminalInput(ctx context.Context, q Querier, cfg releasing.Configuration, raw, signed []byte, now time.Time) (TerminalInput, error) {
	r := TerminalInput{Configuration: cfg}
	var sig authority.Receipt
	if AssertComponent(ctx, q, "release-manager") != nil || len(signed) > 4096 || authority.DecodeStrict(signed, &sig) != nil {
		return r, ErrExecution
	}
	var e error
	r.Command, e = releasecompletion.VerifyCommand(cfg, sig, raw, now)
	if e != nil {
		return r, ErrExecution
	}
	r.Upstream, e = readCompletionInput(ctx, q, r.Command.DeploymentID, "release-manager")
	if e != nil {
		return r, ErrExecution
	}
	u := r.Upstream
	r.Plan, e = releasecompletion.CompilePlan(cfg, r.Command, u.Deployment, u.Upstream.Promotion, u.Upstream.Provenance, u.Upstream.Build, now)
	if e != nil {
		return r, ErrExecution
	}
	return r, nil
}
func (j *TerminalJournal) fresh(ctx context.Context, q Querier, r TerminalInput) error {
	cfg, e := j.configuration()
	u := r.Upstream
	if e != nil || r.Plan.BindRecords(cfg, r.Command, u.Deployment, u.Upstream.Promotion, u.Upstream.Provenance, u.Upstream.Build, time.Now()) != nil || freshBuildInput(ctx, q, cfg.Provenance.Build, u.Upstream.Build.Command, "release-manager") != nil {
		return ErrExecution
	}
	cfg, e = j.configuration()
	if e != nil || ctx.Err() != nil || r.Plan.BindRecords(cfg, r.Command, u.Deployment, u.Upstream.Promotion, u.Upstream.Provenance, u.Upstream.Build, time.Now()) != nil {
		return ErrExecution
	}
	return nil
}
func (j *TerminalJournal) QueueTerminalTx(ctx context.Context, tx *sql.Tx, raw, signed []byte) error {
	cfg, e := j.configuration()
	if e != nil || tx == nil {
		return ErrExecution
	}
	r, e := terminalInput(ctx, tx, cfg, raw, signed, time.Now())
	if e != nil || j.fresh(ctx, tx, r) != nil {
		return ErrExecution
	}
	config, _ := json.Marshal(cfg)
	plan, _ := json.Marshal(r.Plan)
	result, e := tx.ExecContext(ctx, `INSERT INTO terminal_requests(id,deployment_id,payload_digest,payload,receipt,configuration,plan,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE $8>clock_timestamp()+interval '30 seconds'`, r.Command.ID(), r.Command.DeploymentID, authority.Digest(raw), raw, signed, config, plan, time.UnixMilli(r.Command.ExpiresAt))
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return j.fresh(ctx, tx, r)
}
func (j *TerminalJournal) QueueTerminal(ctx context.Context, raw, signed []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.WithTx(ctx, func(tx *sql.Tx) error { return j.QueueTerminalTx(ctx, tx, raw, signed) })
}

// archived authenticates the original command and complete deployment chain at
// issuance. It records acknowledged facts even after the issue closes or policy
// changes; fresh is separately mandatory before each new external mutation.
func terminalArchived(ctx context.Context, q Querier, c TerminalClaim) (TerminalInput, error) {
	var r TerminalInput
	var raw, signed, config, plan []byte
	var hash string
	var until, created time.Time
	if !terminalID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || AssertComponent(ctx, q, "release-manager") != nil {
		return r, ErrExecution
	}
	if q.QueryRowContext(ctx, `SELECT payload,receipt,configuration,plan,payload_digest,expires_at,created_at FROM terminal_requests WHERE id=$1 AND claim_token=$2 AND state IN('claimed','completed','quarantined')`, c.ID, c.Token).Scan(&raw, &signed, &config, &plan, &hash, &until, &created) != nil || hash != authority.Digest(raw) || !until.Equal(c.Until) {
		return r, ErrExecution
	}
	var cfg releasing.Configuration
	var command releasecompletion.Command
	var saved releasecompletion.Plan
	if authority.DecodeStrict(config, &cfg) != nil || authority.DecodeStrict(raw, &command) != nil || authority.DecodeStrict(plan, &saved) != nil || command.ID() != c.ID || command.ExpiresAt != until.UnixMilli() || created.UnixMilli() < command.IssuedAt-5 || !created.Before(until) {
		return r, ErrExecution
	}
	r, e := terminalInput(ctx, q, cfg, raw, signed, time.UnixMilli(command.IssuedAt))
	if e != nil || r.Plan != saved {
		return TerminalInput{}, ErrExecution
	}
	return r, nil
}
func (j *TerminalJournal) check(ctx context.Context, tx *sql.Tx, c TerminalClaim, fresh bool) (TerminalInput, error) {
	var state string
	if tx == nil || AssertComponent(ctx, tx, "release-manager") != nil || tx.QueryRowContext(ctx, `SELECT state FROM terminal_requests WHERE id=$1 AND claim_token=$2 AND state='claimed' FOR UPDATE`, c.ID, c.Token).Scan(&state) != nil {
		return TerminalInput{}, ErrExecution
	}
	r, e := terminalArchived(ctx, tx, c)
	if e != nil {
		return r, e
	}
	if fresh {
		if j.fresh(ctx, tx, r) != nil {
			return r, ErrExecution
		}
		var alive bool
		if tx.QueryRowContext(ctx, `SELECT $1>clock_timestamp()`, c.Until).Scan(&alive) != nil || !alive {
			return r, ErrExecution
		}
	}
	return r, nil
}
func (j *TerminalJournal) ClaimTerminal(ctx context.Context, id string) (TerminalClaim, error) {
	c := TerminalClaim{ID: id, Token: NewID()}
	if !terminalID.MatchString(id) {
		return TerminalClaim{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "release-manager") != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE terminal_requests SET state='claimed',claim_token=$2 WHERE id=$1 AND state='queued' AND expires_at>clock_timestamp()+interval '30 seconds' RETURNING expires_at`, id, c.Token).Scan(&c.Until) != nil {
			return ErrExecution
		}
		_, e := j.check(ctx, tx, c, true)
		return e
	})
	if e != nil {
		return TerminalClaim{}, ErrExecution
	}
	return c, nil
}
func (j *TerminalJournal) ReadTerminal(ctx context.Context, c TerminalClaim) (TerminalInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return terminalArchived(ctx, j.Pool, c)
}
func (j *TerminalJournal) NextTerminal(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, e := j.configuration()
	if e != nil || cfg.Validate(time.Now()) != nil || AssertComponent(ctx, j.Pool, "release-manager") != nil {
		return "", ErrExecution
	}
	// Filter revoked generations and changed authority before LIMIT, so an old
	// queued command cannot starve a later eligible issue. Original commands are
	// retained and expire; no payload or deadline is refreshed.
	var id string
	e = j.Pool.QueryRowContext(ctx, `SELECT q.id FROM terminal_requests q
 JOIN deployment_requests d ON d.id=q.deployment_id
 JOIN promotion_requests pm ON pm.id=d.promotion_id
 JOIN provenance_requests pv ON pv.id=pm.provenance_id
 JOIN build_requests b ON b.id=pv.build_id
 JOIN merge_requests m ON m.id=b.merge_id
 JOIN publication_requests pub ON pub.id=m.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pub.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(q.plan,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(q.plan,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$2
 WHERE q.state='queued' AND w.status='running' AND q.expires_at>clock_timestamp()+interval '30 seconds'
 AND convert_from(q.payload,'UTF8')::jsonb->>'configurationSHA256'=$1
 AND h.repository=$5 AND ir.config_digest=$2 AND o.config_digest=$2 AND o.authority_revision=$3
 AND h.generation=(convert_from(q.plan,'UTF8')::jsonb->>'generation')::bigint
 AND ir.revision=convert_from(q.plan,'UTF8')::jsonb->>'issueRevision'
 AND convert_from(ir.payload,'UTF8')::jsonb->>'eligible'='true'
 AND o.observed_at BETWEEN clock_timestamp()-($4*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND cur.watermark BETWEEN clock_timestamp()-($4*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 ORDER BY q.created_at,q.id LIMIT 1`, deployment.ConfigurationDigest(cfg), cfg.Provenance.Build.IntakeConfigSHA256, cfg.Provenance.Build.Inner.Revision(), cfg.Provenance.Build.IssueMaxAgeSeconds, cfg.Provenance.Build.Outer.Repository).Scan(&id)
	if e == sql.ErrNoRows {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}

func (j *TerminalJournal) ExpireTerminals(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "release-manager") != nil {
		return ErrExecution
	}
	// A fully acknowledged close remains finishable after expiry. Expiry never
	// authorizes a new external effect, but it must not discard a completed fact.
	_, e := j.Pool.ExecContext(ctx, `UPDATE terminal_requests q SET state='quarantined',reason='expired' WHERE q.state IN('queued','claimed') AND q.expires_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM terminal_receipts r WHERE r.request_id=q.id AND r.step='close')`)
	return e
}

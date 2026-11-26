package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// FlowConsoleComponent is the optional read-only observer of the journal
// (docs/operations/flow-console.md). It is not a workflow component: it holds
// no outbox, inbox or nonce privileges, writes nothing, and reads only the
// timeline columns below. Its role exists only while the installation's
// credential document names it.
const FlowConsoleComponent = "flow-console"

// FlowConsoleRole is the dedicated login role of the flow console.
const FlowConsoleRole = "aisdlc_flow_console"

// flowConsoleColumns is the closed timeline surface: identifiers, states,
// bounded reasons, counts and timestamps. Payloads, receipts, proofs, digests,
// claim tokens, pod/resource identities, deltas, model observations and issue
// content are deliberately absent. Migration 0039 adds the matching SELECT
// policies on tables with row security; this list is the column grant.
var flowConsoleColumns = map[string][]string{
	"plans":      {"id", "repository"},
	"work_items": {"plan_id", "uuid", "issue_numbers", "status", "created_at", "updated_at"},
	// Every observed revision (intake_observations keeps only the latest
	// one per issue). payload, revision, event_id and config_digest are
	// never granted: the payload is the issue content.
	"intake_revisions":      {"repository", "issue_number", "generation", "observed_at"},
	"admissions":            {"repository", "issue_number", "decision", "risk", "created_at", "updated_at"},
	"execution_requests":    {"id", "work_item_uuid", "attempt", "repository", "state", "stop_reason", "expires_at", "claim_until", "created_at", "updated_at"},
	"execution_lifecycles":  {"request_id", "phase", "cleanup_state", "cleanup_attempts", "created_at", "updated_at"},
	"execution_effects":     {"request_id", "kind", "created_at", "completed_at"},
	"execution_failures":    {"request_id", "exit_code", "observed_at"},
	"execution_candidates":  {"request_id", "changes", "model_calls", "created_at"},
	"execution_models":      {"request_id", "allocation_id", "created_at"},
	"model_calls":           {"allocation_id", "sequence", "state", "response_status", "input_bytes", "output_bytes", "response_handed_off_at"},
	"verification_requests": {"id", "candidate_id", "state", "reason", "cleanup_state", "expires_at", "claim_until", "created_at"},
	"verification_effects":  {"request_id", "kind", "created_at", "completed_at", "not_sent_at"},
	"review_runs":           {"id", "candidate_id", "work_item_uuid", "state", "reason", "cleanup_state", "expires_at", "claim_until", "created_at"},
	"review_effects":        {"run_id", "kind", "created_at", "completed_at", "not_sent_at"},
	"review_model_calls":    {"run_id", "sequence", "state", "input_bytes", "output_bytes", "created_at", "handed_off_at"},
	"publication_requests":  {"id", "candidate_id", "state", "reason", "expires_at", "claim_until", "commit_time", "created_at"},
	"publication_effects":   {"request_id", "kind", "created_at", "completed_at", "not_sent_at"},
	"published_records":     {"publication_id", "published_at"},
	"merge_requests":        {"id", "publication_id", "state", "reason", "expires_at", "claim_until", "merged_at", "created_at"},
	"merge_effects":         {"request_id", "created_at", "completed_at", "not_sent_at"},
}

// FlowConsoleGrants returns the column grants in a stable order.
func FlowConsoleGrants() []string {
	tables := make([]string, 0, len(flowConsoleColumns))
	for table := range flowConsoleColumns {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	grants := make([]string, 0, len(tables))
	for _, table := range tables {
		grants = append(grants, `GRANT SELECT (`+strings.Join(flowConsoleColumns[table], ",")+`) ON `+table+` TO `+FlowConsoleRole)
	}
	return grants
}

// AssertFlowConsole refuses any identity other than the dedicated, unelevated
// flow console role: the bootstrap owner or a component credential must never
// serve the console, even in local mode.
func AssertFlowConsole(ctx context.Context, q Querier) error {
	return assertRole(ctx, q, FlowConsoleRole)
}

// reconcileFlowConsoleRole runs inside role provisioning. With a credential it
// creates or adopts only the managed observer role and grants exactly the
// timeline columns. Without one it leaves an absent role absent and disables a
// managed one (NOLOGIN, every privilege revoked), so turning the console off
// in the credential document withdraws its database access.
func reconcileFlowConsoleRole(ctx context.Context, tx *sql.Tx, quotedSchema string, columnRevocations []string, password string, enabled bool) error {
	var exists, managed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, FlowConsoleRole).Scan(&exists); err != nil {
		return errors.New("role lookup failed")
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_component_roles WHERE component=$1 AND role_name=$2)`, FlowConsoleComponent, FlowConsoleRole).Scan(&managed); err != nil {
		return errors.New("role ownership lookup failed")
	}
	if exists && !managed {
		return errors.New("refusing to adopt an unmanaged database role")
	}
	if !exists && !enabled {
		return nil
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, `CREATE ROLE `+FlowConsoleRole); err != nil {
			return errors.New("role creation failed")
		}
	}
	var member bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname=$1)`, FlowConsoleRole).Scan(&member); err != nil {
		return errors.New("role membership lookup failed")
	}
	if member {
		return errors.New("component role has unexpected inherited or settable roles")
	}
	login := `NOLOGIN`
	if enabled {
		login = `LOGIN PASSWORD '` + password + `'`
	}
	if _, err := tx.ExecContext(ctx, `ALTER ROLE `+FlowConsoleRole+` `+login+` NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION`); err != nil {
		return errors.New("role credential reconciliation failed")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO managed_component_roles(component,role_name) VALUES($1,$2) ON CONFLICT DO NOTHING`, FlowConsoleComponent, FlowConsoleRole); err != nil {
		return errors.New("role ownership registration failed")
	}
	statements := []string{
		`REVOKE ALL ON SCHEMA ` + quotedSchema + ` FROM ` + FlowConsoleRole,
		`REVOKE ALL ON ALL TABLES IN SCHEMA ` + quotedSchema + ` FROM ` + FlowConsoleRole,
		`REVOKE ALL ON FUNCTION public.lock_repair_parent_budget(uuid) FROM ` + FlowConsoleRole,
	}
	for _, revoke := range columnRevocations {
		statements = append(statements, revoke+FlowConsoleRole)
	}
	if enabled {
		statements = append(statements, `GRANT USAGE ON SCHEMA `+quotedSchema+` TO `+FlowConsoleRole)
		statements = append(statements, FlowConsoleGrants()...)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return errors.New("flow console privilege reconciliation failed")
		}
	}
	return nil
}

// FlowLink binds a stage row to its work item and attempt.
type FlowLink struct {
	WorkItem   string
	Attempt    int
	Repository string
}

type FlowIntake struct {
	Repository string
	Issue      int
	Generation int64
	ObservedAt time.Time
}

type FlowAdmission struct {
	Repository           string
	Issue                int
	Decision, Risk       string
	CreatedAt, UpdatedAt time.Time
}

type FlowWorkItem struct {
	UUID, Repository     string
	Issues               []int
	Status               string
	CreatedAt, UpdatedAt time.Time
}

type FlowExecution struct {
	ID string
	FlowLink
	State, StopReason               string
	CreatedAt, UpdatedAt, ExpiresAt time.Time
	ClaimUntil                      *time.Time
	Phase, Cleanup                  string
	CleanupAttempts                 int
	LifecycleAt, LifecycleUpdated   *time.Time
	ExitCode                        *int
	FailedAt                        *time.Time
	Changes, ModelCalls             *int
	CandidateAt                     *time.Time
}

// FlowEffect is one durable external effect of a stage (a Kubernetes object,
// a forge write). Its digest, token and receipt are never read.
type FlowEffect struct {
	Parent, Kind           string
	CreatedAt              time.Time
	CompletedAt, NotSentAt *time.Time
}

// FlowModelCall is one generator (model_calls) or reviewer
// (review_model_calls) call. Generator calls record no start time: Start is
// the previous handoff, or the allocation for the first call.
type FlowModelCall struct {
	Parent              string
	Sequence            int
	State               string
	Status              *int
	InputBytes          int
	OutputBytes         *int
	CreatedAt, Finished *time.Time
	WindowStart         time.Time
}

type FlowVerdict struct {
	ID string
	FlowLink
	State, Reason, Cleanup string
	CreatedAt, ExpiresAt   time.Time
	ClaimUntil             *time.Time
	// Publication: commit time and published record; merge: merged time.
	CommitTime, FinishedAt *time.Time
	// Candidate is the execution request the stage works on: it chains
	// verification, review, publication and merge of one attempt.
	Candidate string
}

// FlowJournal is one consistent read of the timeline surface.
type FlowJournal struct {
	ObservedAt          time.Time
	Truncated           bool
	Intake              []FlowIntake
	Admissions          []FlowAdmission
	WorkItems           []FlowWorkItem
	Executions          []FlowExecution
	ExecutionEffects    []FlowEffect
	ModelCalls          []FlowModelCall
	Verifications       []FlowVerdict
	VerificationEffects []FlowEffect
	Reviews             []FlowVerdict
	ReviewEffects       []FlowEffect
	ReviewModelCalls    []FlowModelCall
	Publications        []FlowVerdict
	PublicationEffects  []FlowEffect
	Merges              []FlowVerdict
	MergeEffects        []FlowEffect
}

var ErrFlowJournal = errors.New("flow journal unavailable")

// ReadFlowJournal reads the timeline surface of work that started at or after
// since, in one read-only REPEATABLE READ snapshot, at most limit rows per
// table (newest first). It must run as FlowConsoleRole. Errors are a single
// sentinel: driver errors, SQL and row contents are never returned.
func ReadFlowJournal(ctx context.Context, db *DB, since time.Time, limit int) (FlowJournal, error) {
	var j FlowJournal
	if db == nil || db.Pool == nil || limit < 1 || limit > 20000 {
		return j, ErrFlowJournal
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := db.Pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return j, ErrFlowJournal
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='5000ms'`); err != nil {
		return j, ErrFlowJournal
	}
	if AssertFlowConsole(ctx, tx) != nil {
		return j, ErrFlowJournal
	}
	if err = tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&j.ObservedAt); err != nil {
		return j, ErrFlowJournal
	}
	r := flowReader{ctx: ctx, tx: tx, since: since, limit: limit}
	r.intake(&j)
	r.admissions(&j)
	r.workItems(&j)
	r.executions(&j)
	r.effects(`SELECT e.request_id,e.kind,e.created_at,e.completed_at,NULL::timestamptz FROM execution_effects e JOIN execution_requests r ON r.id=e.request_id
 WHERE r.created_at>=$1 ORDER BY e.created_at DESC LIMIT $2`, &j.ExecutionEffects)
	r.modelCalls(&j)
	r.verdicts(`SELECT v.id,r.work_item_uuid,r.attempt,r.repository,v.state,v.reason,v.cleanup_state,v.created_at,v.expires_at,v.claim_until,NULL::timestamptz,NULL::timestamptz,v.candidate_id
 FROM verification_requests v JOIN execution_requests r ON r.id=v.candidate_id WHERE v.created_at>=$1 ORDER BY v.created_at DESC LIMIT $2`, &j.Verifications)
	r.effects(`SELECT e.request_id,e.kind,e.created_at,e.completed_at,e.not_sent_at FROM verification_effects e JOIN verification_requests v ON v.id=e.request_id
 WHERE v.created_at>=$1 ORDER BY e.created_at DESC LIMIT $2`, &j.VerificationEffects)
	r.verdicts(`SELECT v.id,v.work_item_uuid::text,r.attempt,r.repository,v.state,v.reason,v.cleanup_state,v.created_at,v.expires_at,v.claim_until,NULL::timestamptz,NULL::timestamptz,v.candidate_id
 FROM review_runs v JOIN execution_requests r ON r.id=v.candidate_id WHERE v.created_at>=$1 ORDER BY v.created_at DESC LIMIT $2`, &j.Reviews)
	r.effects(`SELECT e.run_id,e.kind,e.created_at,e.completed_at,e.not_sent_at FROM review_effects e JOIN review_runs v ON v.id=e.run_id
 WHERE v.created_at>=$1 ORDER BY e.created_at DESC LIMIT $2`, &j.ReviewEffects)
	r.reviewModelCalls(&j)
	r.verdicts(`SELECT p.id,r.work_item_uuid,r.attempt,r.repository,p.state,p.reason,'complete',p.created_at,p.expires_at,p.claim_until,p.commit_time,d.published_at,p.candidate_id
 FROM publication_requests p JOIN execution_requests r ON r.id=p.candidate_id LEFT JOIN published_records d ON d.publication_id=p.id
 WHERE p.created_at>=$1 ORDER BY p.created_at DESC LIMIT $2`, &j.Publications)
	r.effects(`SELECT e.request_id,e.kind,e.created_at,e.completed_at,e.not_sent_at FROM publication_effects e JOIN publication_requests p ON p.id=e.request_id
 WHERE p.created_at>=$1 ORDER BY e.created_at DESC LIMIT $2`, &j.PublicationEffects)
	r.verdicts(`SELECT m.id,r.work_item_uuid,r.attempt,r.repository,m.state,m.reason,'complete',m.created_at,m.expires_at,m.claim_until,NULL::timestamptz,m.merged_at,p.candidate_id
 FROM merge_requests m JOIN publication_requests p ON p.id=m.publication_id JOIN execution_requests r ON r.id=p.candidate_id
 WHERE m.created_at>=$1 ORDER BY m.created_at DESC LIMIT $2`, &j.Merges)
	r.effects(`SELECT e.request_id,'merge',e.created_at,e.completed_at,e.not_sent_at FROM merge_effects e JOIN merge_requests m ON m.id=e.request_id
 WHERE m.created_at>=$1 ORDER BY e.created_at DESC LIMIT $2`, &j.MergeEffects)
	if r.err != nil {
		return FlowJournal{}, ErrFlowJournal
	}
	j.Truncated = r.truncated
	if tx.Commit() != nil {
		return FlowJournal{}, ErrFlowJournal
	}
	return j, nil
}

type flowReader struct {
	ctx       context.Context
	tx        *sql.Tx
	since     time.Time
	limit     int
	err       error
	truncated bool
}

func (r *flowReader) query(query string, scan func(*sql.Rows) error) {
	if r.err != nil {
		return
	}
	rows, err := r.tx.QueryContext(r.ctx, query, r.since, r.limit)
	if err != nil {
		r.err = err
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := scan(rows); err != nil {
			r.err = err
			return
		}
		n++
	}
	if err := rows.Err(); err != nil {
		r.err = err
		return
	}
	if n >= r.limit {
		r.truncated = true
	}
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func intPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func (r *flowReader) intake(j *FlowJournal) {
	r.query(`SELECT repository,issue_number,generation,observed_at FROM intake_revisions WHERE observed_at>=$1 ORDER BY observed_at DESC LIMIT $2`, func(rows *sql.Rows) error {
		var v FlowIntake
		if err := rows.Scan(&v.Repository, &v.Issue, &v.Generation, &v.ObservedAt); err != nil {
			return err
		}
		j.Intake = append(j.Intake, v)
		return nil
	})
}

func (r *flowReader) admissions(j *FlowJournal) {
	r.query(`SELECT repository,issue_number,decision,risk,created_at,updated_at FROM admissions WHERE updated_at>=$1 ORDER BY updated_at DESC LIMIT $2`, func(rows *sql.Rows) error {
		var v FlowAdmission
		if err := rows.Scan(&v.Repository, &v.Issue, &v.Decision, &v.Risk, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return err
		}
		j.Admissions = append(j.Admissions, v)
		return nil
	})
}

func (r *flowReader) workItems(j *FlowJournal) {
	// Include older work items still referenced by an attempt in the window.
	r.query(`SELECT w.uuid::text,p.repository,w.issue_numbers,w.status,w.created_at,w.updated_at FROM work_items w JOIN plans p ON p.id=w.plan_id
 WHERE w.updated_at>=$1 OR w.uuid IN (SELECT work_item_uuid FROM execution_requests WHERE created_at>=$1) ORDER BY w.created_at DESC LIMIT $2`, func(rows *sql.Rows) error {
		var v FlowWorkItem
		var issues []byte
		if err := rows.Scan(&v.UUID, &v.Repository, &issues, &v.Status, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return err
		}
		if json.Unmarshal(issues, &v.Issues) != nil {
			v.Issues = nil
		}
		j.WorkItems = append(j.WorkItems, v)
		return nil
	})
}

func (r *flowReader) executions(j *FlowJournal) {
	r.query(`SELECT r.id,r.work_item_uuid::text,r.attempt,r.repository,r.state,r.stop_reason,r.created_at,r.updated_at,r.expires_at,r.claim_until,
 COALESCE(l.phase,''),COALESCE(l.cleanup_state,''),COALESCE(l.cleanup_attempts,0),l.created_at,l.updated_at,f.exit_code,f.observed_at,c.changes,c.model_calls,c.created_at
 FROM execution_requests r LEFT JOIN execution_lifecycles l ON l.request_id=r.id LEFT JOIN execution_failures f ON f.request_id=r.id
 LEFT JOIN execution_candidates c ON c.request_id=r.id WHERE r.created_at>=$1 ORDER BY r.created_at DESC LIMIT $2`, func(rows *sql.Rows) error {
		var v FlowExecution
		var claim, lifeAt, lifeUpdated, failedAt, candidateAt sql.NullTime
		var exitCode, changes, calls sql.NullInt64
		if err := rows.Scan(&v.ID, &v.WorkItem, &v.Attempt, &v.Repository, &v.State, &v.StopReason, &v.CreatedAt, &v.UpdatedAt, &v.ExpiresAt, &claim,
			&v.Phase, &v.Cleanup, &v.CleanupAttempts, &lifeAt, &lifeUpdated, &exitCode, &failedAt, &changes, &calls, &candidateAt); err != nil {
			return err
		}
		v.ClaimUntil, v.LifecycleAt, v.LifecycleUpdated, v.FailedAt, v.CandidateAt = timePtr(claim), timePtr(lifeAt), timePtr(lifeUpdated), timePtr(failedAt), timePtr(candidateAt)
		v.ExitCode, v.Changes, v.ModelCalls = intPtr(exitCode), intPtr(changes), intPtr(calls)
		j.Executions = append(j.Executions, v)
		return nil
	})
}

func (r *flowReader) effects(query string, target *[]FlowEffect) {
	r.query(query, func(rows *sql.Rows) error {
		var v FlowEffect
		var completed, notSent sql.NullTime
		if err := rows.Scan(&v.Parent, &v.Kind, &v.CreatedAt, &completed, &notSent); err != nil {
			return err
		}
		v.CompletedAt, v.NotSentAt = timePtr(completed), timePtr(notSent)
		*target = append(*target, v)
		return nil
	})
}

func (r *flowReader) modelCalls(j *FlowJournal) {
	r.query(`SELECT m.request_id,c.sequence,c.state,c.response_status,c.input_bytes,c.output_bytes,c.response_handed_off_at,m.created_at
 FROM execution_models m JOIN model_calls c ON c.allocation_id=m.allocation_id WHERE m.created_at>=$1 ORDER BY m.created_at DESC,c.sequence LIMIT $2`, func(rows *sql.Rows) error {
		var v FlowModelCall
		var status, output sql.NullInt64
		var handed sql.NullTime
		if err := rows.Scan(&v.Parent, &v.Sequence, &v.State, &status, &v.InputBytes, &output, &handed, &v.WindowStart); err != nil {
			return err
		}
		v.Status, v.OutputBytes, v.Finished = intPtr(status), intPtr(output), timePtr(handed)
		j.ModelCalls = append(j.ModelCalls, v)
		return nil
	})
}

func (r *flowReader) reviewModelCalls(j *FlowJournal) {
	r.query(`SELECT c.run_id,c.sequence,c.state,c.input_bytes,c.output_bytes,c.created_at,c.handed_off_at,v.created_at
 FROM review_model_calls c JOIN review_runs v ON v.id=c.run_id WHERE v.created_at>=$1 ORDER BY c.created_at DESC LIMIT $2`, func(rows *sql.Rows) error {
		var v FlowModelCall
		var output sql.NullInt64
		var created, handed sql.NullTime
		if err := rows.Scan(&v.Parent, &v.Sequence, &v.State, &v.InputBytes, &output, &created, &handed, &v.WindowStart); err != nil {
			return err
		}
		v.OutputBytes, v.CreatedAt, v.Finished = intPtr(output), timePtr(created), timePtr(handed)
		j.ReviewModelCalls = append(j.ReviewModelCalls, v)
		return nil
	})
}

func (r *flowReader) verdicts(query string, target *[]FlowVerdict) {
	r.query(query, func(rows *sql.Rows) error {
		var v FlowVerdict
		var claim, commit, finished sql.NullTime
		if err := rows.Scan(&v.ID, &v.WorkItem, &v.Attempt, &v.Repository, &v.State, &v.Reason, &v.Cleanup, &v.CreatedAt, &v.ExpiresAt, &claim, &commit, &finished, &v.Candidate); err != nil {
			return err
		}
		v.ClaimUntil, v.CommitTime, v.FinishedAt = timePtr(claim), timePtr(commit), timePtr(finished)
		*target = append(*target, v)
		return nil
	})
}

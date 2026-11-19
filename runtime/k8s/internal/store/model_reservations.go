package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

// This hidden ledger is not an authorization endpoint. Only a future trusted
// controller may provision policy or submit data; no runtime role has access.
// Limits are bytes and calls, not provider tokens or monetary expenditure.
type ModelLimits struct {
	Calls                   int
	InputBytes, OutputBytes int64
}
type ModelBudget struct {
	WorkItem, Repository, SourceSHA, PolicyDigest, Model string
	Limits                                               ModelLimits
	ExpiresAt                                            time.Time
}
type ModelAllocation struct {
	ID, WorkItem, PodUID string
	Attempt              int
	Limits               ModelLimits
	ResponseLimit        int
	ExpiresAt            time.Time
}
type ModelCallRef struct {
	AllocationID, PodUID, RequestSHA256 string
	Sequence                            int
}
type ModelCall struct {
	ModelCallRef
	State                     string
	InputBytes, ResponseLimit int
	ExpiresAt                 time.Time
	ResponseHandedOff         bool
}
type ModelDecision struct {
	Reason string
	Call   ModelCall
	// Invoke is true ONLY on a newly committed reserved -> started transition.
	// An error, uncertain commit or observed started record never grants invocation.
	Invoke bool
}

var modelUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var modelSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)
var modelDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errModelPolicy = errors.New("invalid or conflicting model policy")

func validModelLimits(l ModelLimits, calls int, input, output int64) bool {
	return l.Calls > 0 && l.Calls <= calls && l.InputBytes > 0 && l.InputBytes <= input && l.OutputBytes > 0 && l.OutputBytes <= output
}
func (db *DB) CreateModelBudget(ctx context.Context, b ModelBudget) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !modelUUID.MatchString(b.WorkItem) || !modelSHA.MatchString(b.SourceSHA) || !modelDigest.MatchString(b.PolicyDigest) || len(b.Repository) < 3 || len(b.Repository) > 256 || len(b.Model) < 1 || len(b.Model) > 128 || !validModelLimits(b.Limits, 128, 16777216, 33554432) {
		return errModelPolicy
	}
	// The one immutable policy row prevents resetting counters through a new
	// allocation or attempt. Changes require a separately reviewed lifecycle.
	result, err := db.Pool.ExecContext(ctx, `INSERT INTO model_budgets
 (work_item_uuid,repository,source_sha,policy_digest,model,max_calls,max_input_bytes,max_output_bytes,expires_at)
 SELECT w.uuid,p.repository,$3,$4,$5,$6,$7,$8,$9 FROM work_items w JOIN plans p ON p.id=w.plan_id
 WHERE w.uuid=$1 AND p.repository=$2 AND w.status='running'
 AND $9>clock_timestamp() AND $9<=clock_timestamp()+interval '24 hours'
 ON CONFLICT DO NOTHING`, b.WorkItem, b.Repository, b.SourceSHA, b.PolicyDigest, b.Model, b.Limits.Calls, b.Limits.InputBytes, b.Limits.OutputBytes, b.ExpiresAt)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errModelPolicy
	}
	return nil
}

func (db *DB) CreateModelAllocation(ctx context.Context, a ModelAllocation) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !modelUUID.MatchString(a.ID) || !modelUUID.MatchString(a.WorkItem) || !modelUUID.MatchString(a.PodUID) || a.Attempt < 1 || a.Attempt > 16 || a.ResponseLimit < 1 || a.ResponseLimit > modelmailbox.MaxResponseBytes || !validModelLimits(a.Limits, 32, 4194304, 8388608) || int64(a.ResponseLimit) > a.Limits.OutputBytes {
		return errModelPolicy
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		var b ModelBudget
		var state string
		err := tx.QueryRowContext(ctx, `SELECT max_calls,max_input_bytes,max_output_bytes,expires_at,state FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, a.WorkItem).Scan(&b.Limits.Calls, &b.Limits.InputBytes, &b.Limits.OutputBytes, &b.ExpiresAt, &state)
		if err != nil {
			return err
		}
		var reservedChild bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM execution_repairs WHERE child_allocation_id=$1)`, a.ID).Scan(&reservedChild); err != nil || reservedChild {
			return errModelPolicy
		}
		var now time.Time
		if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if state != "active" || !a.ExpiresAt.After(now) || a.ExpiresAt.After(now.Add(time.Hour)) || a.ExpiresAt.After(b.ExpiresAt) || !validModelLimits(a.Limits, b.Limits.Calls, b.Limits.InputBytes, b.Limits.OutputBytes) {
			return errModelPolicy
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO model_allocations(id,work_item_uuid,attempt,pod_uid,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, a.ID, a.WorkItem, a.Attempt, a.PodUID, a.Limits.Calls, a.Limits.InputBytes, a.Limits.OutputBytes, a.ResponseLimit, a.ExpiresAt)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errModelPolicy
		}
		return nil
	})
}

type lockedModel struct {
	workItem, model, state, podUID      string
	allocationID                        string
	derived                             bool
	budget, allocation                  ModelLimits
	budgetUsed, allocationUsed          ModelLimits
	responseLimit                       int
	budgetExpiry, allocationExpiry, now time.Time
}

// All mutations lock budget -> allocation -> call. clock_timestamp is read
// after blocking locks, avoiding the stale transaction-start value of now().
func lockModel(ctx context.Context, tx *sql.Tx, allocation string) (lockedModel, error) {
	l := lockedModel{allocationID: allocation}
	err := tx.QueryRowContext(ctx, `SELECT work_item_uuid FROM model_allocations WHERE id=$1`, allocation).Scan(&l.workItem)
	if err != nil {
		return l, err
	}
	err = tx.QueryRowContext(ctx, `SELECT model,state,max_calls,max_input_bytes,max_output_bytes,reserved_calls,reserved_input_bytes,reserved_output_bytes,expires_at FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, l.workItem).Scan(&l.model, &l.state, &l.budget.Calls, &l.budget.InputBytes, &l.budget.OutputBytes, &l.budgetUsed.Calls, &l.budgetUsed.InputBytes, &l.budgetUsed.OutputBytes, &l.budgetExpiry)
	if err != nil {
		return l, err
	}
	if err = lockRepairModel(ctx, tx, &l); err != nil {
		return l, err
	}
	err = tx.QueryRowContext(ctx, `SELECT pod_uid,max_calls,max_input_bytes,max_output_bytes,reserved_calls,reserved_input_bytes,reserved_output_bytes,response_limit,expires_at FROM model_allocations WHERE id=$1 FOR UPDATE`, allocation).Scan(&l.podUID, &l.allocation.Calls, &l.allocation.InputBytes, &l.allocation.OutputBytes, &l.allocationUsed.Calls, &l.allocationUsed.InputBytes, &l.allocationUsed.OutputBytes, &l.responseLimit, &l.allocationExpiry)
	if err != nil {
		return l, err
	}
	if l.derived && (l.allocation != l.budget || l.allocationExpiry.After(l.budgetExpiry)) {
		return l, errModelPolicy
	}
	return l, nil
}

// Evaluate every expiration against one DB instant, after ALL potentially
// blocking row locks (including the selected call) have been acquired.
func refreshModelTime(ctx context.Context, tx *sql.Tx, l *lockedModel) error {
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&l.now); err != nil {
		return err
	}
	// An expired pending call anywhere in this parent must stop new spending
	// immediately, even if the periodic recovery sweep has not run. The parent
	// lock serializes all call transitions; this observation needs no call lock.
	var expired bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_calls c JOIN model_allocations a ON a.id=c.allocation_id WHERE a.work_item_uuid=$1 AND (c.state IN ('reserved','started') OR (c.state='completed' AND c.response_handed_off_at IS NULL)) AND c.expires_at<=$2 AND (($3 AND a.id=$4::uuid) OR (NOT $3 AND NOT EXISTS(SELECT 1 FROM repair_model_budgets rb WHERE rb.allocation_id=a.id))))`, l.workItem, l.now, l.derived, l.allocationID).Scan(&expired); err != nil {
		return err
	}
	if expired && l.state == "active" {
		if err := stopModel(ctx, tx, *l, "expired-pending-call"); err != nil {
			return err
		}
		l.state = "stopped"
	}
	return nil
}
func (l lockedModel) active() bool {
	return l.state == "active" && l.now.Before(l.budgetExpiry) && l.now.Before(l.allocationExpiry)
}
func stopModel(ctx context.Context, tx *sql.Tx, l lockedModel, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE `+l.budgetTable()+` SET state='stopped',stop_reason=$2 WHERE work_item_uuid=$1 AND state='active'`, l.workItem, reason)
	return err
}
func readModelCall(ctx context.Context, tx *sql.Tx, r ModelCallRef) (ModelCall, error) {
	c := ModelCall{ModelCallRef: r}
	err := tx.QueryRowContext(ctx, `SELECT request_sha256,state,input_bytes,response_limit,expires_at,response_handed_off_at IS NOT NULL FROM model_calls WHERE allocation_id=$1 AND sequence=$2 FOR UPDATE`, r.AllocationID, r.Sequence).Scan(&c.RequestSHA256, &c.State, &c.InputBytes, &c.ResponseLimit, &c.ExpiresAt, &c.ResponseHandedOff)
	return c, err
}

// The parent budget lock serializes every response handoff. Check this barrier
// both before reservation and before claiming a previously reserved call.
func pendingModelHandoff(ctx context.Context, tx *sql.Tx, l lockedModel) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_calls c JOIN model_allocations a ON a.id=c.allocation_id WHERE a.work_item_uuid=$1 AND c.state='completed' AND c.response_handed_off_at IS NULL AND (($2 AND a.id=$3::uuid) OR (NOT $2 AND NOT EXISTS(SELECT 1 FROM repair_model_budgets rb WHERE rb.allocation_id=a.id))))`, l.workItem, l.derived, l.allocationID).Scan(&pending)
	return pending, err
}
func validModelRef(r ModelCallRef) bool {
	return modelUUID.MatchString(r.AllocationID) && modelUUID.MatchString(r.PodUID) && r.Sequence >= 1 && r.Sequence <= 32 && modelDigest.MatchString(r.RequestSHA256)
}
func room(l, u ModelLimits, input, output int64) bool {
	return u.Calls < l.Calls && input <= l.InputBytes-u.InputBytes && output <= l.OutputBytes-u.OutputBytes
}

// ReserveModelCall computes the digest and size from the actual bytes. Those
// bytes are never stored. Caller must retain and revalidate the exact body
// before dispatch, and never substitute a reconstructed request.
func (db *DB) ReserveModelCall(ctx context.Context, allocation, podUID string, sequence int, payload []byte) (ModelDecision, error) {
	return db.ReserveModelCallWithin(ctx, allocation, podUID, sequence, payload, time.Minute)
}

// ReserveModelCallWithin reserves one call whose expiry is bounded by window
// (the profile's per-call window, 60-600 s), the allocation and the budget.
func (db *DB) ReserveModelCallWithin(ctx context.Context, allocation, podUID string, sequence int, payload []byte, window time.Duration) (ModelDecision, error) {
	if window < time.Minute || window > 10*time.Minute {
		return ModelDecision{}, errModelPolicy
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d ModelDecision
	if modelmailbox.ValidateRequest(payload) != nil {
		return d, errModelPolicy
	}
	r := ModelCallRef{allocation, podUID, modelmailbox.Digest(payload), sequence}
	if !validModelRef(r) {
		return d, errModelPolicy
	}
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return d, errModelPolicy
	}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		l, err := lockModel(ctx, tx, allocation)
		if err != nil {
			return err
		}
		if l.podUID != podUID {
			d.Reason = "pod-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		if request.Model != l.model {
			d.Reason = "model-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		c, err := readModelCall(ctx, tx, r)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if refreshErr := refreshModelTime(ctx, tx, &l); refreshErr != nil {
			return refreshErr
		}
		if !l.active() {
			d.Reason = "inactive"
			return nil
		}
		if err == nil {
			if c.RequestSHA256 != r.RequestSHA256 {
				d.Reason = "replay-mismatch"
				return stopModel(ctx, tx, l, d.Reason)
			}
			d.Call = c
			d.Reason = "existing"
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if pending, err := pendingModelHandoff(ctx, tx, l); err != nil {
			return err
		} else if pending {
			d.Reason = "response-handoff-pending"
			return nil
		}
		if sequence != l.allocationUsed.Calls+1 {
			d.Reason = "sequence-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		var pending bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_calls WHERE allocation_id=$1 AND state IN ('reserved','started'))`, allocation).Scan(&pending); err != nil {
			return err
		}
		if pending {
			d.Reason = "pending"
			return nil
		}
		input, output := int64(len(payload)), int64(l.responseLimit)
		if !room(l.budget, l.budgetUsed, input, output) || !room(l.allocation, l.allocationUsed, input, output) {
			d.Reason = "budget-exhausted"
			return nil
		}
		expires := l.now.Add(window)
		if l.allocationExpiry.Before(expires) {
			expires = l.allocationExpiry
		}
		if l.budgetExpiry.Before(expires) {
			expires = l.budgetExpiry
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, allocation, sequence, r.RequestSHA256, input, output, expires)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE `+l.budgetTable()+` SET reserved_calls=reserved_calls+1,reserved_input_bytes=reserved_input_bytes+$2,reserved_output_bytes=reserved_output_bytes+$3 WHERE work_item_uuid=$1`, l.workItem, input, output)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE model_allocations SET reserved_calls=reserved_calls+1,reserved_input_bytes=reserved_input_bytes+$2,reserved_output_bytes=reserved_output_bytes+$3 WHERE id=$1`, allocation, input, output)
		if err != nil {
			return err
		}
		d = ModelDecision{Reason: "reserved", Call: ModelCall{ModelCallRef: r, State: "reserved", InputBytes: len(payload), ResponseLimit: l.responseLimit, ExpiresAt: expires}}
		return nil
	})
	if err != nil {
		return ModelDecision{}, err
	}
	return d, nil
}

// ClaimModelCall grants a single dispatch only after a confirmed commit. The
// caller must disable provider retries. Commit errors require reconciliation;
// they NEVER permit dispatch, even if the database may have committed.
func (db *DB) ClaimModelCall(ctx context.Context, r ModelCallRef) (ModelDecision, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d ModelDecision
	if !validModelRef(r) {
		return d, errModelPolicy
	}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		l, err := lockModel(ctx, tx, r.AllocationID)
		if err != nil {
			return err
		}
		c, err := readModelCall(ctx, tx, r)
		if err != nil {
			return err
		}
		if err = refreshModelTime(ctx, tx, &l); err != nil {
			return err
		}
		d.Call = c
		if l.podUID != r.PodUID || c.RequestSHA256 != r.RequestSHA256 {
			d.Reason = "binding-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		if !l.active() || !l.now.Before(c.ExpiresAt) {
			d.Reason = "inactive"
			return nil
		}
		if c.State != "reserved" {
			d.Reason = "already-claimed"
			return nil
		}
		if pending, err := pendingModelHandoff(ctx, tx, l); err != nil {
			return err
		} else if pending {
			d.Reason = "response-handoff-pending"
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE model_calls SET state='started' WHERE allocation_id=$1 AND sequence=$2`, r.AllocationID, r.Sequence)
		if err != nil {
			return err
		}
		d.Call.State = "started"
		d.Reason = "started"
		d.Invoke = true
		return nil
	})
	if err != nil {
		return ModelDecision{}, err
	}
	return d, nil
}

// FinishModelCall persists bounded response metadata only. A response lost
// after completion cannot be replayed; a repeated claim cannot call again.
func (db *DB) FinishModelCall(ctx context.Context, r ModelCallRef, status int, response []byte) (ModelDecision, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d ModelDecision
	if !validModelRef(r) || len(response) > modelmailbox.MaxResponseBytes {
		return d, errModelPolicy
	}
	switch status {
	case 200, 400, 401, 403, 429, 500, 502, 503, 504:
	default:
		return d, errModelPolicy
	}
	digest := modelmailbox.Digest(response)
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		l, err := lockModel(ctx, tx, r.AllocationID)
		if err != nil {
			return err
		}
		c, err := readModelCall(ctx, tx, r)
		if err != nil {
			return err
		}
		if err = refreshModelTime(ctx, tx, &l); err != nil {
			return err
		}
		d.Call = c
		if l.podUID != r.PodUID || c.RequestSHA256 != r.RequestSHA256 {
			d.Reason = "binding-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		if c.State == "completed" {
			var same bool
			err = tx.QueryRowContext(ctx, `SELECT output_bytes=$3 AND response_sha256=$4 AND response_status=$5 FROM model_calls WHERE allocation_id=$1 AND sequence=$2`, r.AllocationID, r.Sequence, len(response), digest, status).Scan(&same)
			if err != nil {
				return err
			}
			if !same {
				d.Reason = "completion-mismatch"
				return stopModel(ctx, tx, l, d.Reason)
			}
			d.Reason = "existing"
			return nil
		}
		if c.State != "started" {
			d.Reason = "not-started"
			return nil
		}
		if !l.active() || !l.now.Before(c.ExpiresAt) || len(response) > c.ResponseLimit {
			d.Reason = "uncertain-outcome"
			if err = stopModel(ctx, tx, l, d.Reason); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE model_calls SET state='ambiguous' WHERE allocation_id=$1 AND sequence=$2`, r.AllocationID, r.Sequence)
			d.Call.State = "ambiguous"
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE model_calls SET state='completed',output_bytes=$3,response_sha256=$4,response_status=$5 WHERE allocation_id=$1 AND sequence=$2`, r.AllocationID, r.Sequence, len(response), digest, status)
		d.Call.State = "completed"
		d.Reason = "completed"
		return err
	})
	if err != nil {
		return ModelDecision{}, err
	}
	return d, nil
}

// StopModelCall is the explicit uncertain-outcome/recovery path. It never
// refunds reservations. A terminal call is never reopened by recovery.
func (db *DB) StopModelCall(ctx context.Context, r ModelCallRef) (ModelDecision, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d ModelDecision
	if !validModelRef(r) {
		return d, errModelPolicy
	}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		l, err := lockModel(ctx, tx, r.AllocationID)
		if err != nil {
			return err
		}
		c, err := readModelCall(ctx, tx, r)
		if err != nil {
			return err
		}
		if err = refreshModelTime(ctx, tx, &l); err != nil {
			return err
		}
		d.Call = c
		if l.podUID != r.PodUID || c.RequestSHA256 != r.RequestSHA256 {
			d.Reason = "binding-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		if c.State != "reserved" && c.State != "started" && !(c.State == "completed" && !c.ResponseHandedOff) {
			d.Reason = "terminal"
			return nil
		}
		next := "stopped"
		if c.State == "started" || c.State == "completed" {
			next = "ambiguous"
		}
		if err = stopModel(ctx, tx, l, "uncertain-outcome"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE model_calls SET state=$3 WHERE allocation_id=$1 AND sequence=$2`, r.AllocationID, r.Sequence, next)
		d.Reason = next
		d.Call.State = next
		return err
	})
	if err != nil {
		return ModelDecision{}, err
	}
	return d, nil
}

// RecoverExpiredModelCalls examines a bounded batch; the stop operation takes
// the same locks as completion. Expiration is immutable, so the initial query
// cannot make a newly extended call expire. No payload or credential is read.
func (db *DB) RecoverExpiredModelCalls(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := db.Pool.QueryContext(ctx, `SELECT c.allocation_id,a.pod_uid,c.sequence,c.request_sha256 FROM model_calls c JOIN model_allocations a ON a.id=c.allocation_id WHERE (c.state IN ('reserved','started') OR (c.state='completed' AND c.response_handed_off_at IS NULL)) AND c.expires_at<=clock_timestamp() ORDER BY c.expires_at LIMIT 100`)
	if err != nil {
		return 0, err
	}
	var refs []ModelCallRef
	for rows.Next() {
		var r ModelCallRef
		if err = rows.Scan(&r.AllocationID, &r.PodUID, &r.Sequence, &r.RequestSHA256); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range refs {
		d, err := db.StopModelCall(ctx, r)
		if err != nil {
			return n, err
		}
		if d.Reason == "stopped" || d.Reason == "ambiguous" {
			n++
		}
	}
	return n, nil
}

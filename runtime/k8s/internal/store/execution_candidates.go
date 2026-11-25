package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// CandidateRecord identifies unvalidated bytes, not acceptance or publication.
type CandidateRecord struct {
	RequestID, AllocationID, PodUID, SourceSHA                       string
	SourceTreeSHA256, CandidateTreeSHA256, ExportSHA256, DeltaSHA256 string
	ExportBytes, Changes, ModelCalls                                 int
	// GenerationEnded is empty for a clean generator exit and
	// candidate.GenerationEndedStepBudget when the generator ended on its
	// tool-step budget. It is derived from the durable export receipt and the
	// retained worker-exit observation, for evaluation only.
	GenerationEnded string
}

// CaptureLifecycleCandidate serializes capture with every model reservation.
// It closes the whole work budget, without refund, in the same transaction as
// the immutable delta and lifecycle phase. No prior result is ever adopted.
func (db *DB) CaptureLifecycleCandidate(ctx context.Context, l Lifecycle, allocationID string, export []byte, delta []byte) (CandidateRecord, error) {
	d, err := candidate.Decode(delta)
	if err != nil || len(export) < 1 || len(export) > workspacebundle.MaxWireBytes || !modelUUID.MatchString(allocationID) {
		return CandidateRecord{}, ErrExecution
	}
	result := CandidateRecord{RequestID: l.Claim.ID, AllocationID: allocationID, SourceSHA: d.SourceSHA, SourceTreeSHA256: d.SourceTreeSHA256, CandidateTreeSHA256: d.CandidateTreeSHA256,
		ExportSHA256: workspacebundle.Digest(export), ExportBytes: len(export), DeltaSHA256: workspacebundle.Digest(delta), Changes: len(d.Changes)}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		phase, _, pod, e := lockLifecycle(ctx, tx, l)
		if e != nil || phase != "export-ready" {
			return ErrExecution
		}
		result.PodUID = pod
		var receiptRaw []byte
		if tx.QueryRowContext(ctx, `SELECT result FROM execution_effects WHERE request_id=$1 AND kind='export' AND completed_at IS NOT NULL FOR UPDATE`, l.Claim.ID).Scan(&receiptRaw) != nil {
			return ErrExecution
		}
		var receipt LifecycleResult
		if json.Unmarshal(receiptRaw, &receipt) != nil || receipt.UID != pod || receipt.ExitCode == nil || receipt.Digest != result.ExportSHA256 || receipt.Bytes != result.ExportBytes {
			return ErrExecution
		}
		switch *receipt.ExitCode {
		case 0:
		case candidate.StepBudgetExitCode:
			// A step-budget exit is captured only with its worker-exit
			// observation already retained for the same Pod, so the durable
			// diagnostic always marks the candidate.
			var observed bool
			if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM execution_failures WHERE request_id=$1 AND pod_uid=$2 AND exit_code=$3)`, l.Claim.ID, pod, candidate.StepBudgetExitCode).Scan(&observed) != nil || !observed {
				return ErrExecution
			}
			result.GenerationEnded = candidate.GenerationEndedStepBudget
		default:
			return ErrExecution
		}
		ledger, e := lockModel(ctx, tx, allocationID)
		if e != nil || ledger.state != "active" {
			return ErrExecution
		}
		var work string
		if tx.QueryRowContext(ctx, `SELECT b.work_item_uuid
 FROM execution_models e JOIN model_allocations a ON a.id=e.allocation_id JOIN model_budgets b ON b.work_item_uuid=a.work_item_uuid
 JOIN execution_requests r ON r.id=e.request_id JOIN work_items w ON w.uuid=r.work_item_uuid
 WHERE e.request_id=$1 AND a.id=$2 AND a.pod_uid=$3 AND b.source_sha=$4
 AND w.status='running' AND r.work_item_uuid=a.work_item_uuid AND r.repository=b.repository AND r.attempt=a.attempt
 `, l.Claim.ID, allocationID, pod, d.SourceSHA).Scan(&work) != nil || work != ledger.workItem {
			return ErrExecution
		}
		var pending int
		if tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE c.state<>'completed' OR c.response_handed_off_at IS NULL)
 FROM model_calls c JOIN model_allocations a ON a.id=c.allocation_id WHERE a.work_item_uuid=$1
 AND (($2 AND a.id=$3::uuid) OR (NOT $2 AND NOT EXISTS(SELECT 1 FROM repair_model_budgets rb WHERE rb.allocation_id=a.id)))`, work, ledger.derived, allocationID).Scan(&result.ModelCalls, &pending) != nil || pending != 0 || result.ModelCalls < 1 || result.ModelCalls > 128 {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO execution_candidates(request_id,allocation_id,pod_uid,source_sha,source_tree_sha256,candidate_tree_sha256,export_sha256,export_bytes,delta_sha256,delta,changes,model_calls)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, result.RequestID, result.AllocationID, result.PodUID, result.SourceSHA, result.SourceTreeSHA256, result.CandidateTreeSHA256, result.ExportSHA256, result.ExportBytes, result.DeltaSHA256, delta, result.Changes, result.ModelCalls); e != nil {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `UPDATE `+ledger.budgetTable()+` SET state='stopped',stop_reason='candidate-captured' WHERE work_item_uuid=$1`, work); e != nil {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `UPDATE execution_lifecycles SET phase='candidate-captured',updated_at=clock_timestamp() WHERE request_id=$1`, l.Claim.ID); e != nil {
			return ErrExecution
		}
		var current bool
		if tx.QueryRowContext(ctx, `SELECT LEAST($1::timestamptz,$2::timestamptz)>clock_timestamp()`, ledger.budgetExpiry, ledger.allocationExpiry).Scan(&current) != nil || !current {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, l.Claim)
	})
	if err != nil {
		return CandidateRecord{}, ErrExecution
	}
	return result, nil
}

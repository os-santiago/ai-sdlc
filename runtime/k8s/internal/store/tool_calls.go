package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"
)

// MaxToolCalls bounds the journaled tool calls of one model allocation (one
// generation attempt). It matches the tool_calls.sequence CHECK.
const MaxToolCalls = 8

// ErrToolCall refuses a tool journal transition: invalid input, an allocation
// that is not live under a claimed execution, a call already in flight, the
// per-allocation bound, or a call that is no longer started.
var ErrToolCall = errors.New("tool call journal transition refused")

// ToolCallRecord is the outcome of one finished tool run. Only identities and
// digests are journaled: never the tool input, diff, summary body or logs.
type ToolCallRecord struct {
	PipelineRun   string // the PipelineRun name; empty only for an error before it existed
	Revision      string // the 40-hex base revision the run used
	Tree          string // the 40-hex tree after the candidate diff; empty without a diff
	SummarySHA256 string // sha256 of the bounded summary
	State         string // passed | failed | error
}

var toolPipelineRun = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func optional(value string, pattern *regexp.Regexp) bool {
	return value == "" || pattern.MatchString(value)
}

// Validate mirrors the table CHECKs so a refusal never depends on a SQL error.
func (r ToolCallRecord) Validate() error {
	if !optional(r.PipelineRun, toolPipelineRun) || !optional(r.Revision, modelSHA) || !optional(r.Tree, modelSHA) || !optional(r.SummarySHA256, modelDigest) {
		return ErrToolCall
	}
	switch r.State {
	case "passed", "failed":
		if r.PipelineRun == "" || r.Revision == "" || r.SummarySHA256 == "" {
			return ErrToolCall
		}
	case "error":
	default:
		return ErrToolCall
	}
	return nil
}

// BeginToolCall journals a tool call before it runs and returns its sequence
// (1..MaxToolCalls). The allocation must belong to a live claimed execution
// and be unexpired; the claim row is share-locked until commit, so claim
// recovery (which locks it for update) cannot stop the execution between this
// check and the insert and miss the new row. A call is refused while another
// call of the allocation is started, and beyond MaxToolCalls.
func (db *DB) BeginToolCall(ctx context.Context, allocationID, requestSHA256 string) (int, error) {
	if !modelUUID.MatchString(allocationID) || !modelDigest.MatchString(requestSHA256) {
		return 0, ErrToolCall
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return 0, ErrToolCall
	}
	sequence := 0
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var live bool
		// Lock order: execution request, then allocation, as claim recovery.
		err := tx.QueryRowContext(ctx, `SELECT r.state='claimed' AND r.claim_until>clock_timestamp() AND r.expires_at>clock_timestamp() AND a.expires_at>clock_timestamp()
 FROM execution_models e JOIN execution_requests r ON r.id=e.request_id JOIN model_allocations a ON a.id=e.allocation_id
 WHERE e.allocation_id=$1 FOR SHARE OF r FOR UPDATE OF a`, allocationID).Scan(&live)
		if err != nil || !live {
			return ErrToolCall
		}
		var next int
		var started bool
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(sequence),0)+1,COALESCE(bool_or(state='started'),false) FROM tool_calls WHERE allocation_id=$1`, allocationID).Scan(&next, &started); err != nil || started || next > MaxToolCalls {
			return ErrToolCall
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tool_calls(allocation_id,sequence,request_sha256) VALUES($1,$2,$3)`, allocationID, next, requestSHA256); err != nil {
			return ErrToolCall
		}
		sequence = next
		return nil
	})
	if err != nil {
		return 0, ErrToolCall
	}
	return sequence, nil
}

// FinishToolCall records the outcome of a started call exactly once. A call
// that recovery already abandoned, or one that was already finished, is
// refused: the journal never rewrites an outcome.
func (db *DB) FinishToolCall(ctx context.Context, allocationID string, sequence int, r ToolCallRecord) error {
	if !modelUUID.MatchString(allocationID) || sequence < 1 || sequence > MaxToolCalls || r.Validate() != nil {
		return ErrToolCall
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return ErrToolCall
	}
	result, err := db.Pool.ExecContext(ctx, `UPDATE tool_calls SET state=$3,pipeline_run=$4,revision=$5,tree=$6,summary_sha256=$7,finished_at=clock_timestamp()
 WHERE allocation_id=$1 AND sequence=$2 AND state='started'`, allocationID, sequence, r.State, r.PipelineRun, r.Revision, r.Tree, r.SummarySHA256)
	if err != nil {
		return ErrToolCall
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrToolCall
	}
	return nil
}

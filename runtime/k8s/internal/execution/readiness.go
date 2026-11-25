package execution

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/normalizedevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// ReadinessLoader reads current reviewed policy and intake configuration, never
// values supplied by issue content. It is called before and after claim waits.
type ReadinessLoader func() (authority.Policy, intake.Config, error)

func readiness(ctx context.Context, db *store.DB, p authority.Policy, config intake.Config, c Command) error {
	_, err := verifiedSnapshot(ctx, db, p, config, c)
	return err
}

func verifiedSnapshot(ctx context.Context, db *store.DB, p authority.Policy, config intake.Config, c Command) (intake.Snapshot, error) {
	if c.Repair != nil {
		if _, _, err := repairPreparation(ctx, db, p, c); err != nil {
			return intake.Snapshot{}, ErrAuthority
		}
		return verifiedSnapshotStatus(ctx, db, p, config, c, "running")
	}
	return verifiedSnapshotStatus(ctx, db, p, config, c, "ready")
}
func verifiedSnapshotStatus(ctx context.Context, db *store.DB, p authority.Policy, config intake.Config, c Command, expectedStatus string) (intake.Snapshot, error) {
	if expectedStatus != "ready" && expectedStatus != "running" {
		return intake.Snapshot{}, ErrAuthority
	}
	if config.Validate(p.Repository) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	age := 2 * config.PollSeconds
	if age > 300 {
		age = 300
	}
	e, err := db.ReadExecutionEvidence(ctx, c.WorkItemID, c.Repository, c.IssueNumber, age)
	if errors.Is(err, store.ErrEvidenceStale) {
		// Stale evidence never authorizes. A changed snapshot still refuses
		// at once; only otherwise unchanged evidence is reported as stale.
		if _, err := validateSnapshotEvidence(p, config, c, expectedStatus, e); err != nil {
			return intake.Snapshot{}, ErrAuthority
		}
		return intake.Snapshot{}, errSnapshotStale
	}
	if err != nil {
		if errors.Is(err, store.ErrObservationUnavailable) {
			return intake.Snapshot{}, err
		}
		return intake.Snapshot{}, ErrAuthority
	}
	return validateSnapshotEvidence(p, config, c, expectedStatus, e)
}

// errSnapshotStale: the signed issue evidence is unchanged, but its intake
// poll watermark or issue observation is older than the evidence window, for
// example after one incomplete intake scan. A later complete scan can refresh
// it; until then nothing is authorized.
// It wraps ErrAuthority, so every caller that refused before still refuses.
var errSnapshotStale = fmt.Errorf("%w: issue evidence older than the window", ErrAuthority)

// CandidateSnapshot verifies current issue/admission/plan evidence for the
// independent verifier. The original signed execution remains bounded by its
// own expiry; a verification command cannot extend generation authority.
func CandidateSnapshot(ctx context.Context, db *store.DB, p authority.Policy, config intake.Config, c Command) (intake.Snapshot, error) {
	if config.Validate(p.Repository) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	age := 2 * config.PollSeconds
	if age > 300 {
		age = 300
	}
	e, err := db.ReadVerificationEvidence(ctx, c.WorkItemID, c.Repository, c.IssueNumber, age)
	if err != nil {
		if errors.Is(err, store.ErrObservationUnavailable) {
			return intake.Snapshot{}, err
		}
		return intake.Snapshot{}, ErrAuthority
	}
	return validateSnapshotEvidence(p, config, c, "running", e)
}

func CandidateSnapshotTx(ctx context.Context, tx *sql.Tx, p authority.Policy, config intake.Config, c Command) (intake.Snapshot, error) {
	if config.Validate(p.Repository) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	e, err := store.ReadVerificationEvidenceTx(ctx, tx, c.WorkItemID, c.Repository, c.IssueNumber, min(2*config.PollSeconds, 300))
	if err != nil {
		return intake.Snapshot{}, err
	}
	return validateSnapshotEvidence(p, config, c, "running", e)
}

// PublicationSnapshot uses the same current low-risk issue checks under the
// separate release-manager identity. It cannot advance generation or review.
func PublicationSnapshot(ctx context.Context, db *store.DB, p authority.Policy, config intake.Config, c Command) (intake.Snapshot, error) {
	if config.Validate(p.Repository) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	age := min(2*config.PollSeconds, 300)
	e, err := db.ReadPublicationEvidence(ctx, c.WorkItemID, c.Repository, c.IssueNumber, age)
	if err != nil {
		return intake.Snapshot{}, err
	}
	return validateSnapshotEvidence(p, config, c, "running", e)
}

// RepairSnapshot uses the orchestrator identity and the same bounded current
// issue proof as generation. A repair never makes failed or superseded work ready.
func RepairSnapshot(ctx context.Context, tx *sql.Tx, p authority.Policy, config intake.Config, c Command) (intake.Snapshot, error) {
	if config.Validate(p.Repository) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	e, err := store.ReadRepairEvidence(ctx, tx, c.WorkItemID, c.Repository, c.IssueNumber, min(2*config.PollSeconds, 300))
	if err != nil {
		return intake.Snapshot{}, err
	}
	return validateSnapshotEvidence(p, config, c, "running", e)
}

func validateSnapshotEvidence(p authority.Policy, config intake.Config, c Command, expectedStatus string, e store.ExecutionEvidence) (intake.Snapshot, error) {
	if e.Status != expectedStatus || e.IntakeGeneration != c.Generation || e.AdmissionGeneration != c.Generation || e.PlanningGeneration != c.Generation || e.ObservationAuthority != c.AuthorityRevision {
		return intake.Snapshot{}, ErrAuthority
	}
	var observed, admitted intake.Snapshot
	var decision intake.Admission
	if authority.DecodeStrict(e.IntakePayload, &observed) != nil || authority.DecodeStrict(e.AdmissionPayload, &admitted) != nil || authority.DecodeStrict(e.PlanningPayload, &decision) != nil ||
		observed.Validate(config) != nil || admitted.Validate(config) != nil || decision.Validate(config) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	canonical, _ := json.Marshal(observed)
	other, _ := json.Marshal(admitted)
	planned, _ := json.Marshal(decision.Snapshot)
	if !bytes.Equal(canonical, other) || !bytes.Equal(canonical, planned) || !observed.Eligible || observed.AuthorityRevision != p.Revision() ||
		observed.Generation != c.Generation || observed.Issue.Number != c.IssueNumber || observed.Repository != c.Repository || observed.BaseSHA != c.SourceSHA ||
		observed.ID() != e.IntakeEvent || observed.ID() != e.AdmissionEvent || observed.ID() != e.PlanCorrelation || observed.ID() != e.AdmissionCorrelation ||
		observed.Revision != e.IntakeRevision || observed.ConfigDigest != e.ConfigDigest ||
		decision.ID() != c.AdmissionID || decision.ID() != e.PlanningEvent || decision.Decision != "accepted" || decision.Risk != "low" ||
		e.Decision != decision.Decision || e.Risk != decision.Risk || e.PolicyVersion != decision.PolicyVersion {
		return intake.Snapshot{}, ErrAuthority
	}
	key := authority.Digest([]byte(observed.Revision + "\x00" + strconv.FormatBool(observed.Eligible) + "\x00" + observed.ConfigDigest + "\x00" + observed.AuthorityRevision))
	if e.ContentKey != key {
		return intake.Snapshot{}, ErrAuthority
	}
	var issues []int
	var criteria, dependencies, reasons, gates []string
	if authority.DecodeStrict(e.IssueNumbers, &issues) != nil || authority.DecodeStrict(e.Criteria, &criteria) != nil || authority.DecodeStrict(e.Dependencies, &dependencies) != nil ||
		authority.DecodeStrict(e.Reasons, &reasons) != nil || authority.DecodeStrict(e.Gates, &gates) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	expected := normalizedevent.ExtractAcceptanceCriteria(observed.Issue.Body)
	if len(issues) != 1 || issues[0] != c.IssueNumber || dependencies == nil || len(dependencies) != 0 || len(criteria) < 1 || len(criteria) > 8 ||
		!reflect.DeepEqual(criteria, expected) || !reflect.DeepEqual(reasons, decision.Reasons) || !reflect.DeepEqual(gates, decision.RequiredGates) || len(gates) == 0 {
		return intake.Snapshot{}, ErrAuthority
	}
	for _, criterion := range criteria {
		if strings.TrimSpace(criterion) == "" || len(criterion) > 4096 {
			return intake.Snapshot{}, ErrAuthority
		}
	}
	for _, gate := range gates {
		if len(gate) == 0 || len(gate) > 128 {
			return intake.Snapshot{}, ErrAuthority
		}
	}
	return observed, nil
}

package execution

import (
	"bytes"
	"context"
	"encoding/json"
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
	if config.Validate(p.Repository) != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	age := 2 * config.PollSeconds
	if age > 300 {
		age = 300
	}
	e, err := db.ReadExecutionEvidence(ctx, c.WorkItemID, c.Repository, c.IssueNumber, age)
	if err != nil {
		return intake.Snapshot{}, ErrAuthority
	}
	if e.Status != "ready" || e.IntakeGeneration != c.Generation || e.AdmissionGeneration != c.Generation || e.PlanningGeneration != c.Generation || e.ObservationAuthority != c.AuthorityRevision {
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

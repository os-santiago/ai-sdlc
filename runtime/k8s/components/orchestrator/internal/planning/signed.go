package planning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repairflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/normalizedevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func SignedRoutes(component *bootstrap.Component, config intake.Config) service.RouteRegistrar {
	return func(mux *http.ServeMux) {
		in := delivery.Inbox{DB: component.DB, Target: "orchestrator"}
		mux.Handle("POST /internal/plan-admission", in.Handler(component.Authority, "plan-admission", "/internal/plan-admission", signedEffect(component, config)))
		if os.Getenv("AI_SDLC_RUNTIME_MODE") == "verification" && os.Getenv("AI_SDLC_INDEPENDENT_REVIEW") == "true" {
			load := repairflow.InstalledLoader(component.Authority, os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"), os.Getenv("AI_SDLC_INTAKE_CONFIG_PATH"), "/etc/ai-sdlc/review/config.json")
			mux.Handle("POST /internal/repair-candidate", in.Handler(component.Authority, "repair-candidate", "/internal/repair-candidate", repairflow.Effect(load)))
			mux.Handle("POST /internal/stop-rejected-work", in.Handler(component.Authority, "stop-rejected-work", "/internal/stop-rejected-work", repairflow.StopEffect(func() (authority.Policy, error) { p, _, e := component.Authority(); return p, e })))
		}
	}
}
func signedEffect(component *bootstrap.Component, config intake.Config) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, raw []byte) ([]byte, error) {
		var admission intake.Admission
		if authority.DecodeStrict(raw, &admission) != nil || admission.Validate(config) != nil || admission.ID() != receipt.ID || admission.Snapshot.Repository != receipt.Repository {
			return nil, errors.New("invalid admission binding")
		}
		snapshot := admission.Snapshot
		policy, identity, err := component.Authority()
		if err != nil {
			return nil, err
		}
		if snapshot.AuthorityRevision != policy.Revision() || (policy.Autonomy != "shadow" && !bootstrap.CandidateAutonomy(policy)) {
			return nil, errors.New("stale planning authority")
		}
		advanced, err := store.AdvanceIssue(ctx, tx, "orchestrator", snapshot.Repository, snapshot.Issue.Number, snapshot.Generation, receipt.ID, raw)
		if err != nil {
			return nil, err
		}
		if !advanced {
			return []byte(`{"ignored":"superseded"}`), nil
		}
		// Preserve older plans as evidence but never leave a superseded item
		// eligible for later execution. Shadow has no running generated Jobs.
		if err := store.BlockSupersededIssue(ctx, tx, snapshot.Repository, snapshot.Issue.Number); err != nil {
			return nil, err
		}
		if admission.Decision != "accepted" {
			return []byte(`{"recorded":true,"planCreated":false}`), nil
		}
		state := Blocked
		var profile []byte
		var installed execution.RuntimeConfig
		if bootstrap.CandidateAutonomy(policy) && admission.Risk == "low" {
			installed, profile, err = execution.LoadRuntime(os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"))
			if err != nil {
				return nil, err
			}
			state = Ready
		}
		criteria := normalizedevent.ExtractAcceptanceCriteria(snapshot.Issue.Body)
		planID, itemID := store.NewID(), store.NewID()
		plan := Plan{ID: planID, Repository: snapshot.Repository, Items: []WorkItem{{ID: "issue-" + strconv.Itoa(snapshot.Issue.Number), IssueNumbers: []int{snapshot.Issue.Number}, AcceptanceCriteria: criteria, Status: state}}}
		if err := Validate(plan); err != nil {
			return nil, err
		}
		if err := store.UpsertPlan(ctx, tx, store.PlanRecord{ID: planID, Repository: snapshot.Repository, CorrelationID: snapshot.ID()}); err != nil {
			return nil, err
		}
		if err := store.UpsertWorkItem(ctx, tx, store.WorkItemRecord{PlanID: planID, ID: plan.Items[0].ID, UUID: itemID, IssueNumbers: plan.Items[0].IssueNumbers, AcceptanceCriteria: criteria, DependsOn: []string{}, Status: string(state)}); err != nil {
			return nil, err
		}
		if state == Ready {
			command := execution.Command{Version: execution.Version, WorkItemID: itemID, Attempt: 1, Repository: snapshot.Repository, IssueNumber: snapshot.Issue.Number, Generation: snapshot.Generation, AdmissionID: admission.ID(), SourceSHA: snapshot.BaseSHA, ProfileSHA256: workspacebundle.Digest(profile), AuthorityRevision: policy.Revision(), ExpiresAt: time.Now().Add(execution.CommandLifetime(installed.Profile.MaxSeconds, execution.StartSecondsFor(snapshot.Repository))).UnixMilli()}
			payload, e := json.Marshal(command)
			if e != nil {
				return nil, e
			}
			current, _, e := component.Authority()
			if e != nil || current.Revision() != policy.Revision() {
				return nil, errors.New("planning authority changed")
			}
			if e = delivery.Append(ctx, tx, policy, identity, command.ID(), "execute-work", payload); e != nil {
				return nil, e
			}
		}
		return json.Marshal(map[string]any{"recorded": true, "planId": planID, "mode": policy.Autonomy, "generation": snapshot.Generation})
	}
}

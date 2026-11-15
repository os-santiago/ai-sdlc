package admission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/normalizedevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func SignedRoutes(component *bootstrap.Component, config intake.Config, trusted contract.Contract) service.RouteRegistrar {
	return func(mux *http.ServeMux) {
		in := delivery.Inbox{DB: component.DB, Target: "admission-controller"}
		mux.Handle("POST /internal/admit-issue", in.Handler(component.Authority, "admit-issue", "/internal/admit-issue", signedEffect(component, config, trusted)))
	}
}
func signedEffect(component *bootstrap.Component, config intake.Config, trusted contract.Contract) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, raw []byte) ([]byte, error) {
		var snapshot intake.Snapshot
		if authority.DecodeStrict(raw, &snapshot) != nil || snapshot.Validate(config) != nil || snapshot.ID() != receipt.ID || snapshot.Repository != receipt.Repository {
			return nil, errors.New("invalid intake binding")
		}
		policy, identity, err := component.Authority()
		if err != nil {
			return nil, err
		}
		if snapshot.AuthorityRevision != policy.Revision() || policy.Autonomy != "shadow" {
			return nil, errors.New("stale shadow authority")
		}
		advanced, err := store.AdvanceIssue(ctx, tx, "admission-controller", snapshot.Repository, snapshot.Issue.Number, snapshot.Generation, receipt.ID, raw)
		if err != nil {
			return nil, err
		}
		if !advanced {
			return []byte(`{"ignored":"superseded"}`), nil
		}
		p := PolicyFromContract(trusted)
		p.RequireAuthorizedAdmission = true
		result := Evaluate(Issue{Repository: snapshot.Repository, Number: snapshot.Issue.Number, Revision: snapshot.Revision, Title: snapshot.Issue.Title, Body: snapshot.Issue.Body, AcceptanceCriteria: normalizedevent.ExtractAcceptanceCriteria(snapshot.Issue.Body), RequestedBy: snapshot.Issue.User.Login, AuthorizedBy: snapshot.AuthorizedActor.Login}, p)
		if !snapshot.Eligible {
			result.Decision = Rejected
			result.Reasons = []string{"Issue is closed or its current admission authorization was withdrawn."}
		}
		decision := intake.Admission{Snapshot: snapshot, Decision: string(result.Decision), Risk: result.Risk, Reasons: result.Reasons, RequiredGates: result.RequiredGates, PolicyVersion: result.PolicyVersion}
		if err := decision.Validate(config); err != nil {
			return nil, err
		}
		if err := store.UpsertAdmission(ctx, tx, store.AdmissionRecord{Repository: snapshot.Repository, IssueNumber: snapshot.Issue.Number, IssueRevision: snapshot.Revision + ":" + strconv.FormatInt(snapshot.Generation, 10), Decision: decision.Decision, Risk: decision.Risk, PolicyVersion: decision.PolicyVersion, Reasons: decision.Reasons, RequiredGates: decision.RequiredGates, CorrelationID: snapshot.ID()}); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(decision)
		if err != nil {
			return nil, err
		}
		if err := delivery.Append(ctx, tx, policy, identity, decision.ID(), "plan-admission", payload); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"decision": decision.Decision, "generation": snapshot.Generation})
	}
}

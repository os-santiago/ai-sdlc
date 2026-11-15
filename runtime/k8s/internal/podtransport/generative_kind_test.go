package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	core "k8s.io/api/core/v1"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// This observer exists only in the opt-in developer fixture. The production
// dispatcher still exposes no completion bodies to its caller.
type fixtureCompletionObserver struct {
	session *Session
	t       *testing.T
}

func (p *fixtureCompletionObserver) Publish(ctx context.Context, e modelmailbox.Envelope) error {
	if err := p.session.Publish(ctx, e); err != nil {
		return err
	}
	var body struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(e.Payload, &body) != nil || len(body.Choices) != 1 {
		return errors.New("invalid fixture completion")
	}
	names := []string{}
	for _, call := range body.Choices[0].Message.ToolCalls {
		names = append(names, call.Function.Name)
	}
	p.t.Logf("completion sequence=%d reason=%s proposedTools=%v", e.Sequence, body.Choices[0].FinishReason, names)
	return nil
}

// No production route invokes this fixture. The input explicitly names a
// disposable database, fixture Pod and authenticated loopback completion host.
func TestRealCodexKubernetesMailboxFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_GENERATIVE_FIXTURE")
	if path == "" {
		t.Skip("explicit real-model kind fixture required")
	}
	raw, err := os.ReadFile(path)
	var input struct {
		Config                       Config
		Binding                      Binding
		DatabaseURL, Endpoint, Token string
	}
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &input) != nil {
		t.Fatal("invalid private generative fixture input")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	client, err := NewClient(input.Config)
	if err != nil {
		t.Fatal("fixture client")
	}
	session, err := Bind(ctx, client, input.Binding)
	if err != nil {
		t.Fatal("fixture Pod binding")
	}
	a, err := executor.ParseAssignment(input.Binding.Assignment, modelmailbox.Digest(input.Binding.Assignment), time.Now())
	if err != nil {
		t.Fatal("fixture assignment")
	}
	profile, err := executor.ParseProfile(input.Binding.Profile, a.ProfileSHA256)
	if err != nil || profile.Model != "codex-cli" || profile.MaxSteps != 24 || profile.MaxSeconds != 180 {
		t.Fatal("fixture profile")
	}
	db, err := store.Open(ctx, input.DatabaseURL)
	if err != nil {
		t.Fatal("fixture database")
	}
	defer db.Close()
	if db.Migrate(ctx) != nil {
		t.Fatal("fixture migration")
	}
	plan := store.NewID()
	if store.UpsertPlan(ctx, db.Pool, store.PlanRecord{ID: plan, Repository: a.Repository, CorrelationID: store.NewID()}) != nil {
		t.Fatal("fixture plan")
	}
	if store.UpsertWorkItem(ctx, db.Pool, store.WorkItemRecord{PlanID: plan, ID: "fixture", UUID: a.WorkItemID, Status: "running", IssueNumbers: []int{1}, AcceptanceCriteria: a.AcceptanceCriteria}) != nil {
		t.Fatal("fixture work item")
	}
	budget := store.ModelBudget{WorkItem: a.WorkItemID, Repository: a.Repository, SourceSHA: a.SourceSHA, PolicyDigest: a.ProfileSHA256, Model: profile.Model, Limits: store.ModelLimits{Calls: 8, InputBytes: 1024 * 1024, OutputBytes: 512 * 1024}, ExpiresAt: time.Now().Add(10 * time.Minute)}
	if db.CreateModelBudget(ctx, budget) != nil {
		t.Fatal("fixture budget")
	}
	allocation := store.ModelAllocation{ID: store.NewID(), WorkItem: a.WorkItemID, PodUID: input.Binding.PodUID, Attempt: a.Attempt, Limits: budget.Limits, ResponseLimit: 64 * 1024, ExpiresAt: time.Now().Add(5 * time.Minute)}
	if db.CreateModelAllocation(ctx, allocation) != nil {
		t.Fatal("fixture allocation")
	}
	defer func() {
		proofCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		var completed, handed, uncertain int
		err := db.Pool.QueryRowContext(proofCtx, `SELECT count(*) FILTER (WHERE state='completed'),count(response_handed_off_at),count(*) FILTER (WHERE state IN ('started','stopped','ambiguous')) FROM model_calls WHERE allocation_id=$1`, allocation.ID).Scan(&completed, &handed, &uncertain)
		if err == nil {
			t.Logf("GENERATIVE_LEDGER completed=%d handed=%d uncertain=%d", completed, handed, uncertain)
		}
	}()
	provider, err := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: input.Endpoint, BearerToken: input.Token, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal("fixture provider")
	}
	observer := &fixtureCompletionObserver{session: session, t: t}
	dispatcher := modelbroker.Dispatcher{DB: db, Provider: provider, Mailbox: observer, Authority: syntheticFixtureAuthority{}}
	seen := map[uint64]bool{}
	for {
		if fixtureWorkerExited(ctx, session) {
			break
		}
		request, err := session.ReadRequest(ctx)
		if errors.Is(err, modelmailbox.ErrPending) {
			select {
			case <-time.After(50 * time.Millisecond):
				continue
			case <-ctx.Done():
				t.Fatal("fixture request deadline")
			}
		}
		if err != nil {
			if fixtureWorkerExited(ctx, session) {
				break
			}
			t.Fatal("fixture mailbox read")
		}
		if seen[request.Sequence] {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if len(seen) >= budget.Limits.Calls {
			t.Fatal("fixture call bound")
		}
		var requestBody struct {
			Messages []struct{ Role, Name, Content string }
		}
		json.Unmarshal(request.Payload, &requestBody)
		for _, message := range requestBody.Messages {
			if message.Role == "tool" {
				classification := "result"
				if strings.HasPrefix(strings.ToLower(message.Content), "error") {
					classification = "tool-error"
				}
				if classification == "tool-error" && strings.Contains(strings.ToLower(message.Content), "permission") {
					classification = "permission"
				}
				if classification == "tool-error" && strings.Contains(strings.ToLower(message.Content), "read-only") {
					classification = "read-only"
				}
				t.Logf("tool-result name=%s class=%s", message.Name, classification)
			}
		}
		result, err := dispatcher.Deliver(ctx, allocation.ID, input.Binding.Profile, request)
		if err != nil || !result.DispatchAttempted || !result.HandoffConfirmed {
			t.Fatalf("fixture dispatch failed: sequence=%d outcome=%s", request.Sequence, result.Outcome)
		}
		seen[request.Sequence] = true
		duplicate, err := dispatcher.Deliver(ctx, allocation.ID, input.Binding.Profile, request)
		if err != nil || duplicate.DispatchAttempted || !duplicate.HandoffConfirmed {
			t.Fatal("fixture duplicate dispatch")
		}
	}
	var completed, handed int
	if db.Pool.QueryRowContext(ctx, `SELECT count(*),count(response_handed_off_at) FROM model_calls WHERE allocation_id=$1 AND state='completed'`, allocation.ID).Scan(&completed, &handed) != nil || completed != len(seen) || handed != len(seen) || len(seen) < 2 {
		t.Fatal("fixture durable counts")
	}
	t.Logf("GENERATIVE_HANDOFFS=%d", len(seen))
}

// A completion stop can trigger SCC's own validation feedback and another model
// request. Only an independently observed terminated worker ends this fixture.
func fixtureWorkerExited(ctx context.Context, s *Session) bool {
	if _, err := s.observe(ctx, true); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.podURL(), nil)
	if err != nil {
		return false
	}
	if s.client.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.client.config.BearerToken)
	}
	response, err := s.client.http.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPodBytes+1))
	if err != nil || len(raw) > maxPodBytes {
		return false
	}
	var pod core.Pod
	if json.Unmarshal(raw, &pod) != nil || string(pod.UID) != s.identity.PodUID {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "worker" && status.State.Terminated != nil {
			return true
		}
	}
	return false
}

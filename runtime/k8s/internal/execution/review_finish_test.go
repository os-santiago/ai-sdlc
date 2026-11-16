package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestReviewProofRequiresExactEvidenceAndCleanup(t *testing.T) {
	f, db, r := reviewJournalFixture(t)
	ctx := context.Background()
	c := claimReview(t, db, r)
	var tree string
	if f.Admin.Pool.QueryRow(`SELECT candidate_tree_sha256 FROM execution_candidates WHERE request_id=$1`, r.CandidateID).Scan(&tree) != nil {
		t.Fatal("candidate tree")
	}
	pod := store.NewID()
	for _, kind := range []string{"config", "pod", "import", "start"} {
		e, err := db.BeginReviewEffect(ctx, c, kind)
		if err != nil {
			t.Fatal(err)
		}
		uid, digest := pod, ""
		if kind == "config" {
			uid = store.NewID()
		}
		if kind == "import" {
			digest = tree
		}
		if db.AcknowledgeReviewEffect(ctx, e, uid, digest) != nil {
			t.Fatal("effect")
		}
	}
	for sequence := 1; sequence <= 2; sequence++ {
		call, e := db.BeginReviewModelCall(ctx, c, pod, sequence, []byte(`{"model":"review-model","messages":[{"role":"user","content":"review"}]}`))
		if e != nil {
			t.Fatal(e)
		}
		message := map[string]any{"role": "assistant", "content": "Inspected source.\nVERDICT: approve"}
		finish := "stop"
		if sequence == 1 {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "read-1", "type": "function", "function": map[string]string{"name": "read_file", "arguments": `{"path":"src/a.js"}`}}}
			finish = "tool_calls"
		}
		response, _ := json.Marshal(map[string]any{"model": "review-model", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}})
		observed, e := modelbroker.ObserveReviewResponse(response, "review-model")
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(observed)
		if db.CompleteReviewModelCall(ctx, call, response, raw) != nil || db.AcknowledgeReviewModelCall(ctx, call, authority.Digest(response)) != nil {
			t.Fatal("model receipt")
		}
	}
	evidence, e := db.ReadReviewModelEvidence(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(evidence)
	proof := map[string]any{"version": "ai-sdlc.independent-review-proof/v1", "reviewID": r.ID, "verificationID": r.VerificationID, "candidateID": r.CandidateID, "behaviorSHA256": r.BehaviorDigest, "profileSHA256": r.ProfileDigest, "configurationSHA256": r.ConfigurationDigest, "assignmentSHA256": r.AssignmentDigest, "candidateTreeSHA256": tree, "modelEvidenceSHA256": authority.Digest(raw), "verdict": "approve", "sourceUnchanged": true, "publicationAuthorized": false}
	encode := func() []byte { v, _ := json.Marshal(proof); return v }
	if db.FinishReview(ctx, c, encode()) == nil {
		t.Fatal("accepted without export/cleanup")
	}
	effect, e := db.BeginReviewEffect(ctx, c, "export")
	if e != nil || db.AcknowledgeReviewEffect(ctx, effect, pod, tree) != nil {
		t.Fatal("export")
	}
	if db.FinishReview(ctx, c, encode()) == nil {
		t.Fatal("accepted without confirmed cleanup")
	}
	if db.ConfirmReviewCleanup(ctx, c) != nil {
		t.Fatal("cleanup")
	}
	for key, value := range map[string]any{"profileSHA256": strings.Repeat("f", 64), "behaviorSHA256": strings.Repeat("f", 64), "modelEvidenceSHA256": strings.Repeat("f", 64), "candidateTreeSHA256": strings.Repeat("f", 64), "verdict": "request_changes", "sourceUnchanged": false, "publicationAuthorized": true} {
		old := proof[key]
		proof[key] = value
		if db.FinishReview(ctx, c, encode()) == nil {
			t.Fatal("accepted invalid proof", key)
		}
		proof[key] = old
	}
	proof["summarySHA256"] = strings.Repeat("a", 64)
	if db.FinishReview(ctx, c, encode()) == nil {
		t.Fatal("accepted summary digest without durable receipt")
	}
	delete(proof, "summarySHA256")
	if f.db.FinishReview(ctx, c, encode()) == nil {
		t.Fatal("generator approved its review")
	}
	if db.FinishReview(ctx, c, encode()) != nil {
		t.Fatal("valid review proof rejected")
	}
	if db.FinishReview(ctx, c, encode()) == nil {
		t.Fatal("review proof replay")
	}
	if next, e := db.NextVerifiedForReview(ctx); e != nil || next != "" {
		t.Fatal("completed review requeued")
	}
}

func TestReviewSelectionSkipsSupersededIssueWithoutMutatingProof(t *testing.T) {
	f, db, r := reviewJournalFixture(t)
	ctx := context.Background()
	// Remove only this disposable fixture's prequeued review to exercise selection.
	if _, e := f.Admin.Pool.Exec(`DELETE FROM review_runs WHERE id=$1`, r.ID); e != nil {
		t.Fatal(e)
	}
	if next, e := db.NextVerifiedForReview(ctx); e != nil || next != r.VerificationID {
		t.Fatal("initial selection", next, e)
	}
	if _, e := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if next, e := db.NextVerifiedForReview(ctx); e != nil || next != "" {
		t.Fatal("obsolete issue blocked selection", next, e)
	}
	var state string
	if e := db.Pool.QueryRow(`SELECT state FROM verification_requests WHERE id=$1`, r.VerificationID).Scan(&state); e != nil || state != "passed" {
		t.Fatal("historical fact mutated", e)
	}
	if _, e := f.Admin.Pool.Exec(`REVOKE SELECT(repository,issue_number,generation,content_key) ON intake_heads FROM aisdlc_verifier`); e != nil {
		t.Fatal(e)
	}
	if _, e := db.NextVerifiedForReview(ctx); e == nil {
		t.Fatal("unavailable observation treated as permanent rejection")
	}
}

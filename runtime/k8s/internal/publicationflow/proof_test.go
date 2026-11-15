package publicationflow

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"strings"
	"testing"
	"time"
)

func proofFixture() (verification.Command, store.PassedReview, ReviewPolicy) {
	d := strings.Repeat("a", 64)
	v := verification.Command{Version: verification.CommandVersion, CandidateID: "execution-" + d, WorkItemID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Repository: "os-santiago/homedir", IssueNumber: 3, Generation: 1, IssueRevision: d, SourceSHA: strings.Repeat("a", 40), DeltaSHA256: d, CandidateTreeSHA256: d, Recipe: verification.LocaleRecipe, RecipeRevision: verification.RecipeRevision(), AuthorityRevision: d, ExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli()}
	b := verificationflow.BehaviorProof{Version: "ai-sdlc.behavior-proof/v1", Command: v, ResourceUIDs: map[string]string{"config": "one", "baseline": "two", "candidate": "three", "reference": "four", "tests": "five"}, BehaviorVerified: true}
	behavior, _ := json.Marshal(b)
	r := store.ReviewRun{VerificationID: v.ID(), CandidateID: v.CandidateID, WorkItem: v.WorkItemID, BehaviorDigest: authority.Digest(behavior), ProfileDigest: d, ConfigurationDigest: d, AssignmentDigest: d, ExpiresAt: time.UnixMilli(v.ExpiresAt)}
	r.ID = store.ReviewID(r.VerificationID, r.BehaviorDigest, r.ProfileDigest, r.ConfigurationDigest, r.AssignmentDigest)
	p := reviewProof{Version: "ai-sdlc.independent-review-proof/v1", ReviewID: r.ID, VerificationID: v.ID(), CandidateID: v.CandidateID, BehaviorSHA256: r.BehaviorDigest, ProfileSHA256: d, ConfigurationSHA256: d, AssignmentSHA256: d, CandidateTreeSHA256: d, ModelEvidenceSHA256: d, Verdict: "approve", SourceUnchanged: true}
	raw, _ := json.Marshal(p)
	return v, store.PassedReview{Run: r, Proof: raw, Behavior: behavior}, ReviewPolicy{d, d}
}
func TestPersistedPublicationProofBindings(t *testing.T) {
	v, r, policy := proofFixture()
	c, e := matchProofs(v, r, policy)
	if e != nil || c.ExpiresAt != v.ExpiresAt || c.IndependentReviewSHA256 != authority.Digest(r.Proof) {
		t.Fatal("valid proof denied")
	}
	cases := map[string]func(*verification.Command, *store.PassedReview, *ReviewPolicy){
		"different source": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			v.SourceSHA = strings.Repeat("b", 40)
		},
		"different candidate": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			r.Run.CandidateID = "execution-" + strings.Repeat("b", 64)
		},
		"different review": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			r.Run.ID = "review-" + strings.Repeat("b", 64)
		},
		"reviewer policy changed": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			p.Profile = strings.Repeat("b", 64)
		},
		"review configuration changed": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			p.Configuration = strings.Repeat("b", 64)
		},
		"expired": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			r.Run.ExpiresAt = time.Now().Add(-time.Second)
		},
		"extended expiry": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			r.Run.ExpiresAt = time.UnixMilli(v.ExpiresAt).Add(time.Second)
		},
		"invented proof": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			r.Proof = []byte(`{"verdict":"approve"}`)
		},
		"extra authority": func(v *verification.Command, r *store.PassedReview, p *ReviewPolicy) {
			r.Proof = append(append([]byte{}, r.Proof[:len(r.Proof)-1]...), []byte(`,"mergeAuthorized":true}`)...)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v, r, p := proofFixture()
			mutate(&v, &r, &p)
			if _, e := matchProofs(v, r, p); e == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	for _, field := range []string{"verdict", "sourceUnchanged", "publicationAuthorized", "modelEvidenceSHA256", "assignmentSHA256"} {
		t.Run(field, func(t *testing.T) {
			v, r, p := proofFixture()
			var raw map[string]any
			_ = json.Unmarshal(r.Proof, &raw)
			switch field {
			case "sourceUnchanged":
				raw[field] = false
			case "publicationAuthorized":
				raw[field] = true
			default:
				raw[field] = "invalid"
			}
			r.Proof, _ = json.Marshal(raw)
			if _, e := matchProofs(v, r, p); e == nil {
				t.Fatal("forged review accepted")
			}
		})
	}
}

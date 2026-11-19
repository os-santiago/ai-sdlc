// Package repair describes one derivation of a rejected candidate. References
// are immutable evidence bindings, never execution authority or model policy.
package repair

import (
	"regexp"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

var candidateID = regexp.MustCompile(`^execution-[a-f0-9]{64}$`)
var reviewID = regexp.MustCompile(`^review-[a-f0-9]{64}$`)
var verificationID = regexp.MustCompile(`^verification-[a-f0-9]{64}$`)

type Reference struct {
	ParentCandidateID  string `json:"parentCandidateID"`
	ParentReviewID     string `json:"parentReviewID"`
	VerificationID     string `json:"verificationID"`
	FeedbackSHA256     string `json:"feedbackSHA256"`
	BehaviorSHA256     string `json:"behaviorSHA256"`
	ParentTreeSHA256   string `json:"parentTreeSHA256"`
	OriginalTreeSHA256 string `json:"originalTreeSHA256"`
}

func (r Reference) Valid() bool {
	return candidateID.MatchString(r.ParentCandidateID) && reviewID.MatchString(r.ParentReviewID) &&
		verificationID.MatchString(r.VerificationID) && workspacebundle.ValidDigest(r.FeedbackSHA256) &&
		workspacebundle.ValidDigest(r.BehaviorSHA256) && workspacebundle.ValidDigest(r.ParentTreeSHA256) &&
		workspacebundle.ValidDigest(r.OriginalTreeSHA256) && r.ParentTreeSHA256 != r.OriginalTreeSHA256
}

// Assignment adds exact private feedback as untrusted data and a previously
// derived call ceiling. It cannot authorize another repair or change criteria.
type Assignment struct {
	Reference      Reference `json:"reference"`
	Feedback       string    `json:"feedback"`
	FinalSHA256    string    `json:"finalSHA256"`
	RemainingCalls int       `json:"remainingCalls"`
}

func (a Assignment) Valid() bool {
	verdict, err := reviewresult.Verdict(a.Feedback)
	return a.Reference.Valid() && utf8.ValidString(a.Feedback) && err == nil && verdict == "request_changes" &&
		a.FinalSHA256 == authority.Digest([]byte(a.Feedback)) && a.RemainingCalls > 0 && a.RemainingCalls <= 32
}

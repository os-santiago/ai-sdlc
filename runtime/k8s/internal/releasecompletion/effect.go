package releasecompletion

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

var commandID = regexp.MustCompile(`^complete-[a-f0-9]{64}$`)
var pinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9./:-]{0,447}@sha256:[a-f0-9]{64}$`)

func (p Plan) ValidateStructure(now time.Time) error {
	if p.Version != "ai-sdlc.terminal-release-plan/v1" || !commandID.MatchString(p.CommandID) || p.Repository != "os-santiago/saferus" || p.IssueNumber < 1 || p.IssueNumber > 2147483647 || p.Generation < 1 || !workspacebundle.ValidSourceSHA(p.SourceSHA) || !workspacebundle.ValidSourceSHA(p.TreeSHA) || !pinnedImage.MatchString(p.Image) || p.Tag != "ai-sdlc/"+p.CommandID || p.Title != "AI-SDLC kind test production: issue #"+strconv.Itoa(p.IssueNumber) || p.Body != p.BodyText() || p.ExpiresAt <= now.UnixMilli() || p.ExpiresAt > now.Add(MaxAge+5*time.Second).UnixMilli() {
		return ErrCompletion
	}
	for _, hash := range []string{p.IssueRevision, p.TargetSHA256, p.DeploymentRecordSHA256, p.ProvenanceRecordSHA256, p.BuildRecordSHA256} {
		if !workspacebundle.ValidDigest(hash) {
			return ErrCompletion
		}
	}
	return nil
}

type EffectRequest struct {
	Step       string          `json:"step"`
	PlanSHA256 string          `json:"planSHA256"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Body       json.RawMessage `json:"body"`
}

func (r EffectRequest) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }
func Request(p Plan, step string, releaseID int64, now time.Time) (EffectRequest, error) {
	if p.ValidateStructure(now) != nil {
		return EffectRequest{}, ErrCompletion
	}
	r := EffectRequest{Step: step, PlanSHA256: p.Digest()}
	base := "/repos/" + p.Repository
	var body any
	switch step {
	case "tag":
		if releaseID != 0 {
			return EffectRequest{}, ErrCompletion
		}
		r.Method, r.Path = http.MethodPost, base+"/git/refs"
		body = struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}{"refs/tags/" + p.Tag, p.SourceSHA}
	case "draft":
		if releaseID != 0 {
			return EffectRequest{}, ErrCompletion
		}
		r.Method, r.Path = http.MethodPost, base+"/releases"
		body = struct {
			Tag        string `json:"tag_name"`
			Target     string `json:"target_commitish"`
			Name       string `json:"name"`
			Body       string `json:"body"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
			Latest     string `json:"make_latest"`
		}{p.Tag, p.SourceSHA, p.Title, p.Body, true, false, "false"}
	case "publish":
		if releaseID <= 0 {
			return EffectRequest{}, ErrCompletion
		}
		r.Method, r.Path = http.MethodPatch, base+"/releases/"+strconv.FormatInt(releaseID, 10)
		body = struct {
			Draft  bool   `json:"draft"`
			Latest string `json:"make_latest"`
		}{false, "false"}
	case "close":
		if releaseID <= 0 {
			return EffectRequest{}, ErrCompletion
		}
		r.Method, r.Path = http.MethodPatch, base+"/issues/"+strconv.Itoa(p.IssueNumber)
		body = struct {
			State  string `json:"state"`
			Reason string `json:"state_reason"`
		}{"closed", "completed"}
	default:
		return EffectRequest{}, ErrCompletion
	}
	r.Body, _ = json.Marshal(body)
	return r, nil
}

type EffectReceipt struct {
	Step          string `json:"step"`
	PlanSHA256    string `json:"planSHA256"`
	RequestSHA256 string `json:"requestSHA256"`
	Tag           string `json:"tag"`
	CommitSHA     string `json:"commitSHA"`
	ReleaseID     int64  `json:"releaseID"`
	ReleaseURL    string `json:"releaseURL"`
	BodySHA256    string `json:"bodySHA256"`
	IssueNumber   int    `json:"issueNumber"`
	IssueID       int64  `json:"issueID"`
	ObservedAt    int64  `json:"observedAt"`
}

// ValidateReceipt binds an acknowledged effect to the original durable request.
// A struct alone is not evidence: the journal authenticates the one-use send and
// the writer validates GitHub responses and the exact tag reference.
func ValidateReceipt(p Plan, request EffectRequest, releaseID int64, r EffectReceipt, sent, recorded time.Time) error {
	expected, e := Request(p, request.Step, releaseID, sent)
	if e != nil || expected.Digest() != request.Digest() || r.Step != request.Step || r.PlanSHA256 != p.Digest() || r.RequestSHA256 != request.Digest() || r.Tag != p.Tag || r.CommitSHA != p.SourceSHA || r.ObservedAt < sent.UnixMilli() || r.ObservedAt > recorded.Add(5*time.Second).UnixMilli() || r.ObservedAt >= p.ExpiresAt || recorded.Before(sent) {
		return ErrCompletion
	}
	switch r.Step {
	case "tag":
		if r.ReleaseID != 0 || r.ReleaseURL != "" || r.BodySHA256 != "" || r.IssueNumber != 0 || r.IssueID != 0 {
			return ErrCompletion
		}
	case "draft":
		if r.ReleaseID <= 0 || r.ReleaseURL != "" || r.BodySHA256 != authority.Digest([]byte(p.Body)) || r.IssueNumber != 0 || r.IssueID != 0 {
			return ErrCompletion
		}
	case "publish", "close":
		if r.ReleaseID != releaseID || r.BodySHA256 != authority.Digest([]byte(p.Body)) || !ValidReleaseURL(r.ReleaseURL, p) {
			return ErrCompletion
		}
		if r.Step == "publish" && (r.IssueID != 0 || r.IssueNumber != 0) || r.Step == "close" && (r.IssueID <= 0 || r.IssueNumber != p.IssueNumber) {
			return ErrCompletion
		}
	default:
		return ErrCompletion
	}
	return nil
}
func ValidReleaseURL(value string, p Plan) bool {
	u, e := url.Parse(value)
	return e == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == "/"+p.Repository+"/releases/tag/"+p.Tag
}

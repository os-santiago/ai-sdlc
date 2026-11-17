// Package browsercheck defines the closed UI recipe's data contracts. Values
// are observations, not effect authority; the deployment journal authenticates
// the original claim, current configuration, one-use send and workload identity.
package browsercheck

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

const RequestVersion = "ai-sdlc.saferus-browser-request/v1"
const ResultVersion = "ai-sdlc.saferus-browser-result/v1"
const ProfileVersion = "ai-sdlc.saferus-browser-profile/v1"
const Namespace = "ai-sdlc-browser"
const RuntimeClass = "ai-sdlc-browser"
const MaxDuration = 30 * time.Second
const CleanupBudget = 10 * time.Second

var ErrBrowser = errors.New("original browser verification rejected")
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var uid = regexp.MustCompile(`^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
var nodeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
var containerID = regexp.MustCompile(`^containerd://[a-f0-9]{64}$`)

// Profile is installed platform input. Its full digest is transitively covered
// by the signed deployment configuration, not selected by an issue or model.
type Profile struct {
	NodeName                   string `json:"nodeName"`
	NodeUID                    string `json:"nodeUID"`
	Version                    string `json:"version"`
	ClusterUID                 string `json:"clusterUID"`
	NamespaceUID               string `json:"namespaceUID"`
	RuntimeClassUID            string `json:"runtimeClassUID"`
	CAConfigMapUID             string `json:"caConfigMapUID"`
	DenyPolicyUID              string `json:"denyPolicyUID"`
	EgressPolicyUID            string `json:"egressPolicyUID"`
	ConsoleIngressPolicyUID    string `json:"consoleIngressPolicyUID"`
	Image                      string `json:"image"`
	RuntimeDigest              string `json:"runtimeDigest"`
	CASHA256                   string `json:"caSHA256"`
	RuntimeConfigurationSHA256 string `json:"runtimeConfigurationSHA256"`
	BaselineExpectation        string `json:"baselineExpectation"`
	CandidateExpectation       string `json:"candidateExpectation"`
}

func hash(value any) string      { raw, _ := json.Marshal(value); return authority.Digest(raw) }
func (p Profile) Digest() string { return hash(p) }
func (p Profile) Validate() error {
	if !nodeName.MatchString(p.NodeName) || p.Version != ProfileVersion || !strings.HasPrefix(p.Image, "quay.io/os-santiago/ai-sdlc@sha256:") || !digest.MatchString(strings.TrimPrefix(p.Image, "quay.io/os-santiago/ai-sdlc@sha256:")) || !imageDigest(p.RuntimeDigest) || !digest.MatchString(p.CASHA256) || !digest.MatchString(p.RuntimeConfigurationSHA256) || !expectation(p.BaselineExpectation) || !expectation(p.CandidateExpectation) {
		return ErrBrowser
	}
	seen := map[string]bool{}
	for _, value := range []string{p.NodeUID, p.ClusterUID, p.NamespaceUID, p.RuntimeClassUID, p.CAConfigMapUID, p.DenyPolicyUID, p.EgressPolicyUID, p.ConsoleIngressPolicyUID} {
		if !uid.MatchString(value) || seen[value] {
			return ErrBrowser
		}
		seen[value] = true
	}
	return nil
}
func expectation(value string) bool {
	return value == "baseline-seconds" || value == "readable-duration"
}
func imageDigest(s string) bool {
	return strings.HasPrefix(s, "sha256:") && digest.MatchString(strings.TrimPrefix(s, "sha256:"))
}

// Request's wire schema is shared with the fixed Python runner. The journal
// derives RequestID from original deployment/claim/phase; there is no URL field.
type Request struct {
	Version        string `json:"version"`
	RequestID      string `json:"requestID"`
	TargetSHA256   string `json:"targetSHA256"`
	SnapshotSHA256 string `json:"snapshotSHA256"`
	ImageDigest    string `json:"imageDigest"`
	IssuedAt       int64  `json:"issuedAt"`
	ExpiresAt      int64  `json:"expiresAt"`
	Expectation    string `json:"expectation"`
}

func (r Request) Digest() string { return hash(r) }
func (r Request) Validate(now time.Time) error {
	if r.Version != RequestVersion || !strings.HasPrefix(r.RequestID, "browser-") || !digest.MatchString(strings.TrimPrefix(r.RequestID, "browser-")) || !digest.MatchString(r.TargetSHA256) || !digest.MatchString(r.SnapshotSHA256) || !imageDigest(r.ImageDigest) || (r.Expectation != "baseline-seconds" && r.Expectation != "readable-duration") || r.IssuedAt <= 0 || r.IssuedAt > now.Add(5*time.Second).UnixMilli() || r.ExpiresAt <= now.UnixMilli() || r.ExpiresAt <= r.IssuedAt || r.ExpiresAt-r.IssuedAt > MaxDuration.Milliseconds() {
		return ErrBrowser
	}
	return nil
}

type Observation struct {
	Viewport       string `json:"viewport"`
	ResponseSHA256 string `json:"responseSHA256"`
}
type Result struct {
	Version               string        `json:"version"`
	Status                string        `json:"status"`
	RequestID             string        `json:"requestID"`
	RequestSHA256         string        `json:"requestSHA256"`
	TargetSHA256          string        `json:"targetSHA256"`
	SnapshotSHA256        string        `json:"snapshotSHA256"`
	ImageDigest           string        `json:"imageDigest"`
	Expectation           string        `json:"expectation"`
	StartedAt             int64         `json:"startedAt"`
	FinishedAt            int64         `json:"finishedAt"`
	Observations          []Observation `json:"observations"`
	NormalTLS             bool          `json:"normalTLS"`
	NetworkScopePreserved bool          `json:"networkScopePreserved"`
}

func ParseResult(raw []byte, request Request, now time.Time) (Result, error) {
	var r Result
	if len(raw) == 0 || len(raw) > 4096 || authority.DecodeStrict(raw, &r) != nil {
		return r, ErrBrowser
	}
	if r.Validate(request, now) != nil {
		return Result{}, ErrBrowser
	}
	return r, nil
}
func (r Result) Validate(request Request, now time.Time) error {
	if request.Validate(time.UnixMilli(request.IssuedAt)) != nil || r.Version != ResultVersion || r.Status != "passed" || r.RequestID != request.RequestID || r.RequestSHA256 != request.Digest() || r.TargetSHA256 != request.TargetSHA256 || r.SnapshotSHA256 != request.SnapshotSHA256 || r.ImageDigest != request.ImageDigest || r.Expectation != request.Expectation || !r.NormalTLS || !r.NetworkScopePreserved || r.StartedAt < request.IssuedAt || r.FinishedAt < r.StartedAt || r.FinishedAt >= request.ExpiresAt || r.FinishedAt > now.UnixMilli() || now.UnixMilli() >= request.ExpiresAt || len(r.Observations) != 2 || r.Observations[0].Viewport != "desktop" || r.Observations[1].Viewport != "mobile" {
		return ErrBrowser
	}
	for _, o := range r.Observations {
		if !digest.MatchString(o.ResponseSHA256) {
			return ErrBrowser
		}
	}
	return nil
}

// Evidence is accepted only after original-Pod deletion and durable journal
// recording. Structural validation cannot substitute for those observations.
type Evidence struct {
	ProfileSHA256 string  `json:"profileSHA256"`
	Request       Request `json:"request"`
	Result        Result  `json:"result"`
	PodUID        string  `json:"podUID"`
	ContainerID   string  `json:"containerID"`
	RuntimeDigest string  `json:"runtimeDigest"`
	DeletedAt     int64   `json:"deletedAt"`
}

func (e Evidence) Digest() string { return hash(e) }
func (e Evidence) Validate(p Profile, now time.Time) error {
	if p.Validate() != nil || e.ProfileSHA256 != p.Digest() || !uid.MatchString(e.PodUID) || !containerID.MatchString(e.ContainerID) || e.RuntimeDigest != p.RuntimeDigest || e.DeletedAt < e.Result.FinishedAt || e.DeletedAt > now.UnixMilli() || e.DeletedAt >= e.Request.ExpiresAt || e.Result.Validate(e.Request, now) != nil {
		return ErrBrowser
	}
	return nil
}

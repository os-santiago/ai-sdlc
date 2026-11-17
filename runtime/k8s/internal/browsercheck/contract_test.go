package browsercheck

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixture() (Profile, Request, Result, time.Time) {
	now := time.UnixMilli(100000)
	p := Profile{NodeName: "fixture-node", NodeUID: "99999999-9999-9999-9999-999999999999", Version: ProfileVersion, BaselineExpectation: "baseline-seconds", CandidateExpectation: "readable-duration", DenyPolicyUID: "66666666-6666-6666-6666-666666666666", EgressPolicyUID: "77777777-7777-7777-7777-777777777777", ConsoleIngressPolicyUID: "88888888-8888-8888-8888-888888888888", ClusterUID: "11111111-1111-1111-1111-111111111111", NamespaceUID: "22222222-2222-2222-2222-222222222222", RuntimeClassUID: "33333333-3333-3333-3333-333333333333", CAConfigMapUID: "44444444-4444-4444-4444-444444444444", Image: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64), RuntimeDigest: "sha256:" + strings.Repeat("b", 64), CASHA256: strings.Repeat("c", 64), RuntimeConfigurationSHA256: strings.Repeat("d", 64)}
	r := Request{Version: RequestVersion, RequestID: "browser-" + strings.Repeat("a", 64), TargetSHA256: strings.Repeat("b", 64), SnapshotSHA256: strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("d", 64), IssuedAt: 99000, ExpiresAt: 129000, Expectation: "readable-duration"}
	out := Result{Version: ResultVersion, Status: "passed", RequestID: r.RequestID, RequestSHA256: r.Digest(), TargetSHA256: r.TargetSHA256, SnapshotSHA256: r.SnapshotSHA256, ImageDigest: r.ImageDigest, Expectation: r.Expectation, StartedAt: 99000, FinishedAt: 99500, Observations: []Observation{{"desktop", strings.Repeat("e", 64)}, {"mobile", strings.Repeat("f", 64)}}, NormalTLS: true, NetworkScopePreserved: true}
	return p, r, out, now
}
func TestResultCannotSubstituteBindingOrExtendOriginalWindow(t *testing.T) {
	_, request, result, now := fixture()
	raw, _ := json.Marshal(result)
	if _, e := ParseResult(raw, request, now); e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(*Result){
		"foreign request":          func(r *Result) { r.RequestID = "browser-" + strings.Repeat("b", 64) },
		"foreign target":           func(r *Result) { r.TargetSHA256 = strings.Repeat("a", 64) },
		"foreign snapshot":         func(r *Result) { r.SnapshotSHA256 = strings.Repeat("a", 64) },
		"foreign runtime":          func(r *Result) { r.ImageDigest = "sha256:" + strings.Repeat("a", 64) },
		"old baseline expectation": func(r *Result) { r.Expectation = "baseline-seconds" },
		"insecure TLS":             func(r *Result) { r.NormalTLS = false },
		"escaped network":          func(r *Result) { r.NetworkScopePreserved = false },
		"one viewport":             func(r *Result) { r.Observations = r.Observations[:1] },
		"late finish":              func(r *Result) { r.FinishedAt = request.ExpiresAt },
		"future observation":       func(r *Result) { r.FinishedAt = now.UnixMilli() + 1 },
		"replayed observation":     func(r *Result) { r.StartedAt = request.IssuedAt - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := result
			mutate(&r)
			raw, _ := json.Marshal(r)
			if _, e := ParseResult(raw, request, now); e == nil {
				t.Fatal("accepted invalid result")
			}
		})
	}
	if _, e := ParseResult(raw, request, time.UnixMilli(request.ExpiresAt)); e == nil {
		t.Fatal("accepted result after original deadline")
	}
	for _, extra := range []string{`,"url":"https://example.com"}`, `,"normalTLS":true}`} {
		invalid := append(append([]byte(nil), raw[:len(raw)-1]...), extra...)
		if _, e := ParseResult(invalid, request, now); e == nil {
			t.Fatal("accepted extra or duplicate field")
		}
	}
	if _, e := ParseResult(bytes.Repeat([]byte(" "), 4097), request, now); e == nil {
		t.Fatal("accepted oversized output")
	}
}
func TestEvidenceRequiresOriginalProfileRuntimeAndCompletedCleanup(t *testing.T) {
	p, r, result, now := fixture()
	evidence := Evidence{ProfileSHA256: p.Digest(), Request: r, Result: result, PodUID: "55555555-5555-5555-5555-555555555555", ContainerID: "containerd://" + strings.Repeat("e", 64), RuntimeDigest: p.RuntimeDigest, DeletedAt: 99900}
	if evidence.Validate(p, now) != nil {
		t.Fatal("valid observed fixture rejected")
	}
	for name, mutate := range map[string]func(*Evidence){
		"replacement pod":     func(e *Evidence) { e.PodUID = "" },
		"foreign profile":     func(e *Evidence) { e.ProfileSHA256 = strings.Repeat("f", 64) },
		"foreign image":       func(e *Evidence) { e.RuntimeDigest = "sha256:" + strings.Repeat("c", 64) },
		"no original process": func(e *Evidence) { e.ContainerID = "unknown" },
		"cleanup pending":     func(e *Evidence) { e.DeletedAt = 0 },
		"late cleanup":        func(e *Evidence) { e.DeletedAt = r.ExpiresAt },
	} {
		t.Run(name, func(t *testing.T) {
			e := evidence
			mutate(&e)
			if e.Validate(p, now) == nil {
				t.Fatal("accepted invalid evidence")
			}
		})
	}
	changed := p
	changed.Image = "localhost/browser:latest"
	if changed.Validate() == nil {
		t.Fatal("accepted unpinned image")
	}
	changed = p
	changed.NamespaceUID = changed.ClusterUID
	if changed.Validate() == nil {
		t.Fatal("accepted duplicate scope UID")
	}
}

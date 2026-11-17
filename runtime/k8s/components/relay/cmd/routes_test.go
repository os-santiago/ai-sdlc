package main

import (
	"encoding/json"
	"testing"
)

func TestProvenanceRelayRequiresExactlyEnabledRoutes(t *testing.T) {
	for k, v := range map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "true", "AI_SDLC_PUBLICATION": "true", "AI_SDLC_MERGE": "true", "AI_SDLC_BUILD": "true"} {
		t.Setenv(k, v)
	}
	routes := map[string]string{}
	for _, op := range []string{"admit-issue", "plan-admission", "execute-work", "verify-artifact", "publish-candidate", "build-merged"} {
		routes[op] = "http://component/internal/" + op
	}
	encode := func() string { raw, _ := json.Marshal(routes); return string(raw) }
	for _, flag := range []string{"", "false", "true", "invalid"} {
		t.Setenv("AI_SDLC_PROVENANCE", flag)
		delete(routes, "verify-built")
		_, e := reviewedRoutes(encode())
		if (e == nil) != (flag == "" || flag == "false") {
			t.Fatal("six route ceiling", flag, e)
		}
		routes["verify-built"] = "http://verifier/internal/verify-built"
		_, e = reviewedRoutes(encode())
		if (e == nil) != (flag == "true") {
			t.Fatal("seven route ceiling", flag, e)
		}
	}
	t.Setenv("AI_SDLC_PROVENANCE", "true")
	for _, key := range []string{"AI_SDLC_BUILD", "AI_SDLC_MERGE", "AI_SDLC_PUBLICATION", "AI_SDLC_INDEPENDENT_REVIEW"} {
		t.Setenv(key, "false")
		if _, e := reviewedRoutes(encode()); e == nil {
			t.Fatal("missing predecessor accepted", key)
		}
		t.Setenv(key, "true")
	}
	routes["promote-artifact"] = "http://release-manager/internal/promote-artifact"
	if _, e := reviewedRoutes(encode()); e == nil {
		t.Fatal("disabled downstream route accepted")
	}
}

func TestReleaseRelayRequiresCompleteNineRouteCatalog(t *testing.T) {
	for k, v := range map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "true", "AI_SDLC_PUBLICATION": "true", "AI_SDLC_MERGE": "true", "AI_SDLC_BUILD": "true", "AI_SDLC_PROVENANCE": "true", "AI_SDLC_RELEASE": "true"} {
		t.Setenv(k, v)
	}
	routes := map[string]string{}
	for _, op := range []string{"admit-issue", "plan-admission", "execute-work", "verify-artifact", "publish-candidate", "build-merged", "verify-built", "promote-artifact", "deploy-release"} {
		routes[op] = "http://component/internal/" + op
	}
	encode := func() string { raw, _ := json.Marshal(routes); return string(raw) }
	if actual, e := reviewedRoutes(encode()); e != nil || len(actual) != 9 {
		t.Fatal("complete release routes denied", e)
	}
	for op, value := range routes {
		delete(routes, op)
		if _, e := reviewedRoutes(encode()); e == nil {
			t.Fatal("missing route accepted", op)
		}
		routes[op] = value
	}
	routes["record-deployment"] = "http://component/internal/record-deployment"
	if _, e := reviewedRoutes(encode()); e == nil {
		t.Fatal("unimplemented completion route accepted")
	}
	delete(routes, "record-deployment")
	for _, flag := range []string{"", "false", "invalid"} {
		t.Setenv("AI_SDLC_RELEASE", flag)
		if _, e := reviewedRoutes(encode()); e == nil {
			t.Fatal("disabled release routes accepted", flag)
		}
	}
}

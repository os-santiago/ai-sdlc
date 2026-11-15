package podtransport

import (
	"context"
	"net/http"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVerificationRequiresExclusiveDenyPolicy(t *testing.T) {
	for _, kind := range []string{"deny", "extra-policy", "egress", "selector", "pagination", "missing-direction", "deleting", "missing"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := resourcesFixture(t, func(_ *Resources, _ core.Pod, w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" || req.URL.Path != "/apis/networking.k8s.io/v1/namespaces/ai-sdlc-verifiers/networkpolicies" || req.URL.Query().Get("limit") != "16" {
					t.Fatal("unbounded policy lookup")
				}
				p := networking.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: "deny-all", Namespace: "ai-sdlc-verifiers"}, Spec: networking.NetworkPolicySpec{PolicyTypes: []networking.PolicyType{networking.PolicyTypeIngress, networking.PolicyTypeEgress}}}
				list := networking.NetworkPolicyList{Items: []networking.NetworkPolicy{p}}
				switch kind {
				case "extra-policy":
					list.Items = append(list.Items, p)
				case "egress":
					list.Items[0].Spec.Egress = []networking.NetworkPolicyEgressRule{{}}
				case "selector":
					list.Items[0].Spec.PodSelector.MatchLabels = map[string]string{"app": "other"}
				case "pagination":
					list.Continue = "more"
				case "missing-direction":
					list.Items[0].Spec.PolicyTypes = []networking.PolicyType{networking.PolicyTypeIngress, networking.PolicyTypeIngress}
				case "deleting":
					now := meta.Now()
					list.Items[0].DeletionTimestamp = &now
				case "missing":
					list.Items = nil
				}
				writeResource(w, 200, list)
			})
			v := &VerificationResources{resources: map[string]*Resources{"reference": r}}
			if got := v.isolation(context.Background()); (got == nil) != (kind == "deny") {
				t.Fatal("incorrect isolation decision", kind)
			}
		})
	}
}

func TestVerificationLogReadIsBoundedAndDoesNotRetry(t *testing.T) {
	for _, kind := range []string{"valid", "overflow", "empty", "forbidden", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			r, calls := resourcesFixture(t, func(r *Resources, _ core.Pod, w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" || req.URL.Path != "/api/v1/namespaces/"+r.binding.Namespace+"/pods/"+r.pod.Name+"/log" || req.Header.Get("Accept") != "*/*" || req.URL.Query().Get("container") != "candidate" || req.URL.Query().Get("limitBytes") != "32769" {
					t.Fatal("log boundary changed")
				}
				switch kind {
				case "forbidden":
					w.WriteHeader(403)
					return
				case "redirect":
					w.Header().Set("Location", "/outside")
					w.WriteHeader(307)
					return
				case "empty":
					return
				case "overflow":
					_, _ = w.Write([]byte(strings.Repeat("x", 32769)))
					return
				}
				_, _ = w.Write([]byte(`["bounded output"]`))
			})
			v := &VerificationResources{resources: map[string]*Resources{"reference": r, "candidate": r}}
			_, err := v.log(context.Background(), "candidate")
			if (err == nil) != (kind == "valid") || calls.Load() != 1 {
				t.Fatal("log read accepted unsafe response or retried")
			}
			if _, err = v.log(context.Background(), "tests"); err == nil || calls.Load() != 1 {
				t.Fatal("arbitrary role log request")
			}
		})
	}
}

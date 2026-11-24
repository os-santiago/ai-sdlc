package podtransport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	core "k8s.io/api/core/v1"
	node "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

func runtimeClassServer(t *testing.T, handler http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) || !strings.HasPrefix(r.URL.Path, "/apis/node.k8s.io/v1/runtimeclasses/") || r.URL.RawQuery != "" {
			t.Error("unexpected runtime class request", r.Method, r.URL.String())
			w.WriteHeader(400)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

func TestCheckRuntimeClassAcceptsAnyPlatformSandboxClass(t *testing.T) {
	for _, name := range []string{"ai-sdlc-offline", "gvisor", "kata", "kata-mshv-vm-isolation"} {
		client, calls := runtimeClassServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/apis/node.k8s.io/v1/runtimeclasses/"+name {
				w.WriteHeader(404)
				return
			}
			w.Write([]byte(`{"kind":"RuntimeClass","apiVersion":"node.k8s.io/v1","metadata":{"name":"` + name + `","uid":"u","resourceVersion":"1","managedFields":[]},"handler":"runsc","scheduling":{"nodeSelector":{"sandbox":"true"}}}`))
		})
		admission, err := client.CheckRuntimeClass(context.Background(), name)
		if err != nil {
			t.Fatal(name, err)
		}
		// The class's scheduling is returned for the exact pod comparison.
		if len(admission.NodeSelector) != 1 || admission.NodeSelector["sandbox"] != "true" || admission.Overhead != nil || admission.Tolerations != nil {
			t.Fatal("admission additions not observed", admission)
		}
		if calls.Load() != 1 {
			t.Fatal("preflight is not one read")
		}
	}
}

func TestCheckRuntimeClassFailsClosed(t *testing.T) {
	cases := map[string]func(http.ResponseWriter, *http.Request){
		"missing":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) },
		"forbidden": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) },
		"server":    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		"other name": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"runc"},"handler":"runc"}`))
		},
		"no handler": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata"}}`))
		},
		"wrong kind": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"Status","metadata":{"name":"kata"},"handler":"kata"}`))
		},
		"terminating": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata","deletionTimestamp":"2026-01-01T00:00:00Z"},"handler":"kata"}`))
		},
		"malformed": func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"kind":`)) },
		"oversized": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata","annotations":{"a":"` + strings.Repeat("x", maxRuntimeClassBytes) + `"}},"handler":"kata"}`))
		},
	}
	for label, handler := range cases {
		client, _ := runtimeClassServer(t, handler)
		if _, err := client.CheckRuntimeClass(context.Background(), "kata"); !errors.Is(err, ErrRuntimeClassUnavailable) {
			t.Fatal(label, "accepted")
		}
	}
	// Invalid names never reach the API server.
	client, calls := runtimeClassServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for _, name := range []string{"", "../namespaces", "kata?x=1", "Kata"} {
		if _, err := client.CheckRuntimeClass(context.Background(), name); !errors.Is(err, ErrRuntimeClassUnavailable) {
			t.Fatal("invalid name accepted", name)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid name was sent")
	}
	var nilClient *Client
	if _, err := nilClient.CheckRuntimeClass(context.Background(), "kata"); err == nil {
		t.Fatal("nil client accepted")
	}
}

func kataClass(overhead core.ResourceList, scheduling *node.Scheduling) node.RuntimeClass {
	class := node.RuntimeClass{Handler: "kata", Scheduling: scheduling}
	class.Kind, class.Name = "RuntimeClass", "kata"
	if overhead != nil {
		class.Overhead = &node.Overhead{PodFixed: overhead}
	}
	return class
}

func TestRuntimeClassOverheadMustFitDeclaredQuotaHeadroom(t *testing.T) {
	overhead := core.ResourceList{core.ResourceCPU: resource.MustParse("250m"), core.ResourceMemory: resource.MustParse("350Mi")}
	headroom := parseDeclaredOverhead("250m", "400Mi")
	a, err := admissionOf(kataClass(overhead, nil), headroom)
	if err != nil || !equality.Semantic.DeepEqual(a.Overhead, overhead) {
		t.Fatal("fitting overhead refused", err)
	}
	for label, c := range map[string]struct {
		overhead core.ResourceList
		headroom core.ResourceList
	}{
		"no headroom":    {overhead, parseDeclaredOverhead("", "")},
		"cpu too large":  {overhead, parseDeclaredOverhead("200m", "400Mi")},
		"memory missing": {overhead, parseDeclaredOverhead("250m", "")},
		"malformed":      {overhead, parseDeclaredOverhead("lots", "400Mi")},
		"other resource": {core.ResourceList{core.ResourceEphemeralStorage: resource.MustParse("1Mi")}, headroom},
	} {
		if _, err := admissionOf(kataClass(c.overhead, nil), c.headroom); !errors.Is(err, ErrRuntimeClassOverhead) {
			t.Fatal(label, "accepted", err)
		}
	}
	// A class without overhead needs no headroom.
	if _, err := admissionOf(kataClass(nil, nil), core.ResourceList{}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeClassSchedulingMustBeAnExactUnion(t *testing.T) {
	gke := &node.Scheduling{NodeSelector: map[string]string{"sandbox.gke.io/runtime": "gvisor"},
		Tolerations: []core.Toleration{{Key: "sandbox.gke.io/runtime", Operator: core.TolerationOpEqual, Value: "gvisor", Effect: core.TaintEffectNoSchedule}}}
	if _, err := admissionOf(kataClass(nil, gke), nil); err != nil {
		t.Fatal(err)
	}
	seconds := int64(10)
	for label, tolerations := range map[string][]core.Toleration{
		"subsumes everything": {{Operator: core.TolerationOpExists}},
		"subsumes a default":  {{Key: "node.kubernetes.io/not-ready", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute}},
		"subsumed by default": {{Key: "node.kubernetes.io/unreachable", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute, TolerationSeconds: &seconds}},
		"duplicate":           {gke.Tolerations[0], gke.Tolerations[0]},
	} {
		if _, err := admissionOf(kataClass(nil, &node.Scheduling{Tolerations: tolerations}), nil); !errors.Is(err, ErrRuntimeClassScheduling) {
			t.Fatal(label, "accepted", err)
		}
	}
}

// admittedPod returns what the API server would store for a compiled pod in a
// class: the exact defaults plus the RuntimeClass admission additions.
func admittedPod(compiled core.Pod, a RuntimeClassAdmission, uid string) core.Pod {
	pod := *compiled.DeepCopy()
	pod.UID = types.UID(uid)
	applyDefaults(&pod.Spec)
	pod.Spec.Overhead = a.Overhead
	if len(a.NodeSelector) > 0 {
		pod.Spec.NodeSelector = a.NodeSelector
	}
	// Admission appends the class's tolerations after the defaults.
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, a.Tolerations...)
	pod.Spec.NodeName = "node-a"
	return pod
}

func TestPodComparisonAcceptsExactlyTheClassAdditions(t *testing.T) {
	b, _, _ := bindingFixture(t)
	w, err := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.RuntimeClass, b.Assignment, b.Profile, b.ADev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(w.Pod)
	var template core.Pod
	if json.Unmarshal(raw, &template) != nil {
		t.Fatal("template")
	}
	overhead := core.ResourceList{core.ResourceCPU: resource.MustParse("250m"), core.ResourceMemory: resource.MustParse("350Mi")}
	class := kataClass(overhead, &node.Scheduling{NodeSelector: map[string]string{"sandbox.gke.io/runtime": "gvisor"},
		Tolerations: []core.Toleration{{Key: "sandbox.gke.io/runtime", Operator: core.TolerationOpEqual, Value: "gvisor", Effect: core.TaintEffectNoSchedule}}})
	a, err := admissionOf(class, parseDeclaredOverhead("250m", "350Mi"))
	if err != nil {
		t.Fatal(err)
	}
	uid := "11111111-1111-4111-8111-111111111111"
	admitted := admittedPod(template, a, uid)
	if !exactPodTemplate(admitted, template, uid, a) {
		t.Fatal("pod with exactly the class additions rejected")
	}
	// Admission may order the class tolerations first; order has no meaning.
	reordered := *admitted.DeepCopy()
	tl := reordered.Spec.Tolerations
	reordered.Spec.Tolerations = append([]core.Toleration{tl[len(tl)-1]}, tl[:len(tl)-1]...)
	if !exactPodTemplate(reordered, template, uid, a) {
		t.Fatal("toleration order changed the decision")
	}
	// Without the observed admission, the same pod fails closed.
	if exactPodTemplate(admitted, template, uid, RuntimeClassAdmission{}) {
		t.Fatal("class additions accepted without the preflight observation")
	}
	for label, mutate := range map[string]func(*core.Pod){
		"different overhead": func(p *core.Pod) {
			p.Spec.Overhead = core.ResourceList{core.ResourceCPU: resource.MustParse("300m"), core.ResourceMemory: resource.MustParse("350Mi")}
		},
		"missing overhead": func(p *core.Pod) { p.Spec.Overhead = nil },
		"extra selector": func(p *core.Pod) {
			p.Spec.NodeSelector = map[string]string{"sandbox.gke.io/runtime": "gvisor", "zone": "a"}
		},
		"extra toleration": func(p *core.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, core.Toleration{Key: "other", Operator: core.TolerationOpExists})
		},
		"other added field": func(p *core.Pod) { p.Spec.PriorityClassName = "system-node-critical" },
		"host network":      func(p *core.Pod) { p.Spec.HostNetwork = true },
	} {
		changed := *admitted.DeepCopy()
		mutate(&changed)
		if exactPodTemplate(changed, template, uid, a) {
			t.Fatal(label, "accepted")
		}
	}
	// A compiled selector conflicting with the class cannot be expected.
	conflicting := *template.DeepCopy()
	conflicting.Spec.NodeSelector = map[string]string{"sandbox.gke.io/runtime": "kata"}
	if exactPodTemplate(admittedPod(conflicting, a, uid), conflicting, uid, a) {
		t.Fatal("conflicting selector accepted")
	}
}

func TestSessionBindAcceptsExactlyTheObservedClassAdditions(t *testing.T) {
	overhead := core.ResourceList{core.ResourceCPU: resource.MustParse("250m"), core.ResourceMemory: resource.MustParse("350Mi")}
	a, err := admissionOf(kataClass(overhead, &node.Scheduling{NodeSelector: map[string]string{"katacontainers.io/kata-runtime": "true"},
		Tolerations: []core.Toleration{{Key: "kata", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoSchedule}}}), parseDeclaredOverhead("1", "1Gi"))
	if err != nil {
		t.Fatal(err)
	}
	for label, c := range map[string]struct {
		admission RuntimeClassAdmission
		mutate    func(*core.Pod)
		accepted  bool
	}{
		"exact":             {a, func(*core.Pod) {}, true},
		"not observed":      {RuntimeClassAdmission{}, func(*core.Pod) {}, false},
		"overhead mismatch": {a, func(p *core.Pod) { p.Spec.Overhead[core.ResourceMemory] = resource.MustParse("1Gi") }, false},
		"extra field":       {a, func(p *core.Pod) { p.Spec.RuntimeClassName = nil }, false},
	} {
		t.Run(label, func(t *testing.T) {
			b, pod, _ := bindingFixture(t)
			b.Admission = c.admission
			pod.Spec.Overhead = core.ResourceList{}
			for k, v := range a.Overhead {
				pod.Spec.Overhead[k] = v.DeepCopy()
			}
			pod.Spec.NodeSelector = a.NodeSelector
			pod.Spec.Tolerations = append(pod.Spec.Tolerations, a.Tolerations...)
			c.mutate(&pod)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(pod)
			}))
			defer server.Close()
			client, err := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = Bind(context.Background(), client, b)
			if (err == nil) != c.accepted {
				t.Fatal("decision", err)
			}
		})
	}
}

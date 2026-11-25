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

// On OpenShift a class may itself declare the memory-pressure toleration that
// OpenShift admission otherwise adds. The normalization must keep the declared
// copy, and still remove the undeclared OpenShift addition for other classes.
func TestOpenShiftKeepsAClassDeclaredMemoryPressureToleration(t *testing.T) {
	withPlatform(t, true)
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
	uid := "11111111-1111-4111-8111-111111111111"
	a, err := admissionOf(kataClass(nil, &node.Scheduling{Tolerations: []core.Toleration{openShiftMemoryPressure}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !exactPodTemplate(admittedPod(template, a, uid), template, uid, a) {
		t.Fatal("class-declared memory-pressure toleration removed as an OpenShift addition")
	}
	plain := admittedPod(template, RuntimeClassAdmission{}, uid)
	plain.Spec.Tolerations = append(plain.Spec.Tolerations, openShiftMemoryPressure)
	if !exactPodTemplate(plain, template, uid, RuntimeClassAdmission{}) {
		t.Fatal("undeclared OpenShift memory-pressure addition not removed")
	}
	twice := admittedPod(template, a, uid)
	twice.Spec.Tolerations = append(twice.Spec.Tolerations, openShiftMemoryPressure)
	if exactPodTemplate(twice, template, uid, a) {
		t.Fatal("extra memory-pressure copy accepted")
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

// fastRuntimeClassRetries shortens the bounded retry for a test.
func fastRuntimeClassRetries(t *testing.T, budget, attempt time.Duration) {
	t.Helper()
	b, a, backoffs := runtimeClassBudget, runtimeClassAttempt, runtimeClassBackoffs
	runtimeClassBudget, runtimeClassAttempt, runtimeClassBackoffs = budget, attempt, []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { runtimeClassBudget, runtimeClassAttempt, runtimeClassBackoffs = b, a, backoffs })
}

func TestCheckRuntimeClassNamesEveryReason(t *testing.T) {
	fastRuntimeClassRetries(t, 5*time.Second, 2*time.Second)
	for label, c := range map[string]struct {
		handler func(http.ResponseWriter, *http.Request)
		reason  string
		unknown bool
		calls   int32
	}{
		"missing":      {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }, RuntimeClassNotFound, false, 1},
		"forbidden":    {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }, RuntimeClassForbidden, false, 1},
		"unauthorized": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }, RuntimeClassForbidden, false, 1},
		"teapot":       {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) }, RuntimeClassInvalid, false, 1},
		"throttled":    {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }, RuntimeClassThrottled, true, 3},
		"server":       {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, RuntimeClassServerError, true, 3},
		"unavailable":  {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, RuntimeClassServerError, true, 3},
		"gateway":      {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(504) }, RuntimeClassServerError, true, 3},
		"no handler": {func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata"}}`))
		}, RuntimeClassNoHandler, false, 1},
		"terminating": {func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata","deletionTimestamp":"2026-01-01T00:00:00Z"},"handler":"kata"}`))
		}, RuntimeClassTerminating, false, 1},
		"terminating without handler": {func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata","deletionTimestamp":"2026-01-01T00:00:00Z"}}`))
		}, RuntimeClassTerminating, false, 1},
		"other name": {func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"runc"},"handler":"runc"}`))
		}, RuntimeClassInvalid, false, 1},
		"malformed": {func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"kind":`)) }, RuntimeClassInvalid, false, 1},
		"oversized": {func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata","annotations":{"a":"` + strings.Repeat("x", maxRuntimeClassBytes) + `"}},"handler":"kata"}`))
		}, RuntimeClassInvalid, false, 1},
		"overhead": {func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata"},"handler":"kata","overhead":{"podFixed":{"cpu":"250m"}}}`))
		}, RuntimeClassOverhead, false, 1},
		"connection lost": {func(w http.ResponseWriter, _ *http.Request) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		}, RuntimeClassTransport, true, 3},
	} {
		client, calls := runtimeClassServer(t, c.handler)
		_, err := client.CheckRuntimeClass(context.Background(), "kata")
		if err == nil || RuntimeClassReason(err) != c.reason || RuntimeClassUnknown(err) != c.unknown || !KnownRuntimeClassReason(c.reason) {
			t.Fatal(label, "reason", RuntimeClassReason(err), RuntimeClassUnknown(err), err)
		}
		// Callers decide exactly as before.
		if c.reason == RuntimeClassOverhead {
			if !errors.Is(err, ErrRuntimeClassOverhead) {
				t.Fatal(label, "overhead no longer named")
			}
		} else if !errors.Is(err, ErrRuntimeClassUnavailable) {
			t.Fatal(label, "no longer unavailable")
		}
		// Only a transient failure is retried, and boundedly.
		if calls.Load() != c.calls {
			t.Fatal(label, "reads", calls.Load())
		}
		if reason, unknown := ClassifyRuntimeClass(err); reason != c.reason || unknown != c.unknown {
			t.Fatal(label, "classification")
		}
	}
	var nilClient *Client
	if _, err := nilClient.CheckRuntimeClass(context.Background(), "kata"); RuntimeClassReason(err) != RuntimeClassConfiguration || RuntimeClassUnknown(err) {
		t.Fatal("nil client reason", err)
	}
	client, calls := runtimeClassServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	if _, err := client.CheckRuntimeClass(context.Background(), "Kata"); RuntimeClassReason(err) != RuntimeClassConfiguration || calls.Load() != 0 {
		t.Fatal("invalid name reason", err)
	}
	if _, err := client.CheckRuntimeClass(context.Background(), "kata"); RuntimeClassReason(err) != RuntimeClassInvalid {
		t.Fatal("empty body reason", err)
	}
	for _, err := range []error{nil, errors.New("sandbox runtime class unavailable"), ErrRuntimeClassUnavailable, runtimeClassError{reason: "<api detail>", cause: ErrRuntimeClassUnavailable}} {
		if RuntimeClassReason(err) != "" || RuntimeClassUnknown(err) {
			t.Fatal("unclassified error named", err)
		}
	}
	if RuntimeClassReason(ErrRuntimeClassScheduling) != RuntimeClassScheduling {
		t.Fatal("scheduling not named")
	}
	for _, reason := range []string{"", "Timeout", "not found", "unknown", "<api detail>"} {
		if KnownRuntimeClassReason(reason) {
			t.Fatal("unbounded reason accepted", reason)
		}
	}
}

func TestCheckRuntimeClassRetriesATransientReadWithinOneCheck(t *testing.T) {
	fastRuntimeClassRetries(t, 5*time.Second, 2*time.Second)
	for _, status := range []int{429, 500, 503} {
		var reads atomic.Int32
		client, calls := runtimeClassServer(t, func(w http.ResponseWriter, _ *http.Request) {
			if reads.Add(1) < 3 {
				w.WriteHeader(status)
				return
			}
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata"},"handler":"kata","scheduling":{"nodeSelector":{"sandbox":"true"}}}`))
		})
		admission, err := client.CheckRuntimeClass(context.Background(), "kata")
		if err != nil || admission.NodeSelector["sandbox"] != "true" || calls.Load() != 3 {
			t.Fatal("transient read not retried", status, err, calls.Load())
		}
	}
	// A definite answer after a transient one is final.
	var reads atomic.Int32
	client, calls := runtimeClassServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if reads.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(404)
	})
	if _, err := client.CheckRuntimeClass(context.Background(), "kata"); RuntimeClassReason(err) != RuntimeClassNotFound || calls.Load() != 2 {
		t.Fatal("definite answer retried or misnamed", err, calls.Load())
	}
}

func TestCheckRuntimeClassTimeoutIsUnknownAndBounded(t *testing.T) {
	fastRuntimeClassRetries(t, 400*time.Millisecond, 50*time.Millisecond)
	release := make(chan struct{})
	defer close(release)
	client, calls := runtimeClassServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	started := time.Now()
	_, err := client.CheckRuntimeClass(context.Background(), "kata")
	if RuntimeClassReason(err) != RuntimeClassTimeout || !RuntimeClassUnknown(err) || !errors.Is(err, ErrRuntimeClassUnavailable) {
		t.Fatal("slow read not unknown", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second || calls.Load() != 3 {
		t.Fatal("retry not bounded", elapsed, calls.Load())
	}
	// The caller's deadline bounds the whole check, as a readiness probe's does.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started = time.Now()
	if _, err := client.CheckRuntimeClass(ctx, "kata"); RuntimeClassReason(err) != RuntimeClassTimeout || time.Since(started) > 300*time.Millisecond {
		t.Fatal("caller deadline not honoured", err, time.Since(started))
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	before := calls.Load()
	if _, err := client.CheckRuntimeClass(canceled, "kata"); RuntimeClassReason(err) != RuntimeClassTimeout || calls.Load() != before {
		t.Fatal("canceled check read the class", err)
	}
}

func TestRuntimeClassStopKeepsTheControllerStop(t *testing.T) {
	base := errors.New("verification stopped at sandbox-runtime-class")
	client, _ := runtimeClassServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	_, check := client.CheckRuntimeClass(context.Background(), "kata")
	wrapped := WithRuntimeClassReason(base, check)
	if !errors.Is(wrapped, base) || wrapped.Error() != base.Error() || RuntimeClassStopReason(wrapped) != RuntimeClassNotFound {
		t.Fatal("stop changed", wrapped)
	}
	if WithRuntimeClassReason(base, errors.New("other")) != base || RuntimeClassStopReason(base) != "" {
		t.Fatal("unclassified check attached a reason")
	}
	if RuntimeClassStopReason(runtimeClassStop{stop: base, reason: "<api detail>"}) != "" {
		t.Fatal("unbounded stop reason returned")
	}
}

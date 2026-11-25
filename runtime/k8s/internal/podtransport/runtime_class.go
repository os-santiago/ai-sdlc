package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	core "k8s.io/api/core/v1"
	node "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ErrRuntimeClassUnavailable means the installation's sandbox RuntimeClass
// could not be observed. Callers stay not ready and claim no work; there is
// no fallback to a pod without a sandbox runtime.
var ErrRuntimeClassUnavailable = errors.New("sandbox runtime class unavailable")

// ErrRuntimeClassOverhead means the class's pod overhead exceeds the per-pod
// quota headroom the installation declared, so its pods might not be admitted.
var ErrRuntimeClassOverhead = errors.New("sandbox runtime class overhead exceeds quota headroom")

// ErrRuntimeClassScheduling means the class's scheduling cannot be predicted
// exactly (for example tolerations that subsume others); it is refused.
var ErrRuntimeClassScheduling = errors.New("sandbox runtime class scheduling unsupported")

const (
	maxRuntimeClassBytes       = 64 * 1024
	maxRuntimeClassTolerations = 16
	maxRuntimeClassSelector    = 16
)

// declaredOverhead is the installation's per-pod headroom, which the chart
// adds to every sandbox namespace quota (sandbox.podOverhead). Empty means no
// headroom, so only a class without overhead is accepted.
var declaredOverhead = parseDeclaredOverhead(os.Getenv("AI_SDLC_SANDBOX_POD_OVERHEAD_CPU"), os.Getenv("AI_SDLC_SANDBOX_POD_OVERHEAD_MEMORY"))

func parseDeclaredOverhead(cpu, memory string) core.ResourceList {
	declared := core.ResourceList{}
	for name, value := range map[core.ResourceName]string{core.ResourceCPU: cpu, core.ResourceMemory: memory} {
		if value == "" {
			continue
		}
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() < 0 {
			// A malformed declaration grants no headroom at all.
			return core.ResourceList{}
		}
		declared[name] = q
	}
	return declared
}

// RuntimeClassAdmission is exactly what Kubernetes RuntimeClass admission adds
// to every pod of the class: spec.overhead from overhead.podFixed, and the
// class's scheduling nodeSelector and tolerations. The zero value is a class
// without overhead or scheduling (the kind ai-sdlc-offline class); using it
// for a class that has either only makes the exact pod comparison fail closed.
type RuntimeClassAdmission struct {
	Overhead     core.ResourceList
	NodeSelector map[string]string
	Tolerations  []core.Toleration
}

// CheckRuntimeClass reads one cluster-scoped RuntimeClass by name. It is a
// single bounded GET with the client's identity: it never creates, changes or
// lists runtime classes. The class must exist, carry a handler and not be
// terminating. The handler belongs to the platform, so any standard sandbox
// (gVisor, Kata, GKE Sandbox, AKS Pod Sandboxing) is accepted. It returns what
// admission adds to the class's pods, so each pod is still compared exactly
// with its compiled template plus those additions.
func (c *Client) CheckRuntimeClass(ctx context.Context, name string) (RuntimeClassAdmission, error) {
	if c == nil || c.http == nil || c.config == nil || !executor.ValidRuntimeClass(name) {
		return RuntimeClassAdmission{}, ErrRuntimeClassUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/apis/node.k8s.io/v1/runtimeclasses/"+name, nil)
	if err != nil {
		return RuntimeClassAdmission{}, ErrRuntimeClassUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if c.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.BearerToken)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return RuntimeClassAdmission{}, ErrRuntimeClassUnavailable
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxRuntimeClassBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || len(raw) > maxRuntimeClassBytes {
		return RuntimeClassAdmission{}, ErrRuntimeClassUnavailable
	}
	// Server-populated fields vary by platform, so this decode is not strict;
	// only identity, handler, lifecycle, overhead and scheduling are used.
	var class node.RuntimeClass
	if json.Unmarshal(raw, &class) != nil || class.Kind != "RuntimeClass" || class.Name != name || class.Handler == "" || class.DeletionTimestamp != nil {
		return RuntimeClassAdmission{}, ErrRuntimeClassUnavailable
	}
	return admissionOf(class, declaredOverhead)
}

func admissionOf(class node.RuntimeClass, headroom core.ResourceList) (RuntimeClassAdmission, error) {
	var a RuntimeClassAdmission
	if class.Overhead != nil && len(class.Overhead.PodFixed) > 0 {
		a.Overhead = core.ResourceList{}
		for name, q := range class.Overhead.PodFixed {
			limit, declared := headroom[name]
			if (name != core.ResourceCPU && name != core.ResourceMemory) || q.Sign() < 0 || (q.Sign() > 0 && (!declared || q.Cmp(limit) > 0)) {
				return RuntimeClassAdmission{}, ErrRuntimeClassOverhead
			}
			a.Overhead[name] = q.DeepCopy()
		}
	}
	if s := class.Scheduling; s != nil {
		if len(s.NodeSelector) > maxRuntimeClassSelector || len(s.Tolerations) > maxRuntimeClassTolerations {
			return RuntimeClassAdmission{}, ErrRuntimeClassScheduling
		}
		if len(s.NodeSelector) > 0 {
			a.NodeSelector = map[string]string{}
			for k, v := range s.NodeSelector {
				a.NodeSelector[k] = v
			}
		}
		// Admission merges tolerations and drops any that another subsumes.
		// Only classes whose tolerations are independent of each other and of
		// the API defaults are accepted, so the result is an exact union.
		all := append(defaultTolerations(), s.Tolerations...)
		for i := range all {
			for j := range all {
				if i != j && tolerationSubsumes(all[i], all[j]) {
					return RuntimeClassAdmission{}, ErrRuntimeClassScheduling
				}
			}
		}
		if len(s.Tolerations) > 0 {
			a.Tolerations = append([]core.Toleration(nil), s.Tolerations...)
		}
	}
	return a, nil
}

// tolerationSubsumes reports whether ss tolerates everything t tolerates, as
// the Kubernetes toleration merge decides redundancy (equal ones count too).
func tolerationSubsumes(ss, t core.Toleration) bool {
	if equality.Semantic.DeepEqual(ss, t) {
		return true
	}
	if t.Key != ss.Key && (ss.Key != "" || ss.Operator != core.TolerationOpExists) {
		return false
	}
	if t.Effect != ss.Effect && ss.Effect != "" {
		return false
	}
	if ss.Effect == core.TaintEffectNoExecute && ss.TolerationSeconds != nil && (t.TolerationSeconds == nil || *t.TolerationSeconds > *ss.TolerationSeconds) {
		return false
	}
	switch ss.Operator {
	case core.TolerationOpEqual, "":
		return t.Operator == core.TolerationOpEqual && t.Value == ss.Value
	case core.TolerationOpExists:
		return true
	}
	// An unknown operator cannot be reasoned about, so it counts as overlap.
	return true
}

// applyAdmission augments an expected pod spec, after the API defaults, with
// exactly what RuntimeClass admission adds. A compiled selector that conflicts
// with the class's fails, as admission would also reject it.
func applyAdmission(s *core.PodSpec, a RuntimeClassAdmission) bool {
	if len(a.Overhead) > 0 {
		s.Overhead = core.ResourceList{}
		for name, q := range a.Overhead {
			s.Overhead[name] = q.DeepCopy()
		}
	}
	if len(a.NodeSelector) > 0 {
		merged := map[string]string{}
		for k, v := range s.NodeSelector {
			merged[k] = v
		}
		for k, v := range a.NodeSelector {
			if existing, ok := merged[k]; ok && existing != v {
				return false
			}
			merged[k] = v
		}
		s.NodeSelector = merged
	}
	s.Tolerations = append(append([]core.Toleration(nil), s.Tolerations...), a.Tolerations...)
	canonicalTolerations(s)
	return true
}

// canonicalTolerations orders tolerations deterministically. Their order has
// no meaning, and admission plugins may append them in either order.
func canonicalTolerations(s *core.PodSpec) {
	if len(s.Tolerations) < 2 {
		return
	}
	s.Tolerations = append([]core.Toleration(nil), s.Tolerations...)
	key := func(t core.Toleration) string {
		raw, _ := json.Marshal(t)
		return string(raw)
	}
	sort.SliceStable(s.Tolerations, func(i, j int) bool { return key(s.Tolerations[i]) < key(s.Tolerations[j]) })
}

func defaultTolerations() []core.Toleration {
	var s core.PodSpec
	applyDefaults(&s)
	return s.Tolerations
}

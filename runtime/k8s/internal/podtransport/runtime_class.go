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

// Bounded retry of transient RuntimeClass reads within one check. Variables
// only so tests can shorten them.
var (
	runtimeClassBudget   = 5 * time.Second
	runtimeClassAttempt  = 2 * time.Second
	runtimeClassBackoffs = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}
)

// RuntimeClass check reasons. Each failed check names exactly one. The first
// four are transient (the class is unknown, not absent); the rest are
// definite observations or refusals.
const (
	RuntimeClassTimeout       = "timeout"       // the read or its caller's deadline expired
	RuntimeClassTransport     = "transport"     // no response (connection refused, reset, TLS)
	RuntimeClassThrottled     = "throttled"     // HTTP 429
	RuntimeClassServerError   = "server-error"  // HTTP 5xx
	RuntimeClassNotFound      = "not-found"     // HTTP 404
	RuntimeClassForbidden     = "forbidden"     // HTTP 401 or 403
	RuntimeClassNoHandler     = "no-handler"    // the class has no handler
	RuntimeClassTerminating   = "terminating"   // the class is being deleted
	RuntimeClassInvalid       = "invalid"       // another status, oversized, malformed or another identity
	RuntimeClassOverhead      = "overhead"      // ErrRuntimeClassOverhead
	RuntimeClassScheduling    = "scheduling"    // ErrRuntimeClassScheduling
	RuntimeClassConfiguration = "configuration" // no client or an invalid class name
)

// runtimeClassError names why a check failed. It unwraps to the error the
// check always returned (ErrRuntimeClassUnavailable, ErrRuntimeClassOverhead
// or ErrRuntimeClassScheduling), so every caller decides exactly as before.
type runtimeClassError struct {
	reason string
	cause  error
}

func (e runtimeClassError) Error() string { return e.cause.Error() }
func (e runtimeClassError) Unwrap() error { return e.cause }

func classFailure(reason string) error {
	return runtimeClassError{reason: reason, cause: ErrRuntimeClassUnavailable}
}

// RuntimeClassReason returns the bounded reason a check failed, or "" for an
// error CheckRuntimeClass did not return.
func RuntimeClassReason(err error) string {
	var failed runtimeClassError
	if errors.As(err, &failed) && KnownRuntimeClassReason(failed.reason) {
		return failed.reason
	}
	switch {
	case errors.Is(err, ErrRuntimeClassOverhead):
		return RuntimeClassOverhead
	case errors.Is(err, ErrRuntimeClassScheduling):
		return RuntimeClassScheduling
	}
	return ""
}

// RuntimeClassUnknown reports that a check failed only transiently: the API
// server did not answer in time or answered with 429 or 5xx. The class is
// then unknown, never available: a caller still refuses new work.
func RuntimeClassUnknown(err error) bool {
	switch RuntimeClassReason(err) {
	case RuntimeClassTimeout, RuntimeClassTransport, RuntimeClassThrottled, RuntimeClassServerError:
		return true
	}
	return false
}

// ClassifyRuntimeClass returns RuntimeClassReason and RuntimeClassUnknown
// together, the shape a readiness gate consumes.
func ClassifyRuntimeClass(err error) (reason string, unknown bool) {
	return RuntimeClassReason(err), RuntimeClassUnknown(err)
}

// runtimeClassStop is a controller's stop before a claim because the class
// check failed. It unwraps to the controller's own stop error, so every
// caller decides exactly as before; the reason only refines the diagnostic.
type runtimeClassStop struct {
	stop   error
	reason string
}

func (e runtimeClassStop) Error() string { return e.stop.Error() }
func (e runtimeClassStop) Unwrap() error { return e.stop }

// WithRuntimeClassReason attaches the failed check's bounded reason to stop.
func WithRuntimeClassReason(stop, check error) error {
	if reason := RuntimeClassReason(check); reason != "" {
		return runtimeClassStop{stop: stop, reason: reason}
	}
	return stop
}

// RuntimeClassStopReason returns the reason WithRuntimeClassReason attached,
// or "".
func RuntimeClassStopReason(err error) string {
	var stopped runtimeClassStop
	if errors.As(err, &stopped) && KnownRuntimeClassReason(stopped.reason) {
		return stopped.reason
	}
	return ""
}

// KnownRuntimeClassReason reports whether reason is one RuntimeClassReason
// can return, so a diagnostic allowlist stays bounded.
func KnownRuntimeClassReason(reason string) bool {
	switch reason {
	case RuntimeClassTimeout, RuntimeClassTransport, RuntimeClassThrottled, RuntimeClassServerError, RuntimeClassNotFound, RuntimeClassForbidden,
		RuntimeClassNoHandler, RuntimeClassTerminating, RuntimeClassInvalid, RuntimeClassOverhead, RuntimeClassScheduling, RuntimeClassConfiguration:
		return true
	}
	return false
}

// CheckRuntimeClass reads one cluster-scoped RuntimeClass by name with the
// client's identity: it never creates, changes or lists runtime classes. The
// class must exist, carry a handler and not be terminating. The handler
// belongs to the platform, so any standard sandbox (gVisor, Kata, GKE
// Sandbox, AKS Pod Sandboxing) is accepted. It returns what admission adds to
// the class's pods, so each pod is still compared exactly with its compiled
// template plus those additions.
//
// A transient failure (timeout, no response, 429 or 5xx) is retried with a
// short bounded backoff inside the same check, within 5 s and the caller's
// deadline; a definite answer (404, no handler, terminating, refused
// overhead or scheduling) is returned at once. A check that still fails
// names its reason (RuntimeClassReason) and never reports the class
// available.
func (c *Client) CheckRuntimeClass(ctx context.Context, name string) (RuntimeClassAdmission, error) {
	if c == nil || c.http == nil || c.config == nil || !executor.ValidRuntimeClass(name) {
		return RuntimeClassAdmission{}, classFailure(RuntimeClassConfiguration)
	}
	ctx, cancel := context.WithTimeout(ctx, runtimeClassBudget)
	defer cancel()
	for attempt := 0; ; attempt++ {
		admission, err := c.readRuntimeClass(ctx, name)
		if err == nil || !RuntimeClassUnknown(err) || attempt >= len(runtimeClassBackoffs) {
			return admission, err
		}
		select {
		case <-ctx.Done():
			return RuntimeClassAdmission{}, classFailure(RuntimeClassTimeout)
		case <-time.After(runtimeClassBackoffs[attempt]):
		}
	}
}

// readRuntimeClass is one bounded GET of the class.
func (c *Client) readRuntimeClass(ctx context.Context, name string) (RuntimeClassAdmission, error) {
	if ctx.Err() != nil {
		return RuntimeClassAdmission{}, classFailure(RuntimeClassTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, runtimeClassAttempt)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/apis/node.k8s.io/v1/runtimeclasses/"+name, nil)
	if err != nil {
		return RuntimeClassAdmission{}, classFailure(RuntimeClassConfiguration)
	}
	req.Header.Set("Accept", "application/json")
	if c.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.BearerToken)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return RuntimeClassAdmission{}, classFailure(RuntimeClassTimeout)
		}
		return RuntimeClassAdmission{}, classFailure(RuntimeClassTransport)
	}
	defer response.Body.Close()
	// Definite statuses are classified before the body is read, so an
	// interrupted error body cannot turn a refusal into a transient failure.
	switch {
	case response.StatusCode == http.StatusNotFound:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassNotFound)
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassForbidden)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxRuntimeClassBytes+1))
	switch {
	case err != nil && ctx.Err() != nil:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassTimeout)
	case err != nil:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassTransport)
	case response.StatusCode == http.StatusTooManyRequests:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassThrottled)
	case response.StatusCode >= 500 && response.StatusCode <= 599:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassServerError)
	case response.StatusCode != http.StatusOK || len(raw) > maxRuntimeClassBytes:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassInvalid)
	}
	// Server-populated fields vary by platform, so this decode is not strict;
	// only identity, handler, lifecycle, overhead and scheduling are used.
	var class node.RuntimeClass
	switch {
	case json.Unmarshal(raw, &class) != nil || class.Kind != "RuntimeClass" || class.Name != name:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassInvalid)
	case class.DeletionTimestamp != nil:
		return RuntimeClassAdmission{}, classFailure(RuntimeClassTerminating)
	case class.Handler == "":
		return RuntimeClassAdmission{}, classFailure(RuntimeClassNoHandler)
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

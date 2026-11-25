package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var resourceUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// Resources is a fixed compiled workload, not execution authority. A trusted
// lifecycle controller must persist intent before each mutating operation.
// There is no arbitrary manifest, resource kind, namespace or retry option.
type Resources struct {
	client  *Client
	binding Binding
	config  core.ConfigMap
	pod     core.Pod
	expires time.Time
}

func PlanResources(client *Client, b Binding) (*Resources, error) {
	if client == nil || client.http == nil || client.config == nil || b.PodUID != "" || len(b.Assignment) == 0 || len(b.Assignment) > executor.MaxAssignmentBytes || len(b.Profile) == 0 || len(b.Profile) > executor.MaxProfileBytes || len(b.ADev) == 0 || len(b.ADev) > 64*1024 {
		return nil, ErrTransport
	}
	b.Assignment = append([]byte(nil), b.Assignment...)
	b.Profile = append([]byte(nil), b.Profile...)
	b.ADev = append([]byte(nil), b.ADev...)
	w, err := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.RuntimeClass, b.Assignment, b.Profile, b.ADev, time.Now())
	if err != nil {
		return nil, ErrTransport
	}
	a, err := executor.ParseAssignment(b.Assignment, modelmailbox.Digest(b.Assignment), time.Now())
	if err != nil {
		return nil, ErrTransport
	}
	r := &Resources{client: client, binding: b, expires: time.UnixMilli(a.ExpiresAt)}
	config, _ := json.Marshal(w.ConfigMap)
	pod, _ := json.Marshal(w.Pod)
	if authority.DecodeStrict(config, &r.config) != nil || authority.DecodeStrict(pod, &r.pod) != nil {
		return nil, ErrTransport
	}
	return r, nil
}
func (r *Resources) url(kind, name string) string {
	path := r.client.base + "/api/v1/namespaces/" + r.binding.Namespace + "/" + kind
	if name != "" {
		path += "/" + name
	}
	return path
}
func (r *Resources) request(ctx context.Context, method, kind, name string, input []byte, cleanup bool) ([]byte, int, error) {
	if r == nil || len(input) > maxPodBytes || (!cleanup && !time.Now().Before(r.expires)) {
		return nil, 0, ErrTransport
	}
	deadline := time.Now().Add(5 * time.Second)
	if !cleanup {
		deadline = minDeadline(deadline, r.expires)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.url(kind, name), bytes.NewReader(input))
	if err != nil {
		return nil, 0, ErrTransport
	}
	req.Header.Set("Accept", "application/json")
	if len(input) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.client.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.client.config.BearerToken)
	}
	response, err := r.client.http.Do(req)
	if err != nil {
		return nil, 0, ErrTransport
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPodBytes+1))
	if err != nil || len(raw) > maxPodBytes || ctx.Err() != nil || (!cleanup && !time.Now().Before(r.expires)) {
		return nil, 0, ErrTransport
	}
	if response.StatusCode == http.StatusForbidden {
		return nil, response.StatusCode, ErrPermission
	}
	return raw, response.StatusCode, nil
}
func exactMetadata(actual, expected meta.ObjectMeta, uid string) bool {
	return resourceUID.MatchString(string(actual.UID)) && (uid == "" || string(actual.UID) == uid) && actual.Name == expected.Name && actual.Namespace == expected.Namespace && actual.GenerateName == "" && actual.DeletionTimestamp == nil && actual.DeletionGracePeriodSeconds == nil && len(actual.OwnerReferences) == 0 && len(actual.Finalizers) == 0 && reflect.DeepEqual(actual.Labels, expected.Labels) && reflect.DeepEqual(actual.Annotations, expected.Annotations)
}
func (r *Resources) checkConfig(raw []byte, uid string) (string, error) {
	var actual core.ConfigMap
	if authority.DecodeStrict(raw, &actual) != nil || actual.APIVersion != "v1" || actual.Kind != "ConfigMap" || !exactMetadata(actual.ObjectMeta, r.config.ObjectMeta, uid) || actual.Immutable == nil || !*actual.Immutable || !reflect.DeepEqual(actual.Data, r.config.Data) || !reflect.DeepEqual(actual.BinaryData, r.config.BinaryData) {
		return "", ErrTransport
	}
	return string(actual.UID), nil
}

// exactPodTemplate compares an observed pod with its compiled template plus
// the exact API defaults and the RuntimeClass admission additions. Any other
// difference, including any other added field, fails.
func exactPodTemplate(actual, expected core.Pod, uid string, admission RuntimeClassAdmission) bool {
	// The platform normalization sees the template with the defaults and the
	// class additions, so a toleration the class declares is never removed
	// as an OpenShift addition.
	expected = *expected.DeepCopy()
	applyDefaults(&expected.Spec)
	if !applyAdmission(&expected.Spec, admission) {
		return false
	}
	actual = platformObserved(actual, expected)
	if actual.APIVersion != "v1" || actual.Kind != "Pod" || !exactMetadata(actual.ObjectMeta, expected.ObjectMeta, uid) {
		return false
	}
	actual = *actual.DeepCopy()
	actual.Spec.NodeName = ""
	canonicalTolerations(&actual.Spec)
	return equality.Semantic.DeepEqual(actual.Spec, expected.Spec)
}
func (r *Resources) getConfig(ctx context.Context, uid string) error {
	raw, status, err := r.getTransient(ctx, "configmaps", r.config.Name)
	if err != nil {
		return err
	}
	if status != 200 {
		return ErrTransport
	}
	_, err = r.checkConfig(raw, uid)
	return err
}
func (r *Resources) CreateConfig(ctx context.Context) (string, error) {
	if r == nil {
		return "", ErrTransport
	}
	raw, _ := json.Marshal(r.config)
	response, status, err := r.request(ctx, http.MethodPost, "configmaps", "", raw, false)
	if err != nil {
		return "", err
	}
	if status != 201 {
		return "", ErrTransport
	}
	return r.checkConfig(response, "")
}
func (r *Resources) CreatePod(ctx context.Context, configUID string) (string, error) {
	uid, err := r.CreatePodObserved(ctx, configUID)
	if err != nil {
		return "", err
	}
	return uid, nil
}

// CreatePodObserved is CreatePod for a ledger that must remove what it
// created: from a decoded 201 on, the pod exists and its UID is known, so the
// UID is returned with any later failure. Callers still treat the error as a
// failed create; the UID serves only to acknowledge and delete that object.
func (r *Resources) CreatePodObserved(ctx context.Context, configUID string) (string, error) {
	if r == nil || !resourceUID.MatchString(configUID) {
		return "", ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// A retry of this read must leave the POST its full bound: a POST cut short
	// could create a pod whose UID is never learned.
	if err := r.getConfig(withReadReserve(ctx, readAttemptBound+time.Second), configUID); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(r.pod)
	response, status, err := r.request(ctx, http.MethodPost, "pods", "", raw, false)
	if err != nil {
		return "", err
	}
	if status != 201 {
		return "", ErrTransport
	}
	var pod core.Pod
	if authority.DecodeStrict(response, &pod) != nil {
		return "", ErrTransport
	}
	created := ""
	if resourceUID.MatchString(string(pod.UID)) {
		created = string(pod.UID)
	}
	if !exactPodTemplate(pod, r.pod, "", r.binding.Admission) || (pod.Status.Phase != core.PodPending && pod.Status.Phase != core.PodRunning) {
		return created, ErrTransport
	}
	if err = r.getConfig(ctx, configUID); err != nil {
		return created, err
	}
	return created, nil
}

// StartBudget bounds a pod's Pending phase: scheduling, a first image pull
// from a registry and the offline sandbox start. It never exceeds the
// assignment's expiry.
const StartBudget = 120 * time.Second

// WaitRunning polls only the compiled Pod name and exact returned UID. Pending
// scheduling is bounded; mutations, replacement and terminal states fail.
func (r *Resources) WaitRunning(ctx context.Context, uid string) (*Session, error) {
	if r == nil || !resourceUID.MatchString(uid) {
		return nil, readinessError{step: "uid"}
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(startBudget(r.pod)), r.expires))
	defer cancel()
	// Init container IDs, once observed, must never change, and a container
	// observed started is never again reported created. Worker and mailbox
	// container IDs, once observed (created or running), are held too.
	seen, started, mainIDs := map[string]instance{}, map[string]bool{}, map[string]string{}
	for {
		raw, status, err := r.readPod(ctx)
		if err != nil {
			step := "request"
			switch {
			case errors.Is(err, ErrPermission):
				step = "permission"
			case ctx.Err() != nil:
				step = "deadline"
			}
			return nil, readinessError{step: step, cause: err}
		}
		if status != 200 {
			return nil, readinessError{step: "status"}
		}
		var pod core.Pod
		if authority.DecodeStrict(raw, &pod) != nil || !exactPodTemplate(pod, r.pod, uid, r.binding.Admission) {
			return nil, readinessError{step: "template"}
		}
		// The init sequence is checked first, so a failed toolchain copy
		// (which fails the pod) is named rather than a generic phase stop.
		ready, err := r.containersReady(pod, seen, started, mainIDs)
		if err != nil {
			return nil, err
		}
		if pod.Status.Phase != core.PodPending && pod.Status.Phase != core.PodRunning {
			return nil, readinessError{step: "phase"}
		}
		if ready && pod.Status.Phase == core.PodRunning {
			b := r.binding
			b.PodUID = uid
			session, err := Bind(ctx, r.client, b)
			if err != nil {
				if ReadinessStep(err) != "" {
					return nil, err
				}
				return nil, readinessError{step: "bind", cause: err}
			}
			// The session's baseline is the same init containers observed here.
			if !stableInit(seen, session.initInstances()) {
				return nil, readinessError{step: "bind-unstable"}
			}
			return session, nil
		}
		select {
		case <-ctx.Done():
			return nil, readinessError{step: "deadline"}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Only initial ContainerCreating/PodInitializing states may be polled. This
// is not a generic retry of failed binding, restarts or unknown transport errors.
// The compiled init containers must progress exactly in order before any
// worker or mailbox container starts.
//
// A worker or mailbox container may be reported created but not started
// (see createdStatus) only after the init sequence completed, and never
// after it was observed running. A worker or mailbox container ID, once
// observed, never changes and never disappears (containers-unstable).
func (r *Resources) containersReady(pod core.Pod, seen map[string]instance, everStarted map[string]bool, mainIDs map[string]string) (bool, error) {
	if len(pod.Status.ContainerStatuses) > 2 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return false, readinessError{step: "containers"}
	}
	initialized, err := observeInit(r.pod.Spec.InitContainers, pod.Status.InitContainerStatuses, false)
	if err != nil {
		return false, err
	}
	if !stableInit(seen, initialized.instances) || !neverUnstarted(everStarted, initialized.instances, initialized.created) {
		return false, initRefusal("unstable")
	}
	images := map[string]string{}
	for _, container := range r.pod.Spec.Containers {
		images[container.Name] = container.Image
	}
	started := map[string]bool{}
	running := 0
	for _, status := range pod.Status.ContainerStatuses {
		pin, known := images[status.Name]
		if !known || started[status.Name] || status.RestartCount != 0 || status.State.Terminated != nil || status.LastTerminationState.Terminated != nil || status.LastTerminationState.Running != nil || status.LastTerminationState.Waiting != nil {
			return false, readinessError{step: "containers"}
		}
		started[status.Name] = true
		if len(status.Image) > 1024 || len(status.ImageID) > 1024 {
			return false, readinessError{step: "containers"}
		}
		if status.ImageID != "" && (!imageIDPattern.MatchString(status.ImageID) || !imageMatchesPin(status.Image, status.ImageID, pin)) {
			return false, readinessError{step: "containers"}
		}
		if previous, ok := mainIDs[status.Name]; ok && status.ContainerID != previous {
			return false, readinessError{step: "containers-unstable"}
		}
		if status.State.Running != nil && status.State.Waiting == nil {
			if !initialized.complete {
				return false, readinessError{step: "containers-before-init"}
			}
			if !containerIDPattern.MatchString(status.ContainerID) || status.ImageID == "" || status.Image == "" {
				return false, readinessError{step: "containers"}
			}
			everStarted[status.Name] = true
			mainIDs[status.Name] = status.ContainerID
			running++
			continue
		}
		if status.ContainerID != "" && status.State.Running == nil && status.State.Waiting != nil {
			// Created, not yet started (the kubelet's ContainerStateCreated).
			if !initialized.complete {
				return false, readinessError{step: "containers-before-init"}
			}
			if !createdStatus(status) || everStarted[status.Name] {
				return false, readinessError{step: "containers-created"}
			}
			mainIDs[status.Name] = status.ContainerID
			continue
		}
		if status.State.Running != nil || status.State.Waiting == nil || status.ContainerID != "" || (status.State.Waiting.Reason != "ContainerCreating" && status.State.Waiting.Reason != "PodInitializing") {
			return false, readinessError{step: "containers"}
		}
	}
	for name := range mainIDs {
		if !started[name] {
			return false, readinessError{step: "containers-unstable"}
		}
	}
	return running == 2, nil
}

func (r *Resources) DeletePod(ctx context.Context, uid string) error {
	if r == nil {
		return ErrTransport
	}
	return r.remove(ctx, "pods", r.pod.Name, uid)
}
func (r *Resources) DeleteConfig(ctx context.Context, uid string) error {
	if r == nil {
		return ErrTransport
	}
	return r.remove(ctx, "configmaps", r.config.Name, uid)
}

// Cleanup is allowed after assignment expiry, but only for an explicit original
// UID. A successful DELETE response alone does not prove removal.
func (r *Resources) remove(ctx context.Context, kind, name, uid string) error {
	if !resourceUID.MatchString(uid) {
		return removalError{"invalid-uid", ErrTransport}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id := types.UID(uid)
	grace := int64(1)
	propagation := meta.DeletePropagationBackground
	options := meta.DeleteOptions{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "DeleteOptions"}, Preconditions: &meta.Preconditions{UID: &id}, GracePeriodSeconds: &grace, PropagationPolicy: &propagation}
	input, _ := json.Marshal(options)
	var status int
	for {
		var err error
		_, status, err = r.request(ctx, http.MethodDelete, kind, name, input, true)
		if err == nil && !transientStatus(status) {
			break
		}
		// A dropped connection or an overloaded API server is retried within
		// the same bound: the UID precondition makes a repeated DELETE safe,
		// and a 404 proves the original is gone. A refusal is never retried.
		if (err != nil && errors.Is(err, ErrPermission)) || !retryRemoval(ctx) {
			if err != nil {
				return removalError{requestStep("delete", err), err}
			}
			break
		}
	}
	if status == 404 {
		return nil
	}
	if status != 200 && status != 202 {
		return removalError{"delete-status-" + statusClass(status), ErrTransport}
	}
	for {
		raw, status, err := r.request(ctx, http.MethodGet, kind, name, nil, true)
		if err != nil {
			if ctx.Err() != nil {
				return removalError{"observe-timeout", err}
			}
			if !errors.Is(err, ErrPermission) && retryRemoval(ctx) {
				continue
			}
			return removalError{requestStep("observe", err), err}
		}
		if status == 404 {
			return nil
		}
		if transientStatus(status) && retryRemoval(ctx) {
			continue
		}
		if status != 200 {
			return removalError{"observe-status-" + statusClass(status), ErrTransport}
		}
		var object struct {
			meta.TypeMeta `json:",inline"`
			Metadata      meta.ObjectMeta `json:"metadata"`
			Spec          json.RawMessage `json:"spec,omitempty"`
			Status        json.RawMessage `json:"status,omitempty"`
			Data          json.RawMessage `json:"data,omitempty"`
			BinaryData    json.RawMessage `json:"binaryData,omitempty"`
			Immutable     *bool           `json:"immutable,omitempty"`
		}
		if authority.DecodeStrict(raw, &object) != nil {
			return removalError{"observe-decode", ErrTransport}
		}
		if string(object.Metadata.UID) != uid || object.Metadata.Name != name || object.Metadata.Namespace != r.binding.Namespace {
			return removalError{"observe-identity", ErrTransport}
		}
		select {
		case <-ctx.Done():
			return removalError{"observe-timeout", ErrTransport}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// observeRetryWindow bounds when a read may start another attempt after a
// dropped connection or a throttled/unavailable API server answer. Each
// attempt keeps request's own 5-second bound, so a slow but valid answer is
// never cut short by the retry window.
const observeRetryWindow = 3 * time.Second

// readPod is one read of the planned pod (see getTransient).
func (r *Resources) readPod(ctx context.Context) ([]byte, int, error) {
	return r.getTransient(ctx, "pods", r.pod.Name)
}

// getTransient is one GET of a planned object. A transport failure or a
// 429/5xx answer starts another attempt while observeRetryWindow lasts:
// evaluation batteries 27 and 28 stopped attempts at readiness-request and
// baseline-observe-request after one failed GET. A permission refusal and
// every other answer are returned at once, and nothing is retried once the
// assignment has expired. A GET never changes the object, so repeating it is
// safe.
func (r *Resources) getTransient(ctx context.Context, kind, name string) ([]byte, int, error) {
	return retryTransient(ctx, r.expires, func() ([]byte, int, error) {
		return r.request(ctx, http.MethodGet, kind, name, nil, false)
	})
}

// minReadAttempts is how many attempts a transient read always gets, however
// long each took. An attempt that timed out after request's 5-second bound
// used up observeRetryWindow on its own; evaluation battery 29 still stopped
// at readiness-request that way, after the window-only retry of #800.
const minReadAttempts = 3

// readReserveKey carries, in a read's context, the time a caller keeps for
// the steps after the read (see withReadReserve).
type readReserveKey struct{}

// withReadReserve marks ctx so that a transient read retries only while at
// least reserve remains for the caller's next steps after the retry: the
// first attempt is unchanged, but no retry may starve, for example, a pod
// POST that needs its own 5 s or an exchange that shares the same budget.
func withReadReserve(ctx context.Context, reserve time.Duration) context.Context {
	return context.WithValue(ctx, readReserveKey{}, reserve)
}

// retryTransient runs read until it returns a non-transient outcome. Another
// attempt starts while observeRetryWindow lasts (quick failures such as a
// refused connection) or until minReadAttempts attempts were made (slow
// failures such as a timeout), and never after the expiry or once ctx has no
// room for the pause. With a read reserve, a retry starts only when the
// reserve plus a full attempt still fit before ctx's deadline. Each attempt
// keeps its own bound and the caller's ctx.
func retryTransient(ctx context.Context, expires time.Time, read func() ([]byte, int, error)) ([]byte, int, error) {
	last := time.Now().Add(observeRetryWindow)
	reserve, _ := ctx.Value(readReserveKey{}).(time.Duration)
	for attempts := 1; ; attempts++ {
		raw, status, err := read()
		transient := (err != nil && !errors.Is(err, ErrPermission)) || (err == nil && transientStatus(status))
		more := time.Now().Before(last) || attempts < minReadAttempts
		if reserve > 0 {
			deadline, ok := ctx.Deadline()
			more = more && ok && time.Until(deadline) >= reserve+readAttemptBound+250*time.Millisecond
		}
		if !transient || !more || !time.Now().Before(expires) || !retryRemoval(ctx) {
			return raw, status, err
		}
	}
}

// readAttemptBound is request's own bound for one attempt.
const readAttemptBound = 5 * time.Second

// transientStatus is an API server answer worth repeating within the removal
// bound: throttling or a temporarily unavailable server.
func transientStatus(status int) bool {
	switch status {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// retryRemoval waits briefly before another removal request; false once the
// removal bound has no room left for one.
func retryRemoval(ctx context.Context) bool {
	const pause = 250 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < pause {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(pause):
		return true
	}
}

// removalError names the bounded removal step that failed. It unwraps to the
// original transport or permission error, so every caller still stops the
// same way; the step only refines the stop diagnostic.
type removalError struct {
	step  string
	cause error
}

func (e removalError) Error() string { return "resource removal failed at " + e.step }
func (e removalError) Unwrap() error { return e.cause }

// RemovalStep returns the failed removal step, or "" for any other error.
func RemovalStep(err error) string {
	var failed removalError
	if errors.As(err, &failed) {
		return failed.step
	}
	return ""
}

var removalSteps = regexp.MustCompile(`^(invalid-uid|(delete|observe)-(request|permission)|observe-(timeout|decode|identity)|(delete|observe)-status-(401|409|410|422|429|500|503|504|[1-5]xx|invalid))$`)

// KnownRemovalStep reports whether step is one RemovalStep can return, so a
// diagnostic allowlist stays bounded.
func KnownRemovalStep(step string) bool { return removalSteps.MatchString(step) }

func requestStep(phase string, err error) string {
	if errors.Is(err, ErrPermission) {
		return phase + "-permission"
	}
	return phase + "-request"
}

// statusClass keeps the diagnostic bounded: the exact code for the few
// statuses a removal can plausibly see, otherwise only its class.
func statusClass(status int) string {
	switch status {
	case 401, 409, 410, 422, 429, 500, 503, 504:
		return strconv.Itoa(status)
	}
	if status >= 100 && status <= 599 {
		return strconv.Itoa(status/100) + "xx"
	}
	return "invalid"
}

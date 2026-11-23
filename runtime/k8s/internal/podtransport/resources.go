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
	w, err := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.Assignment, b.Profile, b.ADev, time.Now())
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
func exactPodTemplate(actual, expected core.Pod, uid string) bool {
	actual = platformObserved(actual, expected)
	if actual.APIVersion != "v1" || actual.Kind != "Pod" || !exactMetadata(actual.ObjectMeta, expected.ObjectMeta, uid) {
		return false
	}
	expected = *expected.DeepCopy()
	applyDefaults(&expected.Spec)
	actual = *actual.DeepCopy()
	actual.Spec.NodeName = ""
	return equality.Semantic.DeepEqual(actual.Spec, expected.Spec)
}
func (r *Resources) getConfig(ctx context.Context, uid string) error {
	raw, status, err := r.request(ctx, http.MethodGet, "configmaps", r.config.Name, nil, false)
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
	if err := r.getConfig(ctx, configUID); err != nil {
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
	if !exactPodTemplate(pod, r.pod, "") || (pod.Status.Phase != core.PodPending && pod.Status.Phase != core.PodRunning) {
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
		return nil, ErrTransport
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(StartBudget), r.expires))
	defer cancel()
	for {
		raw, status, err := r.request(ctx, http.MethodGet, "pods", r.pod.Name, nil, false)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, ErrTransport
		}
		var pod core.Pod
		if authority.DecodeStrict(raw, &pod) != nil || !exactPodTemplate(pod, r.pod, uid) {
			return nil, ErrTransport
		}
		if pod.Status.Phase != core.PodPending && pod.Status.Phase != core.PodRunning {
			return nil, ErrTransport
		}
		ready, err := r.containersReady(pod)
		if err != nil {
			return nil, err
		}
		if ready && pod.Status.Phase == core.PodRunning {
			b := r.binding
			b.PodUID = uid
			return Bind(ctx, r.client, b)
		}
		select {
		case <-ctx.Done():
			return nil, ErrTransport
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Only initial ContainerCreating/PodInitializing states may be polled. This
// is not a generic retry of failed binding, restarts or unknown transport errors.
func (r *Resources) containersReady(pod core.Pod) (bool, error) {
	if len(pod.Status.ContainerStatuses) > 2 || len(pod.Status.InitContainerStatuses) != 0 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return false, ErrTransport
	}
	images := map[string]string{}
	for _, container := range r.pod.Spec.Containers {
		images[container.Name] = container.Image
	}
	seen := map[string]bool{}
	running := 0
	for _, status := range pod.Status.ContainerStatuses {
		pin, known := images[status.Name]
		if !known || seen[status.Name] || status.RestartCount != 0 || status.State.Terminated != nil || status.LastTerminationState.Terminated != nil || status.LastTerminationState.Running != nil || status.LastTerminationState.Waiting != nil {
			return false, ErrTransport
		}
		seen[status.Name] = true
		if len(status.Image) > 1024 || len(status.ImageID) > 1024 {
			return false, ErrTransport
		}
		if status.ImageID != "" && (!imageIDPattern.MatchString(status.ImageID) || !imageMatchesPin(status.Image, status.ImageID, pin)) {
			return false, ErrTransport
		}
		if status.State.Running != nil && status.State.Waiting == nil {
			if !containerIDPattern.MatchString(status.ContainerID) || status.ImageID == "" || status.Image == "" {
				return false, ErrTransport
			}
			running++
			continue
		}
		if status.State.Running != nil || status.State.Waiting == nil || status.ContainerID != "" || (status.State.Waiting.Reason != "ContainerCreating" && status.State.Waiting.Reason != "PodInitializing") {
			return false, ErrTransport
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
	_, status, err := r.request(ctx, http.MethodDelete, kind, name, input, true)
	if err != nil {
		return removalError{requestStep("delete", err), err}
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
			return removalError{requestStep("observe", err), err}
		}
		if status == 404 {
			return nil
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

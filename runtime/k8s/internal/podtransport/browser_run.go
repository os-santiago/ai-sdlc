package podtransport

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func sameBrowserRoute(a, b ConsoleRollout) bool { a.ObservedAt = b.ObservedAt; return a == b }
func (b *BrowserResources) empty(ctx context.Context) error {
	raw, status, e := b.resources.request(ctx, http.MethodGet, "pods", "", nil, false)
	var pods core.PodList
	if e != nil || status != 200 || authority.DecodeStrict(raw, &pods) != nil || len(pods.Items) != 0 {
		return ErrTransport
	}
	return nil
}
func (b *BrowserResources) observePod(ctx context.Context, uid string) (core.Pod, error) {
	var pod core.Pod
	raw, status, e := b.resources.request(ctx, http.MethodGet, "pods", b.pod.Name, nil, false)
	if e != nil || status != 200 || authority.DecodeStrict(raw, &pod) != nil || !exactBrowserPod(pod, b.pod, uid) {
		return pod, ErrTransport
	}
	return pod, nil
}
func browserTermination(pod core.Pod, profile browsercheck.Profile) (core.ContainerStateTerminated, string, bool, error) {
	if pod.Status.Phase == core.PodPending || pod.Status.Phase == core.PodRunning {
		return core.ContainerStateTerminated{}, "", false, nil
	}
	if pod.Spec.NodeName != profile.NodeName || pod.Status.Phase != core.PodSucceeded || len(pod.Status.ContainerStatuses) != 1 {
		return core.ContainerStateTerminated{}, "", false, ErrTransport
	}
	s := pod.Status.ContainerStatuses[0]
	if s.Name != "browser" || s.RestartCount != 0 || s.State.Terminated == nil || s.State.Terminated.ExitCode != 0 || s.State.Terminated.Signal != 0 || s.State.Terminated.StartedAt.IsZero() || s.State.Terminated.FinishedAt.Before(&s.State.Terminated.StartedAt) || imageSuffix(s.ImageID) != profile.RuntimeDigest || s.ContainerID == "" || s.State.Terminated.ContainerID != s.ContainerID {
		return core.ContainerStateTerminated{}, "", false, ErrTransport
	}
	return *s.State.Terminated, s.ContainerID, true, nil
}

// cleanup observes and deletes only the acknowledged Pod UID in the original
// namespace. It can outlive the result deadline for containment, but cannot
// create a success receipt after that deadline or adopt a replacement resource.
// Its caller also caps this independent containment by the original claim expiry.
func (b *BrowserResources) cleanupScope(ctx context.Context) error {
	var cluster, namespace core.Namespace
	if ctx.Err() != nil || b.read(ctx, "/api/v1/namespaces/kube-system", &cluster) != nil || string(cluster.UID) != b.profile.ClusterUID || b.read(ctx, "/api/v1/namespaces/"+browsercheck.Namespace, &namespace) != nil || string(namespace.UID) != b.profile.NamespaceUID {
		return ErrTransport
	}
	return nil
}
func (b *BrowserResources) cleanup(ctx context.Context, uid string, check func(context.Context) error) error {
	if check == nil || !resourceUID.MatchString(uid) || check(ctx) != nil || b.cleanupScope(ctx) != nil {
		return ErrTransport
	}
	raw, status, e := b.resources.request(ctx, http.MethodGet, "pods", b.pod.Name, nil, true)
	if e != nil {
		return e
	}
	if status == 404 {
		if b.cleanupScope(ctx) != nil {
			return ErrTransport
		}
		return check(ctx)
	}
	var pod core.Pod
	if status != 200 || authority.DecodeStrict(raw, &pod) != nil || string(pod.UID) != uid || pod.Name != b.pod.Name || pod.Namespace != browsercheck.Namespace {
		return ErrTransport
	}
	original := types.UID(uid)
	zero := int64(0)
	body, _ := json.Marshal(meta.DeleteOptions{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "DeleteOptions"}, Preconditions: &meta.Preconditions{UID: &original}, GracePeriodSeconds: &zero})
	// Reobserve identity after potentially blocking reads; gate immediately before DELETE.
	if b.cleanupScope(ctx) != nil || check(ctx) != nil {
		return ErrTransport
	}
	_, status, e = b.resources.request(ctx, http.MethodDelete, "pods", b.pod.Name, body, true)
	if e != nil || (status != 200 && status != 202 && status != 404) {
		return ErrTransport
	}
	for {
		if check(ctx) != nil || b.cleanupScope(ctx) != nil {
			return ErrTransport
		}
		raw, status, e = b.resources.request(ctx, http.MethodGet, "pods", b.pod.Name, nil, true)
		if e != nil {
			return e
		}
		if status == 404 {
			if b.cleanupScope(ctx) != nil {
				return ErrTransport
			}
			return check(ctx)
		}
		if status != 200 || authority.DecodeStrict(raw, &pod) != nil || string(pod.UID) != uid {
			return ErrTransport
		}
		select {
		case <-ctx.Done():
			return ErrTransport
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *BrowserResources) Verify(ctx context.Context, snapshot ConsoleSnapshot, route ConsoleRollout, gate browsercheck.Gate) (ConsoleRollout, browsercheck.Evidence, error) {
	var evidence browsercheck.Evidence
	if b == nil || gate == nil || snapshot.TargetSHA256 != b.request.TargetSHA256 {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	cleanupParent := gate.CleanupContext()
	cleanupUntil := gate.CleanupDeadline()
	if cleanupParent == nil || cleanupParent.Err() != nil || cleanupUntil.Before(time.UnixMilli(b.request.ExpiresAt)) || cleanupUntil.After(time.UnixMilli(b.console.target.ExpiresAt)) {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	rawSnapshot, _ := json.Marshal(snapshot)
	if authority.Digest(rawSnapshot) != b.request.SnapshotSHA256 {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(b.request.ExpiresAt))
	defer cancel()
	before, ready, e := b.console.ObserveRollout(ctx, snapshot)
	if e != nil || !ready || !sameBrowserRoute(before, route) || b.scope(ctx) != nil || b.empty(ctx) != nil {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	if gate.BeforeCreate(ctx, BrowserPodDigest(b.pod)) != nil || b.scope(ctx) != nil || b.empty(ctx) != nil {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	body, _ := json.Marshal(b.pod)
	raw, status, e := b.resources.request(ctx, http.MethodPost, "pods", "", body, false)
	var created core.Pod
	if e != nil || status != 201 || authority.DecodeStrict(raw, &created) != nil || !exactMetadata(created.ObjectMeta, b.pod.ObjectMeta, "") {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	uid := string(created.UID)
	cleanupCheck := func(c context.Context) error {
		if c.Err() != nil || cleanupParent.Err() != nil || !time.Now().Before(cleanupUntil) {
			return ErrTransport
		}
		return nil
	}
	cleanupAttempted := false
	defer func() {
		if !cleanupAttempted {
			cleanupCtx, stop := context.WithDeadline(cleanupParent, minDeadline(time.Now().Add(browsercheck.CleanupBudget), cleanupUntil))
			defer stop()
			_ = b.cleanup(cleanupCtx, uid, cleanupCheck)
		}
	}()
	if !exactBrowserPod(created, b.pod, uid) || gate.AcknowledgePod(ctx, b.request.RequestID, uid) != nil {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	var terminated core.ContainerStateTerminated
	var identity string
	for {
		if b.scope(ctx) != nil {
			return ConsoleRollout{}, evidence, ErrTransport
		}
		pod, e := b.observePod(ctx, uid)
		if e != nil {
			return ConsoleRollout{}, evidence, ErrTransport
		}
		var done bool
		terminated, identity, done, e = browserTermination(pod, b.profile)
		if e != nil {
			return ConsoleRollout{}, evidence, ErrTransport
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return ConsoleRollout{}, evidence, ErrTransport
		case <-time.After(250 * time.Millisecond):
		}
	}
	raw, status, e = b.resources.request(ctx, http.MethodGet, "pods", b.pod.Name+"/log?container=browser&limitBytes=4097", nil, false)
	if e != nil || status != 200 {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	// Kubernetes metav1.Time serializes container timestamps to whole seconds.
	// Bind the browser millisecond timestamp to that exact one-second bucket.
	result, e := browsercheck.ParseResult(raw, b.request, time.Now())
	if e != nil || result.StartedAt < terminated.StartedAt.UnixMilli() || result.FinishedAt >= terminated.FinishedAt.Add(time.Second).UnixMilli() {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	again, e := b.observePod(ctx, uid)
	if e != nil {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	final, finalID, done, e := browserTermination(again, b.profile)
	if e != nil || !done || finalID != identity || final != terminated || b.scope(ctx) != nil {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	after, ready, e := b.console.ObserveRollout(ctx, snapshot)
	if e != nil || !ready || !sameBrowserRoute(before, after) {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	cleanupAttempted = true
	cleanupCtx, stop := context.WithDeadline(cleanupParent, minDeadline(time.Now().Add(browsercheck.CleanupBudget), cleanupUntil))
	e = b.cleanup(cleanupCtx, uid, cleanupCheck)
	stop()
	if e != nil || ctx.Err() != nil || b.scope(ctx) != nil || b.empty(ctx) != nil {
		return ConsoleRollout{}, evidence, ErrTransport
	}
	evidence = browsercheck.Evidence{ProfileSHA256: b.profile.Digest(), Request: b.request, Result: result, PodUID: uid, ContainerID: identity, RuntimeDigest: b.profile.RuntimeDigest, DeletedAt: time.Now().UnixMilli()}
	if evidence.Validate(b.profile, time.Now()) != nil || gate.RecordEvidence(ctx, evidence) != nil {
		return ConsoleRollout{}, browsercheck.Evidence{}, ErrTransport
	}
	return after, evidence, nil
}

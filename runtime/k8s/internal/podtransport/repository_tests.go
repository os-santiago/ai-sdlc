package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	core "k8s.io/api/core/v1"
)

// VerifierNamespace is the only namespace repository test pods run in: its
// single deny-all policy is checked before every effect.
const VerifierNamespace = "ai-sdlc-verifiers"

// TestsRun is one compiled repository-tests run: its settings, its pod and
// the exact source tree it may import. It is not authority: the caller fences
// every effect with current signed ownership and a durable effect ledger.
// No method is retried; an uncertain effect fails the whole verification.
type TestsRun struct {
	resources *Resources
	run       string
	sourceSHA string
	treeSHA   string
	source    []byte
	timeout   time.Duration
	tests     string // container ID of the tests container seen by Prepare
}

// PlanTestsRun compiles the run and checks, before any effect, that source is
// a valid bundle of exactly the pod's source revision and tree.
func PlanTestsRun(client *Client, p repositorytests.Pod, source []byte, expires time.Time) (*TestsRun, error) {
	if client == nil || client.http == nil || client.config == nil || p.Namespace != VerifierNamespace ||
		len(source) == 0 || len(source) > workspacebundle.MaxWireBytes || !time.Now().Before(expires) {
		return nil, ErrTransport
	}
	source = append([]byte(nil), source...)
	bundle, err := workspacebundle.ReadVerified(bytes.NewReader(source), workspacebundle.Digest(source), p.SourceSHA)
	if err != nil {
		return nil, ErrTransport
	}
	if tree, err := bundle.TreeDigest(); err != nil || tree != p.TreeSHA256 {
		return nil, ErrTransport
	}
	w, err := repositorytests.CompilePod(p)
	if err != nil {
		return nil, ErrTransport
	}
	t := &TestsRun{resources: &Resources{client: client, binding: Binding{Namespace: p.Namespace}, expires: expires},
		run: p.Run, sourceSHA: p.SourceSHA, treeSHA: p.TreeSHA256, source: source, timeout: time.Duration(p.Verification.TimeoutSeconds) * time.Second}
	config, _ := json.Marshal(w.ConfigMap)
	pod, _ := json.Marshal(w.Pod)
	if authority.DecodeStrict(config, &t.resources.config) != nil || authority.DecodeStrict(pod, &t.resources.pod) != nil {
		return nil, ErrTransport
	}
	return t, nil
}

// Names returns the compiled settings and pod names, for the effect ledger.
func (t *TestsRun) Names() (config, pod string) {
	return t.resources.config.Name, t.resources.pod.Name
}

func (t *TestsRun) CreateConfig(ctx context.Context) (string, error) {
	if t == nil || t.resources.isolation(ctx) != nil {
		return "", ErrTransport
	}
	return t.resources.CreateConfig(ctx)
}

func (t *TestsRun) CreatePod(ctx context.Context, configUID string) (string, error) {
	if t == nil || t.resources.isolation(ctx) != nil {
		return "", ErrTransport
	}
	return t.resources.CreatePod(ctx, configUID)
}

func (t *TestsRun) DeletePod(ctx context.Context, uid string) error {
	if t == nil {
		return ErrTransport
	}
	return t.resources.DeletePod(ctx, uid)
}

func (t *TestsRun) DeleteConfig(ctx context.Context, uid string) error {
	if t == nil {
		return ErrTransport
	}
	return t.resources.DeleteConfig(ctx, uid)
}

// testsObservation is one exact read of the pod.
type testsObservation struct {
	phase      core.PodPhase
	transfer   string // container ID of the running sidecar, or ""
	tests      string // container ID of the tests container, or ""
	testsState string // "waiting", "running" or "completed"
}

// observe reads the pod by its compiled name and checks the exact template,
// UID and every container status: the init copy completed once, the sidecar
// never restarted, and the tests container is waiting, running or completed
// with exit code 0. Anything else (a failed run included) is an error.
func (t *TestsRun) observe(ctx context.Context, uid string) (testsObservation, error) {
	r := t.resources
	raw, status, err := r.request(ctx, http.MethodGet, "pods", r.pod.Name, nil, false)
	if err != nil {
		return testsObservation{}, err
	}
	var pod core.Pod
	if status != 200 || authority.DecodeStrict(raw, &pod) != nil || !exactPodTemplate(pod, r.pod, uid) || len(pod.Status.EphemeralContainerStatuses) != 0 ||
		len(pod.Status.InitContainerStatuses) > 2 || len(pod.Status.ContainerStatuses) > 1 {
		return testsObservation{}, ErrTransport
	}
	o := testsObservation{phase: pod.Status.Phase, testsState: "waiting"}
	if o.phase != core.PodPending && o.phase != core.PodRunning && o.phase != core.PodSucceeded {
		return testsObservation{}, ErrTransport
	}
	images := map[string]string{}
	for _, c := range append(append([]core.Container{}, r.pod.Spec.InitContainers...), r.pod.Spec.Containers...) {
		images[c.Name] = c.Image
	}
	seen := map[string]bool{}
	for _, s := range append(append([]core.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		pin, known := images[s.Name]
		if !known || seen[s.Name] || s.RestartCount != 0 || s.LastTerminationState.Terminated != nil || s.LastTerminationState.Running != nil ||
			s.LastTerminationState.Waiting != nil || len(s.Image) > 1024 || len(s.ImageID) > 1024 {
			return testsObservation{}, ErrTransport
		}
		seen[s.Name] = true
		if s.ImageID != "" && (!imageIDPattern.MatchString(s.ImageID) || s.ImageID[strings.LastIndex(s.ImageID, "@")+1:] != pin[strings.LastIndex(pin, "@")+1:]) {
			return testsObservation{}, ErrTransport
		}
		started := s.ContainerID != ""
		if started && (!containerIDPattern.MatchString(s.ContainerID) || s.ImageID == "") {
			return testsObservation{}, ErrTransport
		}
		waiting := s.State.Waiting != nil && s.State.Running == nil && s.State.Terminated == nil && !started &&
			(s.State.Waiting.Reason == "ContainerCreating" || s.State.Waiting.Reason == "PodInitializing")
		running := s.State.Running != nil && s.State.Waiting == nil && s.State.Terminated == nil && started
		completed := s.State.Terminated != nil && s.State.Running == nil && s.State.Waiting == nil && started &&
			s.State.Terminated.ExitCode == 0 && s.State.Terminated.ContainerID == s.ContainerID
		switch s.Name {
		case "install-runner":
			if !waiting && !completed {
				return testsObservation{}, ErrTransport
			}
		case "transfer":
			// The sidecar is stopped (exit 0 on SIGTERM) once the tests end.
			if !waiting && !running && !(completed && o.phase == core.PodSucceeded) {
				return testsObservation{}, ErrTransport
			}
			if running {
				o.transfer = s.ContainerID
			}
		case "tests":
			switch {
			case running:
				o.tests, o.testsState = s.ContainerID, "running"
			case completed:
				o.tests, o.testsState = s.ContainerID, "completed"
			case !waiting:
				return testsObservation{}, ErrTransport
			}
		}
	}
	if o.phase == core.PodSucceeded && o.testsState != "completed" {
		return testsObservation{}, ErrTransport
	}
	return o, nil
}

func (t *TestsRun) poll(ctx context.Context, uid string, deadline time.Time, done func(testsObservation) bool) (testsObservation, error) {
	ctx, cancel := context.WithDeadline(ctx, minDeadline(deadline, t.resources.expires))
	defer cancel()
	for {
		o, err := t.observe(ctx, uid)
		if err != nil {
			return testsObservation{}, err
		}
		if done(o) {
			return o, nil
		}
		select {
		case <-ctx.Done():
			return testsObservation{}, ErrTransport
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Prepare waits until the sidecar and the tests container run (the runner
// is then waiting for its marker), imports the exact source tree through
// the sidecar and publishes the ready marker. The same sidecar and tests
// container instances must be observed before and after.
func (t *TestsRun) Prepare(ctx context.Context, uid string) error {
	if t == nil || !resourceUID.MatchString(uid) || t.tests != "" {
		return ErrTransport
	}
	started := time.Now()
	before, err := t.poll(ctx, uid, started.Add(repositorytests.PullSeconds*time.Second), func(o testsObservation) bool {
		return o.phase == core.PodRunning && o.transfer != "" && o.testsState == "running"
	})
	if err != nil {
		return err
	}
	// The runner waits ReadySeconds from its own start; import and ready
	// must finish well within that.
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add((repositorytests.ReadySeconds-30)*time.Second), t.resources.expires))
	defer cancel()
	session := &Session{client: t.resources.client, expected: t.resources.pod, identity: modelmailbox.Identity{Attempt: "repository-tests", PodUID: uid}, expires: t.resources.expires, helper: "transfer"}
	digest := workspacebundle.Digest(t.source)
	raw, code, err := session.exchangeFixed(ctx, helperOperation{
		command:    []string{"/workspace-transfer", "import", "--pod-uid", uid, "--source-sha", t.sourceSHA, "--digest", digest},
		inputLimit: workspacebundle.MaxWireBytes, outputLimit: 2048, frames: 1024, timeout: 30 * time.Second, stdin: true,
	}, t.source)
	if err != nil || code != 0 {
		return ErrTransport
	}
	bundle, _ := workspacebundle.Decode(t.source)
	var receipt struct {
		Imported     *bool   `json:"imported"`
		Files        *int    `json:"files"`
		SourceSHA    *string `json:"sourceSHA"`
		BundleSHA256 *string `json:"bundleSHA256"`
	}
	if authority.DecodeStrict(raw, &receipt) != nil || receipt.Imported == nil || !*receipt.Imported || receipt.Files == nil || *receipt.Files != len(bundle.Files) ||
		receipt.SourceSHA == nil || *receipt.SourceSHA != t.sourceSHA || receipt.BundleSHA256 == nil || *receipt.BundleSHA256 != digest {
		return ErrTransport
	}
	if after, err := t.observe(ctx, uid); err != nil || after.transfer != before.transfer || after.tests != before.tests || after.testsState != "running" {
		return ErrTransport
	}
	raw, code, err = session.exchangeFixed(ctx, helperOperation{
		command: []string{"/workspace-transfer", "ready", "--pod-uid", uid, "--source-sha", t.sourceSHA, "--tree-sha256", t.treeSHA},
		frames:  1024, timeout: 60 * time.Second,
	}, nil)
	if err != nil || code != 0 || len(raw) != 0 {
		return ErrTransport
	}
	// From here on the repository's code controls /workspaces: no further
	// helper runs in this pod.
	if after, err := t.observe(ctx, uid); err != nil || after.tests != before.tests || (after.testsState == "running" && after.transfer != before.transfer) {
		return ErrTransport
	}
	t.tests = before.tests
	return nil
}

// ReadResult waits for the pod to succeed (the runner exits 0 only after it
// wrote its result), then reads the tests container's bounded log: exactly
// one result line from the same container instance Prepare observed.
func (t *TestsRun) ReadResult(ctx context.Context, uid string) (repositorytests.Run, []byte, error) {
	if t == nil || t.tests == "" || !resourceUID.MatchString(uid) {
		return repositorytests.Run{}, nil, ErrTransport
	}
	deadline := time.Now().Add(t.timeout + repositorytests.MarginSeconds*time.Second)
	if _, err := t.poll(ctx, uid, deadline, func(o testsObservation) bool { return o.phase == core.PodSucceeded }); err != nil {
		return repositorytests.Run{}, nil, err
	}
	r := t.resources
	limit := int64(repositorytests.MaxResultBytes + 1)
	raw, err := r.fetch(ctx, r.url("pods", r.pod.Name)+"/log?container=tests&limitBytes="+url.QueryEscape(itoa(limit+1)), "*/*", limit)
	if err != nil {
		return repositorytests.Run{}, nil, err
	}
	after, err := t.observe(ctx, uid)
	if err != nil || after.phase != core.PodSucceeded || after.tests != t.tests {
		return repositorytests.Run{}, nil, ErrTransport
	}
	line, ok := bytes.CutSuffix(raw, []byte("\n"))
	if !ok || bytes.ContainsAny(line, "\r\n") {
		return repositorytests.Run{}, nil, ErrTransport
	}
	run, err := repositorytests.ParseRun(line)
	if err != nil {
		return repositorytests.Run{}, nil, ErrTransport
	}
	return run, append([]byte(nil), line...), nil
}

func itoa(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
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
	analysis  time.Duration // the run's analysis commands, 0 without
	runs      int
	imported  bool
	tests     string // container ID of the tests container seen by Import
}

// PlanTestsRun compiles the run and checks, before any effect, that source is
// a valid bundle of exactly the pod's source revision and tree.
func PlanTestsRun(client *Client, p repositorytests.Pod, admission RuntimeClassAdmission, source []byte, expires time.Time) (*TestsRun, error) {
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
	t := &TestsRun{resources: &Resources{client: client, binding: Binding{Namespace: p.Namespace, RuntimeClass: p.RuntimeClass, Admission: admission}, expires: expires},
		run: p.Run, sourceSHA: p.SourceSHA, treeSHA: p.TreeSHA256, source: source, runs: repositorytests.Repetitions(p.Run, p.Verification),
		timeout: time.Duration(p.Verification.TimeoutSeconds) * time.Second, analysis: time.Duration(repositorytests.AnalysisSeconds(p.Run, p.Verification)) * time.Second}
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
	return t.resources.CreatePodObserved(ctx, configUID)
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
	raw, status, err := r.readPod(ctx)
	if err != nil {
		return testsObservation{}, testsFailure(requestStep("observe", err), err)
	}
	if status != 200 {
		return testsObservation{}, testsFailure("observe-status", ErrTransport)
	}
	var pod core.Pod
	if authority.DecodeStrict(raw, &pod) != nil || !exactPodTemplate(pod, r.pod, uid, r.binding.Admission) || len(pod.Status.EphemeralContainerStatuses) != 0 ||
		len(pod.Status.InitContainerStatuses) > 2 || len(pod.Status.ContainerStatuses) > 1 {
		return testsObservation{}, testsFailure("observe-template", ErrTransport)
	}
	o := testsObservation{phase: pod.Status.Phase, testsState: "waiting"}
	if o.phase != core.PodPending && o.phase != core.PodRunning && o.phase != core.PodSucceeded {
		return testsObservation{}, testsFailure("observe-phase", ErrTransport)
	}
	images := map[string]string{}
	for _, c := range append(append([]core.Container{}, r.pod.Spec.InitContainers...), r.pod.Spec.Containers...) {
		images[c.Name] = c.Image
	}
	seen := map[string]bool{}
	transferStopped := false
	for _, s := range append(append([]core.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		pin, known := images[s.Name]
		if !known || seen[s.Name] || s.RestartCount != 0 || s.LastTerminationState.Terminated != nil || s.LastTerminationState.Running != nil ||
			s.LastTerminationState.Waiting != nil || len(s.Image) > 1024 || len(s.ImageID) > 1024 {
			return testsObservation{}, testsFailure("observe-container", ErrTransport)
		}
		seen[s.Name] = true
		if s.ImageID != "" && (!imageIDPattern.MatchString(s.ImageID) || !imageMatchesPin(s.Image, s.ImageID, pin)) {
			return testsObservation{}, testsFailure("observe-container", ErrTransport)
		}
		// A container the runtime created but has not started is reported
		// waiting with its ID and no reason (see createdStatus); it counts
		// as waiting. Every other container with an ID has a pinned image ID.
		created := createdStatus(s)
		started := s.ContainerID != "" && !created
		if started && (!containerIDPattern.MatchString(s.ContainerID) || s.ImageID == "") {
			return testsObservation{}, testsFailure("observe-container", ErrTransport)
		}
		waiting := created || s.State.Waiting != nil && s.State.Running == nil && s.State.Terminated == nil && s.ContainerID == "" &&
			(s.State.Waiting.Reason == "ContainerCreating" || s.State.Waiting.Reason == "PodInitializing")
		running := s.State.Running != nil && s.State.Waiting == nil && s.State.Terminated == nil && started
		completed := s.State.Terminated != nil && s.State.Running == nil && s.State.Waiting == nil && started &&
			s.State.Terminated.ExitCode == 0 && s.State.Terminated.ContainerID == s.ContainerID
		switch s.Name {
		case "install-runner":
			if !waiting && !completed && !(running && o.phase == core.PodPending) {
				return testsObservation{}, testsFailure("observe-container", ErrTransport)
			}
		case "transfer":
			// The kubelet stops the sidecar once the tests end, possibly before
			// the phase changes; its exit then no longer matters.
			stopped := s.State.Terminated != nil && s.State.Running == nil && s.State.Waiting == nil && started
			if !waiting && !running && !stopped {
				return testsObservation{}, testsFailure("observe-container", ErrTransport)
			}
			if running {
				o.transfer = s.ContainerID
			} else if stopped {
				transferStopped = true
			}
		case "tests":
			switch {
			case running:
				o.tests, o.testsState = s.ContainerID, "running"
			case completed:
				o.tests, o.testsState = s.ContainerID, "completed"
			case !waiting:
				return testsObservation{}, testsFailure("observe-container", ErrTransport)
			}
		}
	}
	if (o.phase == core.PodSucceeded || transferStopped) && o.testsState != "completed" {
		return testsObservation{}, testsFailure("observe-progress", ErrTransport)
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

// TestsStatus is one non-blocking observation of a run's pod.
type TestsStatus struct {
	Ready     bool // the sidecar and the tests container run: import may start
	Succeeded bool // the tests container completed and the pod succeeded
}

// Status observes the pod once. Any state outside the expected progression
// (a failed container, a restart, a changed template) is an error.
func (t *TestsRun) Status(ctx context.Context, uid string) (TestsStatus, error) {
	if t == nil || !resourceUID.MatchString(uid) {
		return TestsStatus{}, testsFailure("observe-uid", ErrTransport)
	}
	o, err := t.observe(ctx, uid)
	if err != nil {
		return TestsStatus{}, err
	}
	return TestsStatus{Ready: o.phase == core.PodRunning && o.transfer != "" && o.testsState == "running", Succeeded: o.phase == core.PodSucceeded}, nil
}

// Prepare waits until the pod is ready, then imports (see Import).
func (t *TestsRun) Prepare(ctx context.Context, uid string) error {
	if t == nil || !resourceUID.MatchString(uid) || t.tests != "" {
		return ErrTransport
	}
	if _, err := t.poll(ctx, uid, time.Now().Add(repositorytests.PullSeconds*time.Second), func(o testsObservation) bool {
		return o.phase == core.PodRunning && o.transfer != "" && o.testsState == "running"
	}); err != nil {
		return err
	}
	return t.Import(ctx, uid)
}

// Import requires a ready pod (the runner is then waiting for its marker),
// imports the exact source tree through the sidecar and publishes the ready
// marker. The same sidecar and tests container instances must be observed
// before and after. It runs at most once per run.
func (t *TestsRun) Import(ctx context.Context, uid string) error {
	if t == nil || !resourceUID.MatchString(uid) || t.tests != "" || t.imported {
		return testsFailure("import-repeat", ErrTransport)
	}
	t.imported = true
	before, err := t.observe(ctx, uid)
	if err != nil {
		return importFailure("import-before", err)
	}
	if before.phase != core.PodRunning || before.transfer == "" || before.testsState != "running" {
		return testsFailure("import-before-state", ErrTransport)
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
	if err != nil {
		return testsFailure(exchangeStep("import-transfer", ctx, err), ErrTransport)
	}
	if code != 0 {
		return testsFailure("import-exit", ErrTransport)
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
		return testsFailure("import-receipt", ErrTransport)
	}
	if after, err := t.observe(ctx, uid); err != nil {
		return importFailure("import-after", err)
	} else if after.transfer != before.transfer || after.tests != before.tests || after.testsState != "running" {
		return testsFailure("import-after-state", ErrTransport)
	}
	raw, code, err = session.exchangeFixed(ctx, helperOperation{
		command: []string{"/workspace-transfer", "ready", "--pod-uid", uid, "--source-sha", t.sourceSHA, "--tree-sha256", t.treeSHA},
		frames:  1024, timeout: 60 * time.Second,
	}, nil)
	if err != nil {
		return testsFailure(exchangeStep("ready-transfer", ctx, err), ErrTransport)
	}
	if code != 0 || len(raw) != 0 {
		return testsFailure("ready-exit", ErrTransport)
	}
	// From here on the repository's code controls /workspaces: no further
	// helper runs in this pod.
	if after, err := t.observe(ctx, uid); err != nil {
		return importFailure("ready-after", err)
	} else if after.tests != before.tests || (after.testsState == "running" && after.transfer != before.transfer) {
		return testsFailure("ready-after-state", ErrTransport)
	}
	t.tests = before.tests
	return nil
}

// ReadResult waits for the pod to succeed, then collects (see Collect).
func (t *TestsRun) ReadResult(ctx context.Context, uid string) ([]repositorytests.Run, []byte, error) {
	if t == nil || t.tests == "" || !resourceUID.MatchString(uid) {
		return nil, nil, ErrTransport
	}
	deadline := time.Now().Add(time.Duration(t.runs)*t.timeout + t.analysis + repositorytests.MarginSeconds*time.Second)
	if _, err := t.poll(ctx, uid, deadline, func(o testsObservation) bool { return o.phase == core.PodSucceeded }); err != nil {
		return nil, nil, err
	}
	return t.Collect(ctx, uid)
}

// Collect requires a succeeded pod (the runner exits 0 only after it wrote
// every result) and reads the tests container's bounded log: exactly one
// result line per repetition, from the same container instance Import
// observed. It returns the runs and the exact log bytes for the proof.
func (t *TestsRun) Collect(ctx context.Context, uid string) ([]repositorytests.Run, []byte, error) {
	if t == nil || t.tests == "" || !resourceUID.MatchString(uid) {
		return nil, nil, ErrTransport
	}
	if o, err := t.observe(ctx, uid); err != nil || o.phase != core.PodSucceeded || o.tests != t.tests {
		return nil, nil, ErrTransport
	}
	r := t.resources
	limit := int64(t.runs * (repositorytests.MaxResultBytes + 1))
	raw, err := r.fetch(ctx, r.url("pods", r.pod.Name)+"/log?container=tests&limitBytes="+url.QueryEscape(itoa(limit+1)), "*/*", limit)
	if err != nil {
		return nil, nil, err
	}
	after, err := t.observe(ctx, uid)
	if err != nil || after.phase != core.PodSucceeded || after.tests != t.tests {
		return nil, nil, ErrTransport
	}
	body, ok := bytes.CutSuffix(raw, []byte("\n"))
	lines := bytes.Split(body, []byte("\n"))
	if !ok || len(lines) != t.runs || bytes.ContainsRune(body, '\r') {
		return nil, nil, ErrTransport
	}
	runs := make([]repositorytests.Run, 0, t.runs)
	for _, line := range lines {
		run, err := repositorytests.ParseRun(line)
		if err != nil {
			return nil, nil, ErrTransport
		}
		runs = append(runs, run)
	}
	return runs, append([]byte(nil), raw...), nil
}

func itoa(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// testsError names the bounded step of a repository-tests observation or
// import that failed. It unwraps to the original error (ErrTransport or
// ErrPermission), so every caller stops exactly as before; the step only
// refines the verification stop diagnostic.
type testsError struct {
	step  string
	cause error
}

func (e testsError) Error() string { return "repository tests stopped at " + e.step }
func (e testsError) Unwrap() error { return e.cause }

func testsFailure(step string, err error) error { return testsError{step: step, cause: err} }

// importFailure prefixes the failed observation step with the import step it
// belongs to, for example import-before-observe-request. Import has always
// stopped with ErrTransport, whatever the observation's cause.
func importFailure(step string, err error) error {
	if observed := TestsStep(err); observed != "" {
		step += "-" + observed
	}
	return testsError{step: step, cause: ErrTransport}
}

// exchangeStep names a failed helper exchange: denied, timed out (its own
// bound or the caller's deadline) or another transport failure.
func exchangeStep(step string, ctx context.Context, err error) string {
	switch {
	case errors.Is(err, ErrPermission):
		return step + "-permission"
	case ctx.Err() != nil:
		return step + "-timeout"
	}
	return step + "-request"
}

var testsSteps = regexp.MustCompile(`^((import-before|import-after|ready-after)-)?observe-(uid|request|permission|status|template|phase|container|progress)$|^(import-(repeat|before-state|after-state|exit|receipt)|ready-(exit|after-state)|(import|ready)-transfer-(request|permission|timeout))$`)

// TestsStep returns the failed repository-tests step, or "" for any other
// error.
func TestsStep(err error) string {
	var failed testsError
	if errors.As(err, &failed) && testsSteps.MatchString(failed.step) {
		return failed.step
	}
	return ""
}

// KnownTestsStep reports whether step is one TestsStep can return.
func KnownTestsStep(step string) bool { return testsSteps.MatchString(step) }

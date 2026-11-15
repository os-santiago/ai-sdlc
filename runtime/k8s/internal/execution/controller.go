package execution

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// ControllerError deliberately excludes provider bodies, credentials and source.
type ControllerError struct{ Stage string }

var errWorkerNonzero = errors.New("original worker exited nonzero")

func (e ControllerError) Error() string { return "execution controller stopped: " + e.Stage }

// DiagnosticStage returns only fixed operational categories. Never log an
// arbitrary underlying error, source text, model output or credential.
func DiagnosticStage(err error) string {
	var stopped ControllerError
	if !errors.As(err, &stopped) {
		return "authority-or-storage"
	}
	switch stopped.Stage {
	case "configuration", "ownership", "resources", "config", "pod", "readiness", "source", "model", "start", "execution", "worker-exit", "export", "capture", "cleanup":
		return stopped.Stage
	}
	if strings.HasPrefix(stopped.Stage, "model-") {
		return "model-" + modelbroker.SafeFailureStage(strings.TrimPrefix(stopped.Stage, "model-"))
	}
	return "unknown"
}

// RunController composes a freshly prepared, signed claim into one isolated
// candidate. Only trusted bootstrap code may supply dependencies. It does not
// accept work from HTTP, create claims, retry/adopt executions, validate code or
// authorize publication. A captured candidate remains unvalidated durable data.
func RunController(ctx context.Context, db *store.DB, load LifecycleLoader, prepared Prepared, client *podtransport.Client, provider *modelbroker.Provider, model ModelPolicyLoader) (record store.CandidateRecord, err error) {
	if db == nil || load == nil || client == nil || provider == nil || model == nil {
		return record, ControllerError{"configuration"}
	}
	stage := "ownership"
	var resources *podtransport.Resources
	var l *Lifecycle
	var configUID, podUID string
	// Cleanup needs its own bounded context even after authority expires. Delete
	// only acknowledged UIDs; an uncertain create is never adopted by name.
	defer func() {
		if recover() != nil {
			err = ControllerError{stage}
		}
		if resources != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 25*time.Second)
			defer stop()
			failed := false
			if podUID != "" {
				failed = resources.DeletePod(cleanup, podUID) != nil
			}
			if configUID != "" && resources.DeleteConfig(cleanup, configUID) != nil {
				failed = true
			}
			if failed {
				err = ControllerError{"cleanup"}
			}
		}
		if err != nil {
			record = store.CandidateRecord{}
			if l != nil {
				stopLifecycleClaim(db, l.durable.Claim)
			} else {
				stopLifecycleClaim(db, prepared.Claim())
			}
		}
	}()
	l, err = OpenLifecycle(ctx, db, load, prepared)
	if err != nil {
		return record, ControllerError{stage}
	}
	ctx, cancel := context.WithDeadline(ctx, l.durable.Claim.Until)
	defer cancel()
	stage = "resources"
	resources, err = podtransport.PlanResources(client, podtransport.Binding{Namespace: l.placement.Namespace, WorkerImage: l.placement.WorkerImage, MailboxImage: l.placement.MailboxImage, Assignment: prepared.Assignment(), Profile: prepared.Profile(), ADev: prepared.ADev()})
	if err != nil {
		return record, ControllerError{stage}
	}
	stage = "config"
	if err = l.RunEffect(ctx, stage, func(ctx context.Context, _ EffectInput) (store.LifecycleResult, error) {
		var e error
		configUID, e = resources.CreateConfig(ctx)
		return store.LifecycleResult{UID: configUID}, e
	}); err != nil {
		return record, ControllerError{stage}
	}
	stage = "pod"
	if err = l.RunEffect(ctx, stage, func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		var e error
		podUID, e = resources.CreatePod(ctx, in.ConfigUID)
		return store.LifecycleResult{UID: podUID}, e
	}); err != nil {
		return record, ControllerError{stage}
	}
	stage = "readiness"
	session, err := resources.WaitRunning(ctx, podUID)
	if err != nil {
		return record, ControllerError{stage}
	}
	stage = "source"
	if err = l.RunEffect(ctx, stage, func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		e := session.ImportSource(ctx, in.Source)
		return store.LifecycleResult{UID: in.PodUID, Digest: workspacebundle.Digest(in.Source), Bytes: len(in.Source)}, e
	}); err != nil {
		return record, ControllerError{stage}
	}
	stage = "model"
	scope, err := l.ActivateModel(ctx, model)
	if err != nil {
		return record, ControllerError{stage}
	}
	stage = "start"
	if err = l.RunEffect(ctx, stage, func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		return store.LifecycleResult{UID: in.PodUID}, session.Start(ctx)
	}); err != nil {
		return record, ControllerError{stage}
	}
	stage = "execution"
	dispatcher := modelbroker.Dispatcher{DB: db, Provider: provider, Mailbox: session, Authority: l}
	if err = driveModel(ctx, session, func(ctx context.Context) error { _, e := l.Authorize(ctx, scope); return e }, func(ctx context.Context, request modelmailbox.Envelope) error {
		result, e := dispatcher.Deliver(ctx, scope.AllocationID, prepared.Profile(), request)
		if e != nil || !result.HandoffConfirmed {
			stage = "model-" + modelbroker.SafeFailureStage(result.FailureStage)
			return ErrAuthority
		}
		return nil
	}); err != nil {
		if errors.Is(err, errWorkerNonzero) {
			stage = "worker-exit"
		}
		return record, ControllerError{stage}
	}
	stage = "export"
	var exported podtransport.Candidate
	if err = l.RunEffect(ctx, stage, func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		var e error
		exported, e = session.ExportCandidate(ctx)
		code := exported.WorkerExitCode()
		raw := exported.Bytes()
		return store.LifecycleResult{UID: in.PodUID, Digest: workspacebundle.Digest(raw), Bytes: len(raw), ExitCode: &code}, e
	}); err != nil {
		return record, ControllerError{stage}
	}
	stage = "capture"
	record, err = l.CaptureCandidate(ctx, exported.Bytes())
	if err != nil {
		return store.CandidateRecord{}, ControllerError{stage}
	}
	return record, nil
}

func sameRequest(a, b modelmailbox.Envelope) bool {
	return a.Version == b.Version && a.RequestSHA256 == b.RequestSHA256 && a.Attempt == b.Attempt && a.PodUID == b.PodUID && a.ExpiresAt == b.ExpiresAt && a.Kind == b.Kind && a.Status == b.Status && a.Sequence == b.Sequence && modelmailbox.Digest(a.Payload) == modelmailbox.Digest(b.Payload)
}

type modelSession interface {
	WorkerExit(context.Context) (int32, bool, error)
	ReadRequest(context.Context) (modelmailbox.Envelope, error)
}

// driveModel waits for the original worker's exit rather than a prescribed
// response count. Every idle cycle rechecks authority. A duplicate observation
// is allowed only for the exact last request; sequence gaps/rebinding stop it.
func driveModel(ctx context.Context, session modelSession, authorize func(context.Context) error, deliver func(context.Context, modelmailbox.Envelope) error) error {
	var previous modelmailbox.Envelope
	for {
		if ctx.Err() != nil || authorize(ctx) != nil {
			return ErrAuthority
		}
		code, exited, err := session.WorkerExit(ctx)
		if err != nil {
			return ErrAuthority
		}
		if exited {
			if code != 0 {
				return errWorkerNonzero
			}
			return nil
		}
		request, err := session.ReadRequest(ctx)
		if err != nil && !errors.Is(err, modelmailbox.ErrPending) {
			if !errors.Is(err, podtransport.ErrWorkerExited) {
				return ErrAuthority
			}
			// An observed request at exit must already have a confirmed handoff.
			// Never hide a new request, sequence gap or substituted duplicate
			// behind the writer's zero exit. Pending/pre-read exit is empty.
			if request.Sequence != 0 && (previous.Sequence == 0 || !sameRequest(request, previous)) {
				return ErrAuthority
			}
			// The writer can exit between ReadRequest's before/after observations.
			// Export/capture still require zero exit, no outstanding model call and a
			// nonempty independently reconstructed delta. Never retry a failed delivery.
			code, exited, exitErr := session.WorkerExit(ctx)
			if exitErr == nil && exited && ctx.Err() == nil {
				if code != 0 {
					return errWorkerNonzero
				}
				return nil
			}
			return ErrAuthority
		}
		if err == nil {
			if request.Sequence == previous.Sequence && previous.Sequence != 0 {
				if !sameRequest(request, previous) {
					return ErrAuthority
				}
			} else {
				if request.Sequence != previous.Sequence+1 {
					return ErrAuthority
				}
				if deliver(ctx, request) != nil {
					return ErrAuthority
				}
				previous = request
				previous.Payload = append([]byte(nil), request.Payload...)
			}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ErrAuthority
		case <-timer.C:
		}
	}
}

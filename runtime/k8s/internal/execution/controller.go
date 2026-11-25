package execution

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// ControllerError deliberately excludes provider bodies, credentials and source.
type ControllerError struct{ Stage string }

var errWorkerNonzero = errors.New("original worker exited nonzero")

type nonzeroWorkerExit struct{ code int32 }

func (e nonzeroWorkerExit) Error() string { return "original worker exited nonzero" }
func (e nonzeroWorkerExit) Unwrap() error { return errWorkerNonzero }

type workerFailure struct {
	code     int32
	recorded bool
}

func (e workerFailure) Error() string { return "execution controller stopped: worker-exit" }
func (e workerFailure) Unwrap() error { return ControllerError{"worker-exit"} }

// WorkerFailureDiagnostic exposes only a finite exit code and persistence flag.
// Unknown failures remain code zero, never a claim of successful execution.
func WorkerFailureDiagnostic(err error) (int32, bool) {
	var failed workerFailure
	if errors.As(err, &failed) && failed.code >= 1 && failed.code <= 255 {
		return failed.code, failed.recorded
	}
	return 0, false
}

func (e ControllerError) Error() string { return "execution controller stopped: " + e.Stage }

// cleanupFailure preserves the original stop cause. Cleanup failure must not
// erase the stage that made a model call uncertain.
// step names the failed cleanup step for the diagnostic when there is no
// earlier cause, for example after a candidate was captured.
type cleanupFailure struct {
	cause error
	step  string
}

func (e cleanupFailure) Error() string { return "execution cleanup incomplete" }
func (e cleanupFailure) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	if e.step != "" {
		return ControllerError{e.step}
	}
	return ControllerError{"cleanup"}
}

// removalStage refines a cleanup step with the bounded removal step that
// failed, for example cleanup-pod-observe-timeout.
func removalStage(step string, err error) string {
	if detail := podtransport.RemovalStep(err); podtransport.KnownRemovalStep(detail) {
		return step + "-" + detail
	}
	return step
}

func CleanupPending(err error) bool { var failed cleanupFailure; return errors.As(err, &failed) }

// DiagnosticStage returns only fixed operational categories. Never log an
// arbitrary underlying error, source text, model output or credential.
func DiagnosticStage(err error) string {
	var stopped ControllerError
	if !errors.As(err, &stopped) {
		return "authority-or-storage"
	}
	if stopped.Stage == "sandbox-runtime-class" {
		if reason := podtransport.RuntimeClassStopReason(err); reason != "" {
			return stopped.Stage + "-" + reason
		}
	}
	switch stopped.Stage {
	case "source-preflight-authority", "sandbox-runtime-class", "source-rate-limited", "source-unavailable", "configuration", "ownership", "resources", "config", "pod", "readiness", "source", "model", "start", "execution", "execution-authority", "execution-authority-snapshot-stale", "execution-observe", "execution-read", "execution-sequence", "execution-exit-race", "worker-exit", "export", "capture", "cleanup", "cleanup-pod", "cleanup-config", "cleanup-confirm":
		return stopped.Stage
	}
	if strings.HasPrefix(stopped.Stage, "model-") {
		return "model-" + modelbroker.SafeFailureStage(strings.TrimPrefix(stopped.Stage, "model-"))
	}
	for _, step := range []string{"cleanup-pod-", "cleanup-config-"} {
		if detail, ok := strings.CutPrefix(stopped.Stage, step); ok && podtransport.KnownRemovalStep(detail) {
			return stopped.Stage
		}
	}
	if detail, ok := strings.CutPrefix(stopped.Stage, "readiness-"); ok && podtransport.KnownReadinessStep(detail) {
		return stopped.Stage
	}
	if detail, ok := strings.CutPrefix(stopped.Stage, "execution-read-"); ok && podtransport.KnownReadStep(detail) {
		return stopped.Stage
	}
	if detail, ok := strings.CutPrefix(stopped.Stage, "export-"); ok && podtransport.KnownExportStep(detail) {
		return stopped.Stage
	}
	return "unknown"
}

// RunController composes a freshly prepared, signed claim into one isolated
// candidate. Only trusted bootstrap code may supply dependencies. It does not
// accept work from HTTP, create claims, retry/adopt executions, validate code or
// authorize publication. A captured candidate remains unvalidated durable data.
// admission is what the preflight observed the sandbox RuntimeClass adds to
// each pod; the zero value is a class without overhead or scheduling.
func RunController(ctx context.Context, db *store.DB, load LifecycleLoader, prepared Prepared, client *podtransport.Client, admission podtransport.RuntimeClassAdmission, provider *modelbroker.Provider, model ModelPolicyLoader) (record store.CandidateRecord, err error) {
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
			step := ""
			if podUID != "" {
				if e := resources.DeletePod(cleanup, podUID); e != nil {
					step = removalStage("cleanup-pod", e)
				}
			}
			if configUID != "" {
				if e := resources.DeleteConfig(cleanup, configUID); e != nil && step == "" {
					step = removalStage("cleanup-config", e)
				}
			}
			if step == "" && l != nil && configUID != "" && podUID != "" && db.ConfirmExecutionCleanup(cleanup, l.durable, configUID, podUID) != nil {
				step = "cleanup-confirm"
			}
			if step != "" {
				err = cleanupFailure{cause: err, step: step}
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
	resources, err = podtransport.PlanResources(client, podtransport.Binding{Namespace: l.placement.Namespace, WorkerImage: l.placement.WorkerImage, MailboxImage: l.placement.MailboxImage, RuntimeClass: l.placement.SandboxRuntimeClass(), Admission: admission, Assignment: prepared.Assignment(), Profile: prepared.Profile(), ADev: prepared.ADev()})
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
		podUID, e = resources.CreatePodObserved(ctx, in.ConfigUID)
		return store.LifecycleResult{UID: podUID}, e
	}); err != nil {
		return record, ControllerError{stage}
	}
	stage = "readiness"
	session, err := resources.WaitRunning(ctx, podUID)
	if err != nil {
		// A refused toolchain copy is named, for example
		// readiness-toolchain-dynamic for a dynamically linked toolchain.
		if step := podtransport.ReadinessStep(err); podtransport.KnownReadinessStep(step) {
			stage = "readiness-" + step
		}
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
	}); err != nil && !errors.Is(err, errWorkerNonzero) {
		var stopped driveStop
		if stage == "execution" && errors.As(err, &stopped) {
			stage = stopped.stage
		}
		return record, ControllerError{stage}
	}
	return finishGeneration(ctx, db, l, sessionEnd{session}, err, &stage)
}

// generationEnd is the part of the original executor session that ends a
// generation: the bounded worker-exit observation and the candidate export.
type generationEnd interface {
	WorkerFailure(context.Context) (podUID, containerID string, code int32, err error)
	Export(context.Context) (raw []byte, code int32, err error)
}

type sessionEnd struct{ session *podtransport.Session }

func (e sessionEnd) WorkerFailure(ctx context.Context) (string, string, int32, error) {
	return e.session.WorkerFailure(ctx)
}

func (e sessionEnd) Export(ctx context.Context) ([]byte, int32, error) {
	exported, err := e.session.ExportCandidate(ctx)
	return exported.Bytes(), exported.WorkerExitCode(), err
}

// finishGeneration decides, after the original worker exited, whether its
// workspace is exported and captured. driven is nil for exit zero or the
// nonzero exit driveModel observed. A nonzero exit stops before export, with
// one exception: the launcher's step-budget code, once its observation is
// retained (that row is the candidate's durable marker), goes through the
// same export and capture as exit zero, with every bound, and then the same
// independent verification and review. A step-budget run that left no change
// stops as the worker exit it is.
func finishGeneration(ctx context.Context, db *store.DB, l *Lifecycle, end generationEnd, driven error, stage *string) (record store.CandidateRecord, err error) {
	stepBudget := false
	if driven != nil {
		var failure nonzeroWorkerExit
		if !errors.As(driven, &failure) {
			return record, ControllerError{*stage}
		}
		uid, container, code, observationErr := end.WorkerFailure(ctx)
		recorded := observationErr == nil && code == failure.code && db.RecordExecutionFailure(ctx, l.durable, uid, container, code) == nil
		if failure.code != candidate.StepBudgetExitCode || !recorded {
			return record, workerFailure{code: failure.code, recorded: recorded}
		}
		stepBudget = true
	}
	*stage = "export"
	var exported []byte
	if err = l.RunEffect(ctx, *stage, func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		raw, code, e := end.Export(ctx)
		exported = raw
		return store.LifecycleResult{UID: in.PodUID, Digest: workspacebundle.Digest(raw), Bytes: len(raw), ExitCode: &code}, e
	}); err != nil {
		if step := podtransport.ExportStep(err); podtransport.KnownExportStep(step) {
			*stage = "export-" + step
		}
		return record, ControllerError{*stage}
	}
	if stepBudget && l.Unchanged(exported) {
		return record, workerFailure{code: candidate.StepBudgetExitCode, recorded: true}
	}
	*stage = "capture"
	record, err = l.CaptureCandidate(ctx, exported)
	if err != nil {
		return store.CandidateRecord{}, ControllerError{*stage}
	}
	return record, nil
}

func sameRequest(a, b modelmailbox.Envelope) bool {
	return a.Version == b.Version && a.RequestSHA256 == b.RequestSHA256 && a.Attempt == b.Attempt && a.PodUID == b.PodUID && a.ExpiresAt == b.ExpiresAt && a.Kind == b.Kind && a.Status == b.Status && a.Sequence == b.Sequence && modelmailbox.Digest(a.Payload) == modelmailbox.Digest(b.Payload)
}

// driveStop names why driveModel stopped, for the safe diagnostic only. It is
// still ErrAuthority: every cause stops the run the same, fail-closed way.
type driveStop struct{ stage string }

func (e driveStop) Error() string        { return ErrAuthority.Error() }
func (e driveStop) Is(target error) bool { return target == ErrAuthority }

type modelSession interface {
	WorkerExit(context.Context) (int32, bool, error)
	ReadRequest(context.Context) (modelmailbox.Envelope, error)
}

// driveModel waits for the original worker's exit rather than a prescribed
// response count. Every idle cycle rechecks authority. A duplicate observation
// is allowed only for the exact last request; sequence gaps/rebinding stop it.
//
// Authority refused only because the issue evidence is stale (snapshot-stale)
// is checked again, without observing, reading or delivering anything, until
// the refusal's own bound; then it stops as execution-authority-snapshot-stale.
// Any other refusal, and the end of ctx (the claim), stop at once as before.
func driveModel(ctx context.Context, session modelSession, authorize func(context.Context) error, deliver func(context.Context, modelmailbox.Envelope) error) error {
	var previous modelmailbox.Envelope
	var staleUntil time.Time
	for {
		if ctx.Err() != nil {
			return driveStop{"execution-authority"}
		}
		if err := authorize(ctx); err != nil {
			var stale interface{ EvidenceWait() time.Duration }
			if !errors.As(err, &stale) || stale.EvidenceWait() <= 0 || ctx.Err() != nil {
				return driveStop{"execution-authority"}
			}
			now := time.Now()
			if staleUntil.IsZero() {
				staleUntil = now.Add(min(stale.EvidenceWait(), 2*time.Minute))
			}
			if !now.Before(staleUntil) {
				return driveStop{"execution-authority-snapshot-stale"}
			}
			if !pauseDrive(ctx) {
				return driveStop{"execution-authority"}
			}
			continue
		}
		staleUntil = time.Time{}
		code, exited, err := session.WorkerExit(ctx)
		if err != nil {
			return driveStop{"execution-observe"}
		}
		if exited {
			if code != 0 {
				return nonzeroWorkerExit{code}
			}
			return nil
		}
		request, err := session.ReadRequest(ctx)
		if err != nil && !errors.Is(err, modelmailbox.ErrPending) {
			if !errors.Is(err, podtransport.ErrWorkerExited) {
				if step := podtransport.ReadStep(err); step != "" {
					return driveStop{"execution-read-" + step}
				}
				return driveStop{"execution-read"}
			}
			// An observed request at exit must already have a confirmed handoff.
			// Never hide a new request, sequence gap or substituted duplicate
			// behind the writer's zero exit. Pending/pre-read exit is empty.
			if request.Sequence != 0 && (previous.Sequence == 0 || !sameRequest(request, previous)) {
				return driveStop{"execution-sequence"}
			}
			// The writer can exit between ReadRequest's before/after observations.
			// Export/capture still require zero exit, no outstanding model call and a
			// nonempty independently reconstructed delta. Never retry a failed delivery.
			code, exited, exitErr := session.WorkerExit(ctx)
			if exitErr == nil && exited && ctx.Err() == nil {
				if code != 0 {
					return nonzeroWorkerExit{code}
				}
				return nil
			}
			return driveStop{"execution-exit-race"}
		}
		if err == nil {
			if request.Sequence == previous.Sequence && previous.Sequence != 0 {
				if !sameRequest(request, previous) {
					return driveStop{"execution-sequence"}
				}
			} else {
				if request.Sequence != previous.Sequence+1 {
					return driveStop{"execution-sequence"}
				}
				if deliver(ctx, request) != nil {
					return ErrAuthority
				}
				previous = request
				previous.Payload = append([]byte(nil), request.Payload...)
			}
		}
		if !pauseDrive(ctx) {
			return driveStop{"execution-authority"}
		}
	}
}

// pauseDrive waits one driveModel cycle; false when ctx ended first.
func pauseDrive(ctx context.Context) bool {
	timer := time.NewTimer(100 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		return true
	}
}

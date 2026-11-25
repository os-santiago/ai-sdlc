package modelbroker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

var (
	ErrRejected   = errors.New("model dispatch binding or profile rejected")
	ErrNotClaimed = errors.New("model dispatch has no new committed claim")
	ErrUncertain  = errors.New("model dispatch or handoff outcome is uncertain")
	ErrStorage    = errors.New("model dispatch storage unavailable")
)

// Publisher is a trusted, bounded controller capability. It must check the
// actual Pod UID and confirm exclusive publication of these exact bytes through
// the mailbox helper, respecting ctx. It must never execute repository code.
// A worker-supplied callback or a successful socket write is not this capability.
type Publisher interface {
	Publish(context.Context, modelmailbox.Envelope) error
}

// CurrentAuthority is a trusted controller capability, never model or issue
// content. It must revalidate current signed execution ownership/configuration
// and return its nonrenewable deadline. Ledger scope alone is not authority.
// Implementations must honor ctx; the broker does not sandbox callbacks.
type CurrentAuthority interface {
	Authorize(context.Context, store.ModelScope) (time.Time, error)
}

type Dispatcher struct {
	DB        *store.DB
	Provider  *Provider
	Mailbox   Publisher
	Authority CurrentAuthority
}
type Result struct {
	Call                                store.ModelCallRef
	Outcome, FailureStage               string
	DispatchAttempted, HandoffConfirmed bool
}

// staleEvidenceRetry is the pause between checks while current authority is
// refused only because its issue evidence is stale.
var staleEvidenceRetry = 3 * time.Second

// minimumProviderTime is the provider time a stale-evidence wait leaves before
// reservation or dispatch: 30 seconds, the provider's default completion
// timeout and the smallest one an installation may select. A wait that would
// leave less refuses as before instead of spending a reserved or claimed call
// that cannot be answered in time; a check without a wait is not affected.
const minimumProviderTime = 30 * time.Second

// authorizeWaiting is authorize, except that a refusal at snapshot-stale (the
// issue evidence is unchanged but older than its window, for example after one
// incomplete intake scan) is checked again until the refusal's own bound, the
// latest useful time, the scope or ctx ends, whichever is first. Every check is
// a complete authorization; the first that is not snapshot-stale decides. When
// the wait ends the refusal keeps its snapshot-stale name. Stale evidence never
// authorizes, and a changed snapshot is refused at once by its own name.
func (d Dispatcher) authorizeWaiting(ctx context.Context, scope store.ModelScope, latest time.Time) (time.Time, error) {
	var waitUntil time.Time
	for {
		until, wait, err := d.authorizeOnce(ctx, scope)
		if err == nil || wait <= 0 {
			return until, err
		}
		if waitUntil.IsZero() {
			waitUntil = time.Now().Add(min(wait, 2*time.Minute))
			if latest.Before(waitUntil) {
				waitUntil = latest
			}
			if scope.ExpiresAt.Before(waitUntil) {
				waitUntil = scope.ExpiresAt
			}
		}
		if !time.Now().Add(staleEvidenceRetry).Before(waitUntil) {
			return time.Time{}, err
		}
		timer := time.NewTimer(staleEvidenceRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return time.Time{}, err
		case <-timer.C:
		}
	}
}

// authorizeOnce is one bounded authorization. It also returns how long a
// snapshot-stale refusal may be waited for, as the authority bounds it; that
// is zero for every other result.
func (d Dispatcher) authorizeOnce(ctx context.Context, scope store.ModelScope) (until time.Time, wait time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer func() {
		if recover() != nil {
			until, wait = time.Time{}, 0
			err = ErrRejected
		}
	}()
	until, err = d.Authority.Authorize(ctx, scope)
	switch {
	case err != nil:
		step := "authority"
		var named interface{ AuthorityStep() string }
		if errors.As(err, &named) && authorizationRefusalStage.MatchString("authorization-dispatch-"+named.AuthorityStep()) {
			step = named.AuthorityStep()
		}
		var stale interface{ EvidenceWait() time.Duration }
		if step == "snapshot-stale" && errors.As(err, &stale) {
			wait = stale.EvidenceWait()
		}
		return time.Time{}, wait, authorizationRefusal(step)
	case ctx.Err() != nil:
		return time.Time{}, 0, authorizationRefusal("deadline")
	case !until.After(time.Now()):
		return time.Time{}, 0, authorizationRefusal("claim-expired")
	case !scope.ExpiresAt.After(time.Now()):
		return time.Time{}, 0, authorizationRefusal("scope-expired")
	}
	if scope.ExpiresAt.Before(until) {
		until = scope.ExpiresAt
	}
	return until, 0, nil
}

func (d Dispatcher) abandon(ctx context.Context, ref store.ModelCallRef) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// Failure remains uncertain. Started or unhanded completion cannot grant a
	// replay; ledger expiry/recovery remains necessary if storage is unavailable.
	_, _ = d.DB.StopModelCall(cleanup, ref)
}

// Deliver is for a future trusted controller with current signed execution
// authority. Ledger scope is necessary but does not itself confer that authority.
// It never provisions a budget/allocation, returns provider bodies to a caller,
// replays an uncertain call or interprets generated tools as control-plane work.
func (d Dispatcher) Deliver(ctx context.Context, allocation string, profileRaw []byte, request modelmailbox.Envelope) (result Result, err error) {
	stage := "configuration"
	defer func() {
		if err != nil {
			result.FailureStage = SafeFailureStage(stage)
		}
	}()
	if d.DB == nil || d.Provider == nil || d.Mailbox == nil || d.Authority == nil {
		return result, ErrRejected
	}
	if len(request.Payload) == 0 || len(request.Payload) > modelmailbox.MaxRequestBytes || len(profileRaw) == 0 || len(profileRaw) > executor.MaxProfileBytes {
		return result, ErrRejected
	}
	// Own the exact bytes before validation/reservation; caller-owned slices must
	// not be substituted between a committed claim and the provider request.
	request.Payload = append([]byte(nil), request.Payload...)
	profileRaw = append([]byte(nil), profileRaw...)
	stage = "scope"
	scope, err := d.DB.ReadModelScope(ctx, allocation)
	if err != nil {
		return result, ErrStorage
	}
	stage = "request-binding"
	profile, err := executor.ParseProfile(profileRaw, scope.PolicyDigest)
	identity := modelmailbox.Identity{Attempt: fmt.Sprintf("%s-%d", scope.WorkItem, scope.Attempt), PodUID: scope.PodUID}
	if err != nil || profile.Model != scope.Model || request.Validate(identity, "request") != nil || (modelmailbox.RequestLimits{Model: profile.Model, MaxTokens: profile.MaxTokens}).Validate(request.Payload) != nil {
		return result, ErrRejected
	}
	stage = "authorization-reservation"
	mailboxDeadline := time.UnixMilli(request.ExpiresAt).Add(-15 * time.Second)
	until, err := d.authorizeWaiting(ctx, scope, mailboxDeadline.Add(-minimumProviderTime))
	if err != nil {
		stage = refusedStage(stage, err)
		return result, ErrRejected
	}
	ctx, cancelAuthority := context.WithDeadline(ctx, until)
	defer cancelAuthority()
	// Retain the deterministic reference even if COMMIT succeeds but its reply
	// is lost. No database error proves that reservation or claim rolled back.
	ref := store.ModelCallRef{AllocationID: allocation, PodUID: scope.PodUID, Sequence: int(request.Sequence), RequestSHA256: modelmailbox.Digest(request.Payload)}
	result.Call = ref
	stage = "reservation"
	decision, err := d.DB.ReserveModelCallWithin(ctx, allocation, scope.PodUID, int(request.Sequence), request.Payload, profile.CallWindow())
	if err != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	if decision.Reason != "reserved" && decision.Reason != "existing" {
		result.Outcome = decision.Reason
		return result, ErrNotClaimed
	}
	ref = decision.Call.ModelCallRef
	result.Call = ref
	if decision.Call.State == "completed" && decision.Call.ResponseHandedOff {
		result.Outcome = "already-handed-off"
		result.HandoffConfirmed = true
		return result, nil
	}
	stage = "claim"
	claim, err := d.DB.ClaimModelCall(ctx, ref)
	if err != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	if !claim.Invoke {
		result.Outcome = claim.Reason
		return result, ErrNotClaimed
	}
	stage = "authorization-dispatch"
	// A stale-evidence wait leaves the provider at least minimumProviderTime
	// before the call's own deadline below.
	callDeadline := claim.Call.ExpiresAt.Add(-15 * time.Second)
	if mailboxDeadline.Before(callDeadline) {
		callDeadline = mailboxDeadline
	}
	until, err = d.authorizeWaiting(ctx, scope, callDeadline.Add(-minimumProviderTime))
	if err != nil {
		stage = refusedStage(stage, err)
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	currentCtx, stopAuthority := context.WithDeadline(ctx, until)
	defer stopAuthority()
	ctx = currentCtx
	stage = "deadline"
	deadline := callDeadline
	if profileDeadline := time.Now().Add(time.Duration(profile.MaxSeconds) * time.Second); profileDeadline.Before(deadline) {
		deadline = profileDeadline
	}
	if !time.Now().Before(deadline) || ctx.Err() != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	callCtx, stopCall := context.WithDeadline(ctx, deadline)
	result.DispatchAttempted = true
	stage = "provider"
	response, err := d.Provider.observedInvoke(callCtx, "generator", request.Payload, claim.Call.ResponseLimit)
	stopCall()
	if err != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	stage = "completion-commit"
	completed, err := d.DB.FinishModelCall(ctx, ref, 200, response)
	if err != nil || completed.Reason != "completed" || completed.Call.State != "completed" {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	responseDigest := modelmailbox.Digest(response)
	reply := request
	reply.Kind = "response"
	reply.Status = 200
	reply.Payload = append([]byte(nil), response...)
	stage = "response-envelope"
	if reply.Validate(identity, "response") != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	stage = "authorization-publication"
	// A stale-evidence wait leaves the publication's 5 seconds before the
	// mailbox stops waiting.
	until, err = d.authorizeWaiting(ctx, scope, time.UnixMilli(request.ExpiresAt).Add(-10*time.Second))
	if err != nil {
		stage = refusedStage(stage, err)
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	// Never publish after the mailbox stopped waiting: SCC has already seen a
	// failure (and may retry), so a late handoff would never be received. The
	// mailbox still rejects expired replies itself (clocks may differ).
	if mailboxEnd := time.UnixMilli(request.ExpiresAt); mailboxEnd.Before(until) {
		until = mailboxEnd
	}
	if !time.Now().Before(until) {
		stage = "publication-expired"
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	publicationCtx, stopPublicationAuthority := context.WithDeadline(ctx, until)
	defer stopPublicationAuthority()
	publishCtx, stopPublish := context.WithTimeout(publicationCtx, 5*time.Second)
	stage = "publication"
	err = d.Mailbox.Publish(publishCtx, reply)
	stopPublish()
	if err != nil || modelmailbox.Digest(reply.Payload) != responseDigest {
		var diagnostic interface{ PublicationStage() string }
		if errors.As(err, &diagnostic) {
			stage = "publication-" + safePublicationStage(diagnostic.PublicationStage())
		}
		// The worker had already exited, for example killed (124) when its run
		// budget ended while this call was in flight: name its exit code.
		var exited interface{ WorkerExitCode() (int32, bool) }
		if errors.As(err, &exited) {
			if code, ok := exited.WorkerExitCode(); ok {
				stage = fmt.Sprintf("publication-worker-exit-%d", code)
			}
		}
		if modelmailbox.Digest(reply.Payload) != responseDigest {
			stage = "publication-response-mutation"
		}
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	ackCtx, stopAck := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	stage = "acknowledgement"
	ack, err := d.DB.AcknowledgeModelResponse(ackCtx, ref, responseDigest, 200, len(response))
	stopAck()
	if err != nil || !ack.Call.ResponseHandedOff || (ack.Reason != "handed-off" && ack.Reason != "existing") {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	result.Outcome = "handed-off"
	result.HandoffConfirmed = true
	return result, nil
}

// SafeFailureStage is an allowlist, never exception text, provider output,
// request content or a command. It can safely be emitted by the controller.
func SafeFailureStage(stage string) string {
	switch stage {
	case "configuration", "scope", "request-binding", "authorization-reservation", "reservation", "claim", "authorization-dispatch", "deadline", "provider", "completion-commit", "response-envelope", "authorization-publication", "publication", "acknowledgement", "publication-envelope", "publication-encoding", "publication-before-observation", "publication-helper-transport", "publication-helper-exit", "publication-receipt", "publication-after-observation", "publication-response-mutation", "publication-expired", "publication-unknown":
		return stage
	}
	if publicationWorkerExit.MatchString(stage) || publicationHelperExit.MatchString(stage) || authorizationRefusalStage.MatchString(stage) {
		return stage
	}
	return "unknown"
}

// A bounded container exit code (0-255), without leading zeros.
var publicationWorkerExit = regexp.MustCompile(`^publication-worker-exit-(0|[1-9][0-9]?|1[0-9][0-9]|2[0-4][0-9]|25[0-5])$`)

// A bounded nonzero exit code (1-255) of the mailbox publication helper.
var publicationHelperExit = regexp.MustCompile(`^publication-helper-exit-([1-9][0-9]?|1[0-9][0-9]|2[0-4][0-9]|25[0-5])$`)

// authorizationRefusal names the bounded check that refused current authority
// before a model call step. It unwraps to ErrRejected.
type authorizationRefusal string

func (e authorizationRefusal) Error() string { return "model call authorization refused" }
func (e authorizationRefusal) Unwrap() error { return ErrRejected }

func refusedStage(stage string, err error) string {
	var refused authorizationRefusal
	if errors.As(err, &refused) {
		return stage + "-" + string(refused)
	}
	return stage
}

// The refusal steps of Dispatcher.authorize and the execution lifecycle's
// Authorize; any other text stays unknown.
var authorizationRefusalStage = regexp.MustCompile(`^authorization-(reservation|dispatch|publication)-(authority|deadline|claim-expired|scope-expired|busy|stopped|scope|panic|phase|model-proof|model-policy|prepared|binding|claim|claim-read|inputs|record|repair|snapshot|snapshot-stale|reload)$`)

func safePublicationStage(stage string) string {
	switch stage {
	case "envelope", "encoding", "before-observation", "helper-transport", "helper-exit", "receipt", "after-observation":
		return stage
	}
	if publicationHelperExit.MatchString("publication-" + stage) {
		return stage
	}
	return "unknown"
}

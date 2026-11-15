package modelbroker

import (
	"context"
	"errors"
	"fmt"
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
	Outcome                             string
	DispatchAttempted, HandoffConfirmed bool
}

func (d Dispatcher) authorize(ctx context.Context, scope store.ModelScope) (until time.Time, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer func() {
		if recover() != nil {
			until = time.Time{}
			err = ErrRejected
		}
	}()
	until, err = d.Authority.Authorize(ctx, scope)
	if err != nil || ctx.Err() != nil || !until.After(time.Now()) || !scope.ExpiresAt.After(time.Now()) {
		return time.Time{}, ErrRejected
	}
	if scope.ExpiresAt.Before(until) {
		until = scope.ExpiresAt
	}
	return until, nil
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
func (d Dispatcher) Deliver(ctx context.Context, allocation string, profileRaw []byte, request modelmailbox.Envelope) (Result, error) {
	var result Result
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
	scope, err := d.DB.ReadModelScope(ctx, allocation)
	if err != nil {
		return result, ErrStorage
	}
	profile, err := executor.ParseProfile(profileRaw, scope.PolicyDigest)
	identity := modelmailbox.Identity{Attempt: fmt.Sprintf("%s-%d", scope.WorkItem, scope.Attempt), PodUID: scope.PodUID}
	if err != nil || profile.Model != scope.Model || request.Validate(identity, "request") != nil || (modelmailbox.RequestLimits{Model: profile.Model, MaxTokens: profile.MaxTokens}).Validate(request.Payload) != nil {
		return result, ErrRejected
	}
	until, err := d.authorize(ctx, scope)
	if err != nil {
		return result, ErrRejected
	}
	ctx, cancelAuthority := context.WithDeadline(ctx, until)
	defer cancelAuthority()
	// Retain the deterministic reference even if COMMIT succeeds but its reply
	// is lost. No database error proves that reservation or claim rolled back.
	ref := store.ModelCallRef{AllocationID: allocation, PodUID: scope.PodUID, Sequence: int(request.Sequence), RequestSHA256: modelmailbox.Digest(request.Payload)}
	result.Call = ref
	decision, err := d.DB.ReserveModelCall(ctx, allocation, scope.PodUID, int(request.Sequence), request.Payload)
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
	claim, err := d.DB.ClaimModelCall(ctx, ref)
	if err != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	if !claim.Invoke {
		result.Outcome = claim.Reason
		return result, ErrNotClaimed
	}
	until, err = d.authorize(ctx, scope)
	if err != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	currentCtx, stopAuthority := context.WithDeadline(ctx, until)
	defer stopAuthority()
	ctx = currentCtx
	deadline := claim.Call.ExpiresAt.Add(-15 * time.Second)
	if mailboxDeadline := time.UnixMilli(request.ExpiresAt).Add(-15 * time.Second); mailboxDeadline.Before(deadline) {
		deadline = mailboxDeadline
	}
	if profileDeadline := time.Now().Add(time.Duration(profile.MaxSeconds) * time.Second); profileDeadline.Before(deadline) {
		deadline = profileDeadline
	}
	if !time.Now().Before(deadline) || ctx.Err() != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	callCtx, stopCall := context.WithDeadline(ctx, deadline)
	result.DispatchAttempted = true
	response, err := d.Provider.invoke(callCtx, request.Payload, claim.Call.ResponseLimit)
	stopCall()
	if err != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
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
	if reply.Validate(identity, "response") != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	until, err = d.authorize(ctx, scope)
	if err != nil {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	publicationCtx, stopPublicationAuthority := context.WithDeadline(ctx, until)
	defer stopPublicationAuthority()
	publishCtx, stopPublish := context.WithTimeout(publicationCtx, 5*time.Second)
	err = d.Mailbox.Publish(publishCtx, reply)
	stopPublish()
	if err != nil || modelmailbox.Digest(reply.Payload) != responseDigest {
		d.abandon(ctx, ref)
		return result, ErrUncertain
	}
	ackCtx, stopAck := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
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

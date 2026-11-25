package modelbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// ReviewAuthority must revalidate the original signed verification/current issue,
// immutable candidate, behavior proof, review configuration and exclusive claim.
// Journal ownership alone is insufficient. This capability is controller-owned.
type ReviewAuthority interface {
	AuthorizeReview(context.Context, store.ReviewRun, store.ReviewClaim) (time.Time, error)
}
type ReviewDispatcher struct {
	DB                  *store.DB
	Provider            *Provider
	Mailbox             Publisher
	Authority           ReviewAuthority
	ConfigurationDigest string
}
type ReviewDelivery struct {
	FailureStage                        string
	DispatchAttempted, HandoffConfirmed bool
}

func (d ReviewDispatcher) authorizeReview(ctx context.Context, r store.ReviewRun, c store.ReviewClaim) (until time.Time, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	defer func() {
		if recover() != nil {
			until = time.Time{}
			err = ErrRejected
		}
	}()
	if d.DB.CheckReviewClaim(ctx, c) != nil {
		return time.Time{}, ErrRejected
	}
	until, err = d.Authority.AuthorizeReview(ctx, r, c)
	if err != nil || ctx.Err() != nil || d.DB.CheckReviewClaim(ctx, c) != nil || ctx.Err() != nil {
		return time.Time{}, ErrRejected
	}
	if c.Until.Before(until) {
		until = c.Until
	}
	if r.ExpiresAt.Before(until) {
		until = r.ExpiresAt
	}
	if !until.After(time.Now()) {
		return time.Time{}, ErrRejected
	}
	return until, nil
}

// DeliverReview never replays a call or accepts worker-authored review evidence.
// Every failed/uncertain attempt stops its own parent claim, including a lost
// successful handoff ACK. A new dispatcher process cannot turn that into retry.
// This is an inactive component until a reviewed controller/profile wires it.
func (d ReviewDispatcher) DeliverReview(ctx context.Context, c store.ReviewClaim, profileRaw []byte, request modelmailbox.Envelope) (result ReviewDelivery, err error) {
	stage := "configuration"
	defer func() {
		if err != nil {
			result.FailureStage = stage
			if d.DB != nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cancel()
				_ = d.DB.StopReview(cleanup, c, "uncertain")
			}
		}
	}()
	if d.DB == nil || d.Provider == nil || d.Mailbox == nil || d.Authority == nil || !reviewDigest(d.ConfigurationDigest) || len(profileRaw) > executor.MaxProfileBytes || len(request.Payload) > modelmailbox.MaxRequestBytes {
		return result, ErrRejected
	}
	profileRaw = append([]byte(nil), profileRaw...)
	request.Payload = append([]byte(nil), request.Payload...)
	stage = "scope"
	run, err := d.DB.ReadReview(ctx, c.ID)
	if err != nil || run.ConfigurationDigest != d.ConfigurationDigest {
		return result, ErrRejected
	}
	stage = "request-binding"
	profile, err := executor.ParseReviewProfile(profileRaw, run.ProfileDigest)
	id := modelmailbox.Identity{Attempt: fmt.Sprintf("%s-1", run.WorkItem), PodUID: request.PodUID}
	limits := modelmailbox.RequestLimits{Model: profile.Model, MaxTokens: profile.MaxTokens}
	if err != nil || request.Validate(id, "request") != nil || limits.Validate(request.Payload) != nil {
		return result, ErrRejected
	}
	// A well-bound request beyond the signed profile's turn budget is named
	// apart from binding failures: the reviewer ran out of turns. It is
	// refused before any journal read, reservation or provider call.
	if request.Sequence > uint64(profile.MaxSteps) {
		stage = "request-turn-budget"
		return result, ErrRejected
	}
	// sent is what the provider receives and what the journal records. On the
	// last permitted turn the broker withholds every tool, so the model must
	// answer; the mailbox reply still binds the worker's own request digest.
	sent := request.Payload
	if finalReviewTurn(request.Sequence, profile.MaxSteps) {
		sent, err = finalReviewTurnPayload(request.Payload)
		if err != nil || len(sent) > modelmailbox.MaxRequestBytes || limits.Validate(sent) != nil {
			return result, ErrRejected
		}
	}
	// Previous final verdicts end the conversation; source cannot solicit another
	// response after seeing an unfavorable answer. DB checks contiguous handoffs.
	if request.Sequence > 1 {
		previous, e := d.DB.ReadReviewModelEvidence(ctx, c)
		if e != nil || len(previous) != int(request.Sequence)-1 {
			return result, ErrRejected
		}
		for _, entry := range previous {
			var o ReviewObservation
			if json.Unmarshal(entry.Observation, &o) != nil || o.Version != reviewObservationVersion || o.Verdict != "" {
				return result, ErrRejected
			}
		}
	}
	stage = "authorization-reservation"
	until, err := d.authorizeReview(ctx, run, c)
	if err != nil {
		return result, ErrRejected
	}
	deadline := time.UnixMilli(request.ExpiresAt).Add(-10 * time.Second)
	if until.Before(deadline) {
		deadline = until
	}
	profileEnd := time.Now().Add(time.Duration(profile.MaxSeconds) * time.Second)
	if profileEnd.Before(deadline) {
		deadline = profileEnd
	}
	if !deadline.After(time.Now()) {
		return result, ErrRejected
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	stage = "reservation"
	call, err := d.DB.BeginReviewModelCall(ctx, c, request.PodUID, int(request.Sequence), sent)
	if err != nil {
		return result, ErrUncertain
	}
	stage = "authorization-dispatch"
	until, err = d.authorizeReview(ctx, run, c)
	if err != nil {
		return result, ErrUncertain
	}
	callCtx, cancelCall := context.WithDeadline(ctx, until)
	stage = "provider"
	result.DispatchAttempted = true
	response, err := d.Provider.observedInvoke(callCtx, "reviewer", sent, 32768)
	cancelCall()
	if err != nil {
		return result, ErrUncertain
	}
	stage = "response-policy"
	observation, err := ObserveReviewResponse(response, profile.Model)
	if err != nil || (request.Sequence == 1 && observation.Verdict != "") {
		return result, ErrRejected
	}
	observationRaw, err := json.Marshal(observation)
	if err != nil {
		return result, ErrRejected
	}
	stage = "authorization-completion"
	until, err = d.authorizeReview(ctx, run, c)
	if err != nil {
		return result, ErrUncertain
	}
	completionCtx, stopCompletion := context.WithDeadline(ctx, until)
	defer stopCompletion()
	stage = "completion-commit"
	if err = d.DB.CompleteReviewModelCall(completionCtx, call, response, observationRaw); err != nil {
		return result, ErrUncertain
	}
	reply := request
	reply.Kind = "response"
	reply.Status = 200
	reply.Payload = append([]byte(nil), response...)
	digest := modelmailbox.Digest(response)
	stage = "response-envelope"
	if reply.Validate(id, "response") != nil {
		return result, ErrRejected
	}
	stage = "authorization-publication"
	until, err = d.authorizeReview(ctx, run, c)
	if err != nil {
		return result, ErrUncertain
	}
	publicationCtx, stop := context.WithDeadline(ctx, until)
	defer stop()
	publishCtx, stopPublish := context.WithTimeout(publicationCtx, 5*time.Second)
	stage = "publication"
	err = d.Mailbox.Publish(publishCtx, reply)
	stopPublish()
	if err != nil || modelmailbox.Digest(reply.Payload) != digest {
		return result, ErrUncertain
	}
	stage = "authorization-acknowledgement"
	until, err = d.authorizeReview(ctx, run, c)
	if err != nil {
		return result, ErrUncertain
	}
	ackCtx, stopAck := context.WithDeadline(ctx, until)
	defer stopAck()
	stage = "acknowledgement"
	if err = d.DB.AcknowledgeReviewModelCall(ackCtx, call, digest); err != nil {
		return result, ErrUncertain
	}
	result.HandoffConfirmed = true
	return result, nil
}

// ReviewFinalTurnNote is the broker's fixed instruction on the last permitted
// review turn. It carries no source, model or repository content.
const ReviewFinalTurnNote = "This is your last turn: give the final verdict now in the required format."

// finalReviewTurn reports whether sequence is the last turn the signed review
// profile permits. The first turn never qualifies: it must read the candidate,
// and the response policy refuses a verdict there.
func finalReviewTurn(sequence uint64, maxSteps int) bool {
	return maxSteps > 1 && sequence == uint64(maxSteps)
}

// finalReviewTurnPayload removes tools and tool_choice from an already
// validated chat request and appends ReviewFinalTurnNote as a last user
// message. Every other field and message keeps its JSON value; nothing is
// added beyond the note.
func finalReviewTurnPayload(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if authority.DecodeStrict(raw, &fields) != nil || fields == nil {
		return nil, ErrRejected
	}
	var messages []json.RawMessage
	if authority.DecodeStrict(fields["messages"], &messages) != nil || len(messages) == 0 {
		return nil, ErrRejected
	}
	note, err := encodeVerbatim(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{"user", ReviewFinalTurnNote})
	if err != nil {
		return nil, ErrRejected
	}
	delete(fields, "tools")
	delete(fields, "tool_choice")
	if fields["messages"], err = encodeVerbatim(append(messages, note)); err != nil {
		return nil, ErrRejected
	}
	if raw, err = encodeVerbatim(fields); err != nil {
		return nil, ErrRejected
	}
	return raw, nil
}

// encodeVerbatim is json.Marshal without HTML escaping, so embedded JSON
// values keep their text apart from insignificant whitespace.
func encodeVerbatim(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

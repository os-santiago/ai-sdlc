package reviewflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type ControllerError struct {
	Stage          string
	CleanupPending bool
}

func (e ControllerError) Error() string { return "independent review stopped at " + e.Stage }

type Proof struct {
	Version               string `json:"version"`
	ReviewID              string `json:"reviewID"`
	VerificationID        string `json:"verificationID"`
	CandidateID           string `json:"candidateID"`
	BehaviorSHA256        string `json:"behaviorSHA256"`
	ProfileSHA256         string `json:"profileSHA256"`
	ConfigurationSHA256   string `json:"configurationSHA256"`
	AssignmentSHA256      string `json:"assignmentSHA256"`
	CandidateTreeSHA256   string `json:"candidateTreeSHA256"`
	ModelEvidenceSHA256   string `json:"modelEvidenceSHA256"`
	Verdict               string `json:"verdict"`
	SourceUnchanged       bool   `json:"sourceUnchanged"`
	PublicationAuthorized bool   `json:"publicationAuthorized"`
}

// Sandbox is the reviewer RuntimeClass the caller verified, with what its
// admission adds to each pod. The review runs only if its configuration still
// names that class.
type Sandbox struct {
	RuntimeClass string
	Admission    podtransport.RuntimeClassAdmission
}

// Run takes only an existing verification identifier from the trusted queue.
// It has no external claim/advance endpoint and never publishes or merges code.
func Run(ctx context.Context, db *store.DB, policy verificationflow.PolicyLoader, load Loader, id string, client *podtransport.Client, sandbox Sandbox) (Proof, error) {
	return run(ctx, db, policy, load, id, client, sandbox, fetchSource)
}
func run(ctx context.Context, db *store.DB, policy verificationflow.PolicyLoader, load Loader, id string, client *podtransport.Client, sandbox Sandbox, fetch sourceLoader) (proof Proof, err error) {
	if db == nil || policy == nil || load == nil || client == nil {
		return proof, ControllerError{Stage: "configuration"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	p, e := prepare(ctx, db, policy, load, id, fetch)
	if e != nil {
		return proof, ControllerError{Stage: preparationStage(e)}
	}
	// Observe the authenticated sidecar before queuing or claiming authority.
	// Startup unavailability must not consume an irreversible model attempt.
	provider, e := modelbroker.NewProvider(p.config.Model)
	if e != nil || provider.CheckGateway(ctx) != nil {
		return proof, ControllerError{Stage: "gateway-unavailable"}
	}
	if p.config.RequireGatewayProfile && gatewayProfileFits(ctx, provider, p.config.Profile) != nil {
		return proof, ControllerError{Stage: "gateway-profile-unfit"}
	}
	if p.binding.RuntimeClass != sandbox.RuntimeClass {
		return proof, ControllerError{Stage: "sandbox-runtime-class"}
	}
	p.binding.Admission = sandbox.Admission
	resources, e := podtransport.PlanResources(client, p.binding)
	if e != nil {
		return proof, ControllerError{Stage: "template"}
	}
	existing, e := db.ReadReview(ctx, p.run.ID)
	if e != nil {
		if errors.Is(e, store.ErrObservationUnavailable) {
			return proof, ControllerError{Stage: "observation"}
		}
		existing, e = db.QueueReview(ctx, id, p.run.BehaviorDigest, p.run.ProfileDigest, p.run.ConfigurationDigest, p.run.AssignmentDigest)
		if e != nil {
			return proof, ControllerError{Stage: "queue"}
		}
	}
	if !sameRun(existing, p.run) {
		return proof, ControllerError{Stage: "binding"}
	}
	p.run = existing
	claim, e := db.ClaimReview(ctx, p.run.ID, p.run.ConfigurationDigest)
	if e != nil {
		return proof, ControllerError{Stage: "claim"}
	}
	ctx, stopClaim := context.WithDeadline(ctx, claim.Until)
	defer stopClaim()
	stage := "authority"
	configUID, podUID := "", ""
	cleaned, cleanupAttempted := false, false
	cleanupStage := ""
	cleanup := func() error {
		cleanupAttempted = true
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		failed := ""
		if podUID != "" {
			if e := resources.DeletePod(cleanupCtx, podUID); e != nil {
				failed = removalStage("cleanup-pod", e)
			}
		}
		if configUID != "" {
			if e := resources.DeleteConfig(cleanupCtx, configUID); e != nil && failed == "" {
				failed = removalStage("cleanup-config", e)
			}
		}
		if failed == "" && db.ConfirmReviewCleanup(cleanupCtx, claim) != nil {
			failed = "cleanup-confirm"
		}
		if failed != "" {
			cleanupStage = failed
			return ErrReview
		}
		cleaned = true
		return nil
	}
	defer func() {
		if recover() != nil {
			err = ControllerError{Stage: stage}
		}
		if !cleaned && !cleanupAttempted {
			if cleanup() != nil {
				primary := stage
				var diagnostic ControllerError
				if errors.As(err, &diagnostic) {
					primary = diagnostic.Stage
				}
				err = ControllerError{Stage: primary, CleanupPending: true}
			}
		}
		if err != nil {
			proof = Proof{}
			stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = db.StopReview(stopCtx, claim, "uncertain")
		}
	}()
	auth := currentAuthority{db: db, policy: policy, load: load, expected: p}
	fence := func(ctx context.Context) error { _, e := auth.AuthorizeReview(ctx, p.run, claim); return e }
	effect := func(kind string, invoke func() (string, string, error)) error {
		if fence(ctx) != nil {
			return ErrReview
		}
		intent, e := db.BeginReviewEffect(ctx, claim, kind)
		if e != nil {
			return e
		}
		if fence(ctx) != nil {
			cancelCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = db.CancelUnsentReviewEffect(cancelCtx, intent)
			return ErrReview
		}
		uid, digest, e := invoke()
		if e != nil {
			// A pod the create response named, though a later check failed:
			// acknowledge it so cleanup can delete exactly that object.
			if kind == "pod" && uid != "" {
				ackCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				_ = db.AcknowledgeReviewEffect(ackCtx, intent, uid, digest)
				stop()
			}
			return e
		}
		if db.AcknowledgeReviewEffect(ctx, intent, uid, digest) != nil {
			return ErrReview
		}
		return fence(ctx)
	}
	stage = "config"
	if effect("config", func() (string, string, error) { configUID, e = resources.CreateConfig(ctx); return configUID, "", e }) != nil {
		return proof, ControllerError{Stage: stage}
	}
	stage = "pod"
	if effect("pod", func() (string, string, error) {
		podUID, e = resources.CreatePodObserved(ctx, configUID)
		return podUID, "", e
	}) != nil {
		return proof, ControllerError{Stage: stage}
	}
	stage = "readiness"
	session, e := resources.WaitRunning(ctx, podUID)
	if e != nil {
		return proof, ControllerError{Stage: stage}
	}
	stage = "import"
	if effect("import", func() (string, string, error) { return podUID, p.tree, session.ImportSource(ctx, p.source) }) != nil {
		return proof, ControllerError{Stage: stage}
	}
	stage = "start"
	if effect("start", func() (string, string, error) { return podUID, "", session.Start(ctx) }) != nil {
		return proof, ControllerError{Stage: stage}
	}
	dispatcher := modelbroker.ReviewDispatcher{DB: db, Provider: provider, Mailbox: session, Authority: auth, ConfigurationDigest: p.run.ConfigurationDigest}
	stage = "model"
	e = drive(ctx, session, fence, func(request modelmailbox.Envelope) error {
		outcome, e := dispatcher.DeliverReview(ctx, claim, p.binding.Profile, request)
		if e != nil {
			stage = "model-" + outcome.FailureStage
		}
		return e
	})
	if e != nil {
		var detail ControllerError
		if errors.As(e, &detail) {
			stage = "model-" + detail.Stage
		}
		return proof, ControllerError{Stage: stage}
	}
	stage = "export"
	var summary podtransport.Review
	if effect("export", func() (string, string, error) {
		exported, e := session.ExportCandidate(ctx)
		if e != nil {
			return "", "", e
		}
		tree, e := workspacebundle.Decode(exported.Bytes())
		if e != nil {
			return "", "", e
		}
		digest, e := tree.TreeDigest()
		if e != nil || tree.SourceSHA != p.command.SourceSHA || digest != p.tree {
			return "", "", ErrReview
		}
		summary, e = session.ExportReview(ctx)
		if e != nil {
			return "", "", e
		}
		return podUID, digest, nil
	}) != nil {
		return proof, ControllerError{Stage: stage}
	}
	stage = "model-evidence"
	evidence, e := db.ReadReviewModelEvidence(ctx, claim)
	result := summary.Result().WithholdUnboundFeedback()
	if e != nil || modelbroker.MatchReviewSummary(result, evidence) != nil {
		return proof, ControllerError{Stage: stage}
	}
	stage = "cleanup"
	if cleanup() != nil {
		return proof, ControllerError{Stage: cleanupStage, CleanupPending: true}
	}
	stage = "authority"
	if fence(ctx) != nil {
		return proof, ControllerError{Stage: stage}
	}
	if result.Verdict != "approve" {
		if db.RejectReviewWithFeedback(ctx, claim, result) != nil {
			return proof, ControllerError{Stage: "rejection-commit"}
		}
		return proof, ControllerError{Stage: "request-changes"}
	}
	modelRaw, _ := json.Marshal(evidence)
	proof = Proof{Version: "ai-sdlc.independent-review-proof/v1", ReviewID: p.run.ID, VerificationID: id, CandidateID: p.run.CandidateID, BehaviorSHA256: p.run.BehaviorDigest, ProfileSHA256: p.run.ProfileDigest, ConfigurationSHA256: p.run.ConfigurationDigest, AssignmentSHA256: p.run.AssignmentDigest, CandidateTreeSHA256: p.tree, ModelEvidenceSHA256: authority.Digest(modelRaw), Verdict: "approve", SourceUnchanged: true, PublicationAuthorized: false}
	raw, _ := json.Marshal(proof)
	stage = "commit"
	if db.FinishReview(ctx, claim, raw) != nil {
		return Proof{}, ControllerError{Stage: stage}
	}
	return proof, nil
}
func sameRun(a, b store.ReviewRun) bool {
	if !a.ExpiresAt.Equal(b.ExpiresAt) {
		return false
	}
	a.ExpiresAt = time.Time{}
	b.ExpiresAt = time.Time{}
	return a == b
}

type reviewSession interface {
	WorkerExit(context.Context) (int32, bool, error)
	ReadRequest(context.Context) (modelmailbox.Envelope, error)
}

func sameRequest(a, b modelmailbox.Envelope) bool {
	left, right := a, b
	left.Payload = nil
	right.Payload = nil
	x, _ := json.Marshal(left)
	y, _ := json.Marshal(right)
	return bytes.Equal(x, y) && bytes.Equal(a.Payload, b.Payload)
}

// drive relays the reviewer's model requests. Each stop names a bounded
// drive step (reported as model-drive-<step>), like the execution drive; a
// failed delivery keeps the dispatcher's own stage. Every stop still ends the
// review the same way.
func drive(ctx context.Context, session reviewSession, fence func(context.Context) error, deliver func(modelmailbox.Envelope) error) error {
	var previous modelmailbox.Envelope
	for {
		if ctx.Err() != nil {
			return ControllerError{Stage: "drive-deadline"}
		}
		if fence(ctx) != nil {
			return ControllerError{Stage: "drive-authority"}
		}
		code, exited, e := session.WorkerExit(ctx)
		if e != nil {
			return ControllerError{Stage: "drive-observe"}
		}
		if exited {
			if code != 10 || previous.Sequence < 2 {
				return ControllerError{Stage: fmt.Sprintf("worker-exit-%d", code)}
			}
			return nil
		}
		request, e := session.ReadRequest(ctx)
		if errors.Is(e, podtransport.ErrWorkerExited) {
			// An unhanded or substituted request at exit is a sequence breach,
			// as in the execution drive.
			if request.Sequence != 0 && !sameRequest(request, previous) {
				return ControllerError{Stage: "drive-sequence"}
			}
			code, exited, e = session.WorkerExit(ctx)
			if e == nil && exited && code == 10 && previous.Sequence >= 2 {
				return nil
			}
			return ControllerError{Stage: "drive-exit-race"}
		}
		if e != nil && !errors.Is(e, modelmailbox.ErrPending) {
			if step := podtransport.ReadStep(e); step != "" {
				return ControllerError{Stage: "mailbox-read-" + step}
			}
			return ControllerError{Stage: "mailbox-read"}
		}
		if e == nil {
			if request.Sequence == previous.Sequence && previous.Sequence > 0 {
				if !sameRequest(request, previous) {
					return ControllerError{Stage: "drive-sequence"}
				}
			} else {
				if request.Sequence != previous.Sequence+1 {
					return ControllerError{Stage: "drive-sequence"}
				}
				if deliver(request) != nil {
					return ErrReview
				}
				previous = request
				previous.Payload = append([]byte(nil), request.Payload...)
			}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ControllerError{Stage: "drive-deadline"}
		case <-timer.C:
		}
	}
}

// gatewayProfileFits refuses a review whose calls could not complete: the
// gateway's benchmarked timeout plus the broker margin must fit the reviewer
// profile's per-call window. A model slower than that would only end uncertain.
// Admission only: each call runs up to the gateway's ceiling, bounded by the
// broker's remaining wait that the provider sends with the request.
func gatewayProfileFits(ctx context.Context, provider *modelbroker.Provider, profile []byte) error {
	seconds, e := provider.GatewayProfile(ctx)
	if e != nil {
		return e
	}
	review, e := executor.ParseReviewProfile(profile, authority.Digest(profile))
	if e != nil || time.Duration(seconds+20)*time.Second > review.CallWindow() {
		return ControllerError{Stage: "gateway-profile-unfit"}
	}
	return nil
}

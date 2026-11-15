package verificationflow

import (
	"context"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

type ControllerError struct{ Stage string }

func (e ControllerError) Error() string { return "verification stopped at " + e.Stage }

type BehaviorProof struct {
	Version               string               `json:"version"`
	Command               verification.Command `json:"command"`
	ToolchainImage        string               `json:"toolchainImage"`
	BaselineResultSHA256  string               `json:"baselineResultSHA256"`
	CandidateResultSHA256 string               `json:"candidateResultSHA256"`
	ReferenceResultSHA256 string               `json:"referenceResultSHA256"`
	ResourceUIDs          map[string]string    `json:"resourceUIDs"`
	BehaviorVerified      bool                 `json:"behaviorVerified"`
	PublicationAuthorized bool                 `json:"publicationAuthorized"`
}

// Run consumes only an already signed durable inbox identifier. It never
// accepts arbitrary commands, adopts claims, invokes a model, or publishes code.
func Run(ctx context.Context, db *store.DB, load PolicyLoader, id string, client *podtransport.Client, image string) (proof BehaviorProof, err error) {
	if db == nil || load == nil || client == nil {
		return proof, ControllerError{"configuration"}
	}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	c, record, snapshot, err := CurrentProof(ctx, db, load, id)
	if err != nil {
		return proof, ControllerError{"authority"}
	}
	request, err := db.ReadVerification(ctx, id)
	if err != nil {
		return proof, ControllerError{"request"}
	}
	claim, err := db.ClaimVerification(ctx, id, request.PayloadDigest)
	if err != nil {
		return proof, ControllerError{"claim"}
	}
	ctx, claimCancel := context.WithDeadline(ctx, claim.Until)
	defer claimCancel()
	stage := "authority"
	var resources *podtransport.VerificationResources
	configUID := ""
	pods := map[string]string{}
	cleaned := false
	cleanupAttempted := false
	cleanup := func() error {
		cleanupAttempted = true
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		failed := false
		if resources != nil {
			for kind, uid := range pods {
				if resources.DeletePod(cleanupCtx, kind, uid) != nil {
					failed = true
				}
			}
			if configUID != "" && resources.DeleteConfig(cleanupCtx, configUID) != nil {
				failed = true
			}
		}
		if failed {
			return ControllerError{"cleanup"}
		}
		if db.ConfirmVerificationCleanup(cleanupCtx, claim) != nil {
			return ControllerError{"cleanup"}
		}
		cleaned = true
		return nil
	}
	defer func() {
		if recover() != nil {
			err = ControllerError{stage}
		}
		if !cleaned && !cleanupAttempted {
			if e := cleanup(); e != nil {
				err = e
			}
		}
		if err != nil {
			proof = BehaviorProof{}
			stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = db.StopVerification(stopCtx, claim, "uncertain")
		}
	}()
	fence := func() error {
		current, _, _, e := CurrentProof(ctx, db, load, id)
		if e != nil || current != c || db.CheckVerificationClaim(ctx, claim) != nil {
			return verification.ErrVerification
		}
		return nil
	}
	if fence() != nil {
		return proof, ControllerError{stage}
	}
	stage = "source"
	sourceClient, e := sourcefetch.New(c.Repository, "")
	if e != nil {
		return proof, ControllerError{stage}
	}
	source, e := sourceClient.Fetch(ctx, c.SourceSHA)
	if e != nil {
		return proof, ControllerError{stage}
	}
	stage = "recipe"
	inputs, e := verification.Prepare(c.Repository, c.Recipe, snapshot.Issue.Body, source, record.Delta)
	if e != nil {
		return proof, ControllerError{stage}
	}
	resources, e = podtransport.PlanVerification(client, c, inputs, image)
	if e != nil {
		return proof, ControllerError{stage}
	}
	create := func(kind string, apply func() (string, error)) error {
		if fence() != nil {
			return verification.ErrVerification
		}
		effect, e := db.BeginVerificationEffect(ctx, claim, kind)
		if e != nil {
			return e
		}
		if fence() != nil {
			unsentCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			// Cleanup requires an observable durable cancellation. A restart
			// must never infer no-send merely from the missing UID.
			_ = db.CancelUnsentVerificationEffect(unsentCtx, effect)
			stop()
			return verification.ErrVerification
		}
		uid, e := apply()
		if uid != "" {
			if kind == "config" {
				configUID = uid
			} else {
				pods[kind] = uid
			}
		}
		if e != nil {
			return e
		}
		if db.AcknowledgeVerificationEffect(ctx, effect, uid) != nil {
			return verification.ErrVerification
		}
		return fence()
	}
	stage = "config"
	if create("config", func() (string, error) { return resources.CreateConfig(ctx) }) != nil {
		return proof, ControllerError{stage}
	}
	for _, kind := range []string{"baseline", "candidate", "reference", "tests"} {
		stage = kind
		if create(kind, func() (string, error) { return resources.CreatePod(ctx, kind, configUID) }) != nil {
			return proof, ControllerError{stage}
		}
	}
	stage = "results"
	if fence() != nil {
		return proof, ControllerError{stage}
	}
	results, e := resources.ReadResults(ctx, pods)
	if e != nil {
		return proof, ControllerError{stage}
	}
	stage = "comparison"
	if verification.Compare(results.Baseline, results.Candidate, results.Reference) != nil {
		return proof, ControllerError{stage}
	}
	if fence() != nil {
		return proof, ControllerError{"authority"}
	}
	stage = "cleanup"
	if cleanup() != nil {
		return proof, ControllerError{stage}
	}
	if fence() != nil {
		return proof, ControllerError{"authority"}
	}
	uids := map[string]string{"config": configUID}
	for kind, uid := range pods {
		uids[kind] = uid
	}
	proof = BehaviorProof{Version: "ai-sdlc.behavior-proof/v1", Command: c, ToolchainImage: image, BaselineResultSHA256: authority.Digest(results.Baseline), CandidateResultSHA256: authority.Digest(results.Candidate), ReferenceResultSHA256: authority.Digest(results.Reference), ResourceUIDs: uids, BehaviorVerified: true, PublicationAuthorized: false}
	raw, _ := json.Marshal(proof)
	if db.FinishVerification(ctx, claim, raw) != nil {
		return BehaviorProof{}, ControllerError{"commit"}
	}
	return proof, nil
}

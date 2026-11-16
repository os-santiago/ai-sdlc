package store

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
	"time"
)

// BuildInput is a read-only historical projection. Current policies, issue
// eligibility, bounded storage and build profile are separate requirements.
type BuildInput struct {
	Source                                                        MergedBuildSource
	PublicationID, WorkItemID, IssueRevision, InnerPolicyRevision string
	Generation                                                    int64
}

func (db *DB) ReadBuildInput(ctx context.Context, id string) (BuildInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readBuildInput(ctx, db.Pool, id, "release-manager")
}
func (db *DB) ReadBuilderInput(ctx context.Context, id string) (BuildInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readBuildInput(ctx, db.Pool, id, "build-controller")
}
func readBuildInput(ctx context.Context, q Querier, id, role string) (BuildInput, error) {
	r, e := readMergedRecord(ctx, q, id, role)
	if e != nil {
		return BuildInput{}, e
	}
	var merged merging.Command
	var effect MergeEvidence
	var policy authority.Policy
	if authority.DecodeStrict(r.Payload, &merged) != nil || authority.DecodeStrict(r.Effect, &effect) != nil || authority.DecodeStrict(r.Policy, &policy) != nil {
		return BuildInput{}, ErrExecution
	}
	pub, e := readPublishedRecordAs(ctx, q, merged.PublicationID, role)
	if e != nil || pub.Digest() != merged.PublicationRecordSHA256 {
		return BuildInput{}, ErrExecution
	}
	// The complete immutable payload is signed and bound by the merged record.
	// Decode only this stage's projection without importing the verifier/store
	// dependency cycle. DecodeStrict(map) still rejects duplicate JSON keys.
	var fields map[string]json.RawMessage
	var signed authority.Receipt
	var original struct {
		Version           string `json:"version"`
		Repository        string `json:"repository"`
		WorkItemID        string `json:"workItemId"`
		IssueNumber       int    `json:"issueNumber"`
		IssueRevision     string `json:"issueRevision"`
		Generation        int64  `json:"generation"`
		SourceSHA         string `json:"sourceSHA"`
		AuthorityRevision string `json:"authorityRevision"`
	}
	if authority.DecodeStrict(pub.Payload, &fields) != nil || json.Unmarshal(pub.Payload, &original) != nil || authority.DecodeStrict(pub.Receipt, &signed) != nil ||
		original.Version != "ai-sdlc.publication-command/v1" || original.Repository != merged.Repository || original.IssueNumber != merged.IssueNumber || original.SourceSHA != merged.BaseSHA || original.AuthorityRevision != merged.AuthorityRevision || original.Generation < 1 || !modelDigest.MatchString(original.IssueRevision) || !lifecycleUUID.MatchString(original.WorkItemID) ||
		signed.ID != merged.PublicationID || signed.ID != "publication-"+authority.Digest(pub.Payload) || policy.VerifyReceipt(signed, "release-manager", "publish-candidate", pub.Payload) != nil {
		return BuildInput{}, ErrExecution
	}
	return BuildInput{Source: MergedBuildSource{Version: "ai-sdlc.merged-build-source/v1", MergeID: id, MergeRecordSHA256: r.Digest(), Repository: merged.Repository, IssueNumber: merged.IssueNumber, PullRequest: merged.PullRequest, SourceSHA: effect.Receipt.CommitSHA, TreeSHA: effect.Receipt.TreeSHA, HeadSHA: effect.Receipt.HeadSHA, BaseSHA: effect.Receipt.BaseSHA, MergedAt: r.MergedAt.UTC()}, PublicationID: merged.PublicationID, WorkItemID: original.WorkItemID, IssueRevision: original.IssueRevision, InnerPolicyRevision: original.AuthorityRevision, Generation: original.Generation}, nil
}

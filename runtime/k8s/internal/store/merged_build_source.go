package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
)

// MergedBuildSource describes verified historical input, not permission to
// build, publish, release or deploy. A build controller must acquire separate
// current authority and bind its recipe, destination and budgets to this fact.
// SourceSHA is the actual merge commit, never the PR head or its old base.
type MergedBuildSource struct {
	Version           string    `json:"version"`
	MergeID           string    `json:"mergeID"`
	MergeRecordSHA256 string    `json:"mergeRecordSHA256"`
	Repository        string    `json:"repository"`
	IssueNumber       int       `json:"issueNumber"`
	PullRequest       int       `json:"pullRequest"`
	SourceSHA         string    `json:"sourceSHA"`
	TreeSHA           string    `json:"treeSHA"`
	HeadSHA           string    `json:"headSHA"`
	BaseSHA           string    `json:"baseSHA"`
	MergedAt          time.Time `json:"mergedAt"`
}

// ReadMergedBuildSource intentionally accepts no caller-supplied source SHA or
// forge result. The immutable record reader verifies the original signed
// authority at completion, terminal state, receipt and evidence digests. It
// remains readable after the merge claim expires, without extending that claim.
func (db *DB) ReadMergedBuildSource(ctx context.Context, id string) (MergedBuildSource, error) {
	r, err := db.ReadMergedRecord(ctx, id)
	if err != nil {
		return MergedBuildSource{}, err
	}
	var c merging.Command
	var effect MergeEvidence
	if authority.DecodeStrict(r.Payload, &c) != nil || authority.DecodeStrict(r.Effect, &effect) != nil {
		return MergedBuildSource{}, ErrExecution
	}
	return MergedBuildSource{
		Version: "ai-sdlc.merged-build-source/v1", MergeID: r.MergeID,
		MergeRecordSHA256: r.Digest(), Repository: c.Repository,
		IssueNumber: c.IssueNumber, PullRequest: effect.Receipt.PullRequest,
		SourceSHA: effect.Receipt.CommitSHA, TreeSHA: effect.Receipt.TreeSHA,
		HeadSHA: effect.Receipt.HeadSHA, BaseSHA: effect.Receipt.BaseSHA,
		MergedAt: r.MergedAt.UTC(),
	}, nil
}

// Digest binds the entire immutable merge record, including signed authority,
// gate observation, effect and original timestamps. Hashing a caller-constructed
// record does not validate it; consumers must use ReadMergedRecord or the source
// projection above before interpreting this digest as evidence.
func (r MergedRecord) Digest() string {
	r.MergedAt = r.MergedAt.UTC()
	r.AuthorityExpiresAt = r.AuthorityExpiresAt.UTC()
	raw, _ := json.Marshal(r)
	return authority.Digest(raw)
}

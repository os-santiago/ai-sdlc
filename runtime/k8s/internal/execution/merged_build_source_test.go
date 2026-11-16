package execution

import (
	"context"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestBuildSourceRequiresCompletedMergeAndActualCommit(t *testing.T) {
	f, db, r, cmd := mergeJournalFixture(t)
	ctx := context.Background()
	if source, err := db.ReadMergedBuildSource(ctx, r.ID); err == nil || source != (store.MergedBuildSource{}) {
		t.Fatal("publication without merge became build input")
	}
	claim := queuedMerge(t, db, r)
	proof := completeMerge(t, db, claim, cmd)
	if _, err := db.ReadMergedBuildSource(ctx, r.ID); err == nil {
		t.Fatal("acknowledged effect without atomic completion became build input")
	}
	if err := db.FinishMerge(ctx, claim, proof); err != nil {
		t.Fatal(err)
	}
	source, err := db.ReadMergedBuildSource(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := db.ReadMergedRecord(ctx, r.ID)
	if err != nil || source.Version != "ai-sdlc.merged-build-source/v1" || source.MergeID != r.ID || source.MergeRecordSHA256 != fact.Digest() || source.Repository != cmd.Repository || source.IssueNumber != cmd.IssueNumber || source.PullRequest != cmd.PullRequest || source.SourceSHA != mergeReceipt(cmd).CommitSHA || source.SourceSHA == cmd.HeadSHA || source.SourceSHA == cmd.BaseSHA || source.TreeSHA != cmd.TreeSHA || source.HeadSHA != cmd.HeadSHA || source.BaseSHA != cmd.BaseSHA || !source.MergedAt.Equal(fact.MergedAt) {
		t.Fatal("build input is not bound to actual merged source", err)
	}
	if _, err := f.db.ReadMergedBuildSource(ctx, r.ID); err == nil {
		t.Fatal("worker obtained outerloop build input")
	}
	if _, err := db.ClaimMerge(ctx, r.ID); err == nil || db.CheckMerge(ctx, claim) == nil {
		t.Fatal("reading build source renewed completed merge authority")
	}
}

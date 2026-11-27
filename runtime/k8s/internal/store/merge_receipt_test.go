package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// An observed merge receipt (from reconciliation) is never accepted where a
// live one is required: the acknowledgement, the merge proof and the merged
// record reader all use liveMergeReceipt.
func TestLiveMergeBoundariesRefuseObservedReceipt(t *testing.T) {
	live := MergeReceipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: strings.Repeat("d", 40), HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), PullRequest: 42}
	if !liveMergeReceipt(live) || !validMergeReceipt(live) {
		t.Fatal("live receipt refused")
	}
	observed := live
	observed.Observed = true
	if liveMergeReceipt(observed) || !validMergeReceipt(observed) {
		t.Fatal("observed receipt accepted as live")
	}
	// Both refuse before any database access.
	j := &MergeJournal{}
	if j.AcknowledgeMergeEffect(context.Background(), MergeEffect{Token: "00000000-0000-4000-8000-000000000000", RequestDigest: strings.Repeat("f", 64)}, observed) == nil {
		t.Fatal("acknowledged an observed receipt")
	}
	proof := `{"version":"ai-sdlc.merge-proof/v1","mergeID":"merge-` + strings.Repeat("1", 64) + `","publicationID":"publication-` + strings.Repeat("5", 64) + `","publicationRecordSHA256":"` + strings.Repeat("2", 64) + `","gateSHA256":"` + strings.Repeat("3", 64) + `","effectSHA256":"` + strings.Repeat("4", 64) + `","receipt":{"version":"ai-sdlc.merge-receipt/v1","commitSHA":"` + live.CommitSHA + `","headSHA":"` + live.HeadSHA + `","baseSHA":"` + live.BaseSHA + `","treeSHA":"` + live.TreeSHA + `","pullRequest":42,"observed":true},"deploymentAuthorized":false}`
	if j.FinishMerge(context.Background(), MergeClaim{ID: "merge-" + strings.Repeat("1", 64), RecordDigest: strings.Repeat("2", 64), GateDigest: strings.Repeat("3", 64)}, []byte(proof)) == nil {
		t.Fatal("finished with an observed receipt")
	}
}

// The observed marker must never change a live receipt: its bytes, and so the
// receipt digest every merge effect and proof binds, are exactly those written
// before reconciliation existed.
func TestLiveMergeReceiptBytesAreUnchanged(t *testing.T) {
	live := MergeReceipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: strings.Repeat("d", 40), HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), PullRequest: 42}
	raw, e := json.Marshal(live)
	if e != nil {
		t.Fatal(e)
	}
	want := `{"version":"ai-sdlc.merge-receipt/v1","commitSHA":"` + live.CommitSHA + `","headSHA":"` + live.HeadSHA + `","baseSHA":"` + live.BaseSHA + `","treeSHA":"` + live.TreeSHA + `","pullRequest":42}`
	if string(raw) != want {
		t.Fatalf("live receipt bytes changed:\n got %s\nwant %s", raw, want)
	}
	if got := authority.Digest(raw); got != "9939cefa4557916534c1f9f3573e9b23073bf0da35d5dd66ec71ae2bd9554b06" {
		t.Fatal("live receipt digest changed", got)
	}
	observed := live
	observed.Observed = true
	if raw, _ = json.Marshal(observed); string(raw) != strings.TrimSuffix(want, "}")+`,"observed":true}` {
		t.Fatal("observed receipt bytes", string(raw))
	}
}

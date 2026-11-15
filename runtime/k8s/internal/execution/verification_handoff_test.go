package execution

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

func TestCandidateHandoffIsSignedAtomicAndNotReplayed(t *testing.T) {
	f, l, _, raw := candidateFixture(t, func(f *fixture) {
		f.Material.Policy.Repository = "os-santiago/homedir"
		f.Config.Repository = f.Material.Policy.Repository
		f.command.Repository = f.Material.Policy.Repository
		f.command.AuthorityRevision = f.Material.Policy.Revision()
		snapshot := f.Snapshot(1, true)
		snapshot.Issue.Body += "\n" + verification.RecipeMarker + "\n"
		snapshot.Revision = snapshot.ContentRevision()
		f.seedReadinessSnapshot(t, snapshot)
	})
	ctx := context.Background()
	captured, err := l.CaptureCandidate(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON verification_handoffs TO aisdlc_worker; GRANT UPDATE(cleanup_state) ON execution_lifecycles TO aisdlc_worker`); err != nil {
		t.Fatal(err)
	}
	var configUID, podUID string
	if err = f.Admin.Pool.QueryRow(`SELECT config_uid,pod_uid FROM execution_lifecycles WHERE request_id=$1`, captured.RequestID).Scan(&configUID, &podUID); err != nil {
		t.Fatal(err)
	}
	load := func() (authority.Policy, authority.Identity, intake.Config, string, error) {
		return f.Material.Policy, f.Material.Identities["worker"], f.Config, f.command.ProfileSHA256, nil
	}
	if sent, err := HandoffCandidate(ctx, f.db, load); err != nil || sent {
		t.Fatal("uncleaned artifact dispatched", err)
	}
	if err = f.db.ConfirmExecutionCleanup(ctx, l.durable, configUID, podUID); err != nil {
		t.Fatal(err)
	}
	if sent, err := HandoffCandidate(ctx, f.db, load); err != nil || !sent {
		t.Fatal("signed handoff", err)
	}
	if sent, err := HandoffCandidate(ctx, f.db, load); err != nil || sent {
		t.Fatal("handoff replay", err)
	}
	var payload, receiptRaw []byte
	if err = f.Admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE producer='worker' AND operation='verify-artifact'`).Scan(&payload, &receiptRaw); err != nil {
		t.Fatal(err)
	}
	var receipt authority.Receipt
	if json.Unmarshal(receiptRaw, &receipt) != nil {
		t.Fatal("receipt")
	}
	command, err := verification.Verify(f.Material.Policy, receipt, payload, time.Now())
	if err != nil || command.CandidateID != captured.RequestID || command.DeltaSHA256 != captured.DeltaSHA256 {
		t.Fatal("outbox binding", err)
	}
	if f.Count(t, `SELECT count(*) FROM verification_handoffs WHERE state='sent'`) != 1 {
		t.Fatal("missing atomic disposition")
	}
	// A worker cannot mutate a committed handoff, even with the fixture grants.
	if _, err = f.db.Pool.Exec(`UPDATE verification_handoffs SET state='rejected'`); err == nil {
		t.Fatal("handoff mutated")
	}
}

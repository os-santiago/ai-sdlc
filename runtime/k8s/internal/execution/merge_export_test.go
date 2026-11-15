package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Exported only into the external test binary. All signatures, synthetic forge
// receipts and direct grants belong to its disposable PostgreSQL fixture.
type PublishedMergeTestFixture struct {
	Admin, DB                                                  *store.DB
	Policy                                                     authority.Policy
	Identity                                                   authority.Identity
	Intake                                                     intake.Config
	Profile, ReviewProfile, ReviewConfiguration, PublicationID string
}

func NewPublishedMergeTestFixture(t *testing.T, lifetime ...time.Duration) PublishedMergeTestFixture {
	t.Helper()
	f, db, r := publicationJournalFixture(t, func(f *fixture) {
		f.Material.Policy.Autonomy = "auto-merge-low"
		if len(lifetime) == 1 {
			f.command.ExpiresAt = time.Now().Add(lifetime[0]).UnixMilli()
		}
	})
	ctx := context.Background()
	c := publicationClaim(t, db, r)
	for _, kind := range []string{"tree", "commit", "branch", "pull-request"} {
		effect, e := db.BeginPublicationEffect(ctx, c, kind, authority.Digest([]byte(kind)))
		if e != nil {
			t.Fatal(e)
		}
		receipt := publicationReceipt(kind)
		if kind != "tree" {
			receipt.SHA = strings.Repeat("c", 40)
		}
		if db.AcknowledgePublicationEffect(ctx, effect, receipt) != nil {
			t.Fatal("fixture publication receipt")
		}
	}
	proof := publicationProof(t, db, c)
	proof["commitSHA"] = strings.Repeat("c", 40)
	raw, _ := json.Marshal(proof)
	if e := db.FinishPublication(ctx, c, raw); e != nil {
		t.Fatal(e)
	}
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if e := f.Admin.ProvisionRolesForCapabilities(ctx, passwords, "verification", true, true); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Admin.Pool.Exec(`GRANT SELECT,INSERT,UPDATE ON merge_requests,merge_effects TO aisdlc_release_manager; GRANT SELECT ON merged_records TO aisdlc_release_manager; GRANT INSERT(merge_id,payload_digest,payload,receipt,gate,policy,proof_digest,proof,effect_digest,effect,authority_expires_at) ON merged_records TO aisdlc_release_manager`); e != nil {
		t.Fatal(e)
	}
	return PublishedMergeTestFixture{f.Admin, db, f.Material.Policy, f.Material.Identities["release-manager"], f.Config, f.command.ProfileSHA256, strings.Repeat("b", 64), strings.Repeat("c", 64), r.ID}
}

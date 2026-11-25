package execution

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

func TestPublicationSelectionSkipsObsoleteProofWithoutChangingEvidence(t *testing.T) {
	for _, scenario := range []string{"authority", "intake", "execution-profile", "review-profile", "review-config", "recipe-revision", "generation", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f, db, run := reviewProofFixture(t, true)
			ctx := context.Background()
			if _, err := f.Admin.Pool.Exec(`GRANT SELECT ON publication_handoffs TO aisdlc_verifier`); err != nil {
				t.Fatal(err)
			}
			selection := store.PublicationSelection{AuthorityRevision: f.Material.Policy.Revision(), IntakeConfiguration: f.Config.Digest(), ExecutionProfile: f.command.ProfileSHA256, ReviewProfile: run.ProfileDigest, ReviewConfiguration: run.ConfigurationDigest,
				RecipeRevision: verification.CurrentRecipeRevision(f.Material.Policy.Repository)}
			before, err := db.ReadPassedReview(ctx, run.VerificationID)
			if err != nil {
				t.Fatal(err)
			}
			if id, err := db.NextPublicationHandoff(ctx, selection); err != nil || id != run.VerificationID {
				t.Fatal("current review not selected", id, err)
			}
			changed := selection
			stale := strings.Repeat("9", 64)
			switch scenario {
			case "authority":
				changed.AuthorityRevision = stale
			case "intake":
				changed.IntakeConfiguration = stale
			case "execution-profile":
				changed.ExecutionProfile = stale
			case "review-profile":
				changed.ReviewProfile = stale
			case "review-config":
				changed.ReviewConfiguration = stale
			case "recipe-revision":
				changed.RecipeRevision = stale
			case "generation":
				if _, err := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1 WHERE repository=$1 AND issue_number=$2`, f.command.Repository, f.command.IssueNumber); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				tx, err := f.Admin.Pool.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err = tx.Exec(`LOCK TABLE review_runs IN ACCESS EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
				if id, err := db.NextPublicationHandoff(ctx, changed); id != "" || !errors.Is(err, store.ErrObservationUnavailable) {
					t.Fatal("SQL outage hidden", id, err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "unavailable" {
				if id, err := db.NextPublicationHandoff(ctx, changed); err != nil || id != "" {
					t.Fatal("obsolete review selected", id, err)
				}
			}
			after, err := db.ReadPassedReview(ctx, run.VerificationID)
			if err != nil || !bytes.Equal(before.Proof, after.Proof) || !before.Run.ExpiresAt.Equal(after.Run.ExpiresAt) {
				t.Fatal("historical evidence changed", err)
			}
			if f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='publish-candidate'`) != 0 {
				t.Fatal("selector granted authority")
			}
		})
	}
}

package store

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestUpgradeFenceFeedbackPlannerBlockedHistory(t *testing.T) {
	if os.Getenv("AI_SDLC_FEEDBACK_PLANNER_FIXTURE") != "true" {
		t.Skip("requires its own disposable PostgreSQL instance for cluster-wide roles")
	}
	db, name, source := upgradeFenceFixture(t, 35)
	{
		ctx := context.Background()
		if _, err := db.Pool.Exec(browserBridgeHistory); err != nil {
			t.Fatal(err)
		}
		passwords := map[string]string{}
		for component := range componentTables {
			passwords[component] = strings.ReplaceAll(NewID(), "-", "") + strings.ReplaceAll(component, "-", "_")
		}
		if err := db.ProvisionRoles(ctx, passwords); err != nil {
			t.Fatal("owned fixture roles", err)
		}
		u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal("fixture connection")
		}
		u.Path = "/" + name
		u.User = url.UserPassword("aisdlc_orchestrator", passwords["orchestrator"])
		query := u.Query()
		for _, key := range []string{"dbname", "database", "search_path"} {
			query.Del(key)
		}
		u.RawQuery = query.Encode()
		planner, err := Open(ctx, u.String())
		if err != nil {
			t.Fatal("fixture planner identity")
		}
		t.Cleanup(func() { planner.Close() })
		tx, err := planner.Pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = BlockSupersededIssue(ctx, tx, "fixture/app", 1); err != nil {
			tx.Rollback()
			t.Fatal("actual planner withdrawal", err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var status string
		if err = db.Pool.QueryRow(`SELECT status FROM work_items WHERE id='contained-review'`).Scan(&status); err != nil || status != "blocked" {
			t.Fatal("planner did not block original candidate", err)
		}
		for _, count := range []int{35, 36} {
			if count == 36 {
				if err = db.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				source, err = MigrationCatalog()
				if err != nil {
					t.Fatal(err)
				}
			}
			before := upgradeHistory(t, db)
			if err = db.WithReviewFeedbackUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error { return f.Check(ctx) }); err != nil {
				t.Fatal("planner-blocked contained history rejected", err)
			}
			called := false
			if err = db.WithContainedReviewUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); err == nil || called {
				t.Fatal("historical reviewer protocol widened")
			}
			if err = db.WithBrowserDeploymentUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); err == nil || called {
				t.Fatal("historical browser protocol widened")
			}
			if upgradeHistory(t, db) != before {
				t.Fatal("maintenance changed blocked history")
			}
		}
	}
}

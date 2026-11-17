package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestUpgradeProtocolRejectsMalformedPrivateInput(t *testing.T) {
	catalog, err := store.MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]any{"database": "ai_sdlc", "databaseURL": "postgres://private-sentinel:secret-sentinel@[invalid", "sourceCatalog": catalog}
	raw, _ := json.Marshal(valid)
	for _, input := range []string{`{}`, `null`, `[]`, `{"database":"ai_sdlc","databaseURL":"private-sentinel","sourceCatalog":[]}`,
		`{"database":"ai_sdlc","database":"other","databaseURL":"private-sentinel","sourceCatalog":[]}`,
		strings.Replace(string(raw), `"database":`, `"Database":`, 1), string(raw), strings.Repeat(" ", 131073)} {
		var output bytes.Buffer
		err := inspectUpgradeMigrations(strings.NewReader(input), &output)
		if err == nil || err.Error() != "upgrade migration observation denied" || output.Len() != 0 {
			t.Fatal("invalid input leaked private data or emitted success")
		}
	}
}

func TestUpgradeProtocolAgainstRealDatabase(t *testing.T) {
	connection := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, connection)
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	defer db.Close()
	// This harness database is disposable. The observation itself never migrates.
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var database string
	if err = db.Pool.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"databaseURL": connection, "database": database, "sourceCatalog": catalog})
	var output bytes.Buffer
	if err = inspectUpgradeMigrations(bytes.NewReader(raw), &output); err != nil {
		t.Fatal(err)
	}
	var result store.UpgradeMigrationObservation
	if err = json.Unmarshal(output.Bytes(), &result); err != nil || result.State != "source-recorded" || result.UpgradeAuthorized || result.HistoricalSQLHashesVerified || result.SchemaIntegrityVerified || result.ProducerExclusionVerified || result.WorkflowEvidence || result.RollbackAuthorized || len(result.RemainingMigrations) != 0 {
		t.Fatal("unexpected protocol observation", err)
	}
	parsed, _ := url.Parse(connection)
	if strings.Contains(output.String(), connection) || parsed != nil && parsed.User != nil && strings.Contains(output.String(), parsed.User.String()) {
		t.Fatal("connection credential entered observation")
	}
}

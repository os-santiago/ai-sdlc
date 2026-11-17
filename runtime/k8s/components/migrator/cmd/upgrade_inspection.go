package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// The database connection is private stdin, never an argument or diagnostic.
// Source identities must come from the separately reviewed publication plan.
func inspectUpgradeMigrations(input io.Reader, output io.Writer) error {
	denied := errors.New("upgrade migration observation denied")
	raw, err := io.ReadAll(io.LimitReader(input, 131073))
	if err != nil || len(raw) > 131072 {
		return denied
	}
	var request struct {
		DatabaseURL   string                    `json:"databaseURL"`
		Database      string                    `json:"database"`
		SourceCatalog []store.MigrationIdentity `json:"sourceCatalog"`
	}
	if !exactInstallFields(raw, []string{"databaseURL", "database", "sourceCatalog"}, nil) || authority.DecodeStrict(raw, &request) != nil || request.DatabaseURL == "" || len(request.DatabaseURL) > 16384 || request.Database == "" || len(request.Database) > 63 || store.ValidateMigrationSource(request.SourceCatalog) != nil {
		return denied
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := store.Open(ctx, request.DatabaseURL)
	if err != nil {
		return denied
	}
	defer db.Close()
	report, err := db.InspectUpgradeMigrations(ctx, request.Database, request.SourceCatalog)
	if err != nil {
		return denied
	}
	return json.NewEncoder(output).Encode(report)
}

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

// Private stdin contains the existing bootstrap connection, never command-line
// arguments or logs. No credential is generated or changed by inspection.
func inspectBootstrap(input io.Reader, output io.Writer) error {
	denied := errors.New("initial database inspection denied")
	raw, err := io.ReadAll(io.LimitReader(input, 16385))
	if err != nil || len(raw) > 16384 {
		return denied
	}
	var request struct {
		DatabaseURL string `json:"databaseURL"`
		Database    string `json:"database"`
	}
	if !exactInstallFields(raw, []string{"databaseURL", "database"}, nil) || authority.DecodeStrict(raw, &request) != nil || request.DatabaseURL == "" || request.Database == "" {
		return denied
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := store.Open(ctx, request.DatabaseURL)
	if err != nil {
		return denied
	}
	defer db.Close()
	if db.InspectEmptyBootstrap(ctx, request.Database) != nil {
		return denied
	}
	return json.NewEncoder(output).Encode(struct {
		Version            string    `json:"version"`
		Empty              bool      `json:"empty"`
		ObservedAt         time.Time `json:"observedAt"`
		DeliveryAuthorized bool      `json:"deliveryAuthorized"`
	}{"ai-sdlc.empty-bootstrap-observation/v1", true, time.Now().UTC(), false})
}

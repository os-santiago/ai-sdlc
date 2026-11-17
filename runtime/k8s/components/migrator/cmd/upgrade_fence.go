package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// This local installation protocol holds only database write exclusion. The
// installer must independently pause and inspect original Kubernetes producers.
// Private connection input is never echoed, persisted or used as CLI arguments.
func holdUpgradeFence(input io.ReadCloser, output io.WriteCloser) error {
	ctx, cancel := context.WithTimeout(context.Background(), 125*time.Second)
	defer cancel()
	return holdUpgradeFenceContext(ctx, input, output)
}

func holdUpgradeFenceContext(ctx context.Context, input io.ReadCloser, output io.WriteCloser) error {
	return holdSelectedUpgradeFence(ctx, input, output, legacyFence)
}

func holdReviewerUpgradeFence(input io.ReadCloser, output io.WriteCloser) error {
	ctx, cancel := context.WithTimeout(context.Background(), 125*time.Second)
	defer cancel()
	return holdSelectedUpgradeFence(ctx, input, output, reviewerFence)
}

type fenceSelection uint8

const (
	legacyFence fenceSelection = iota
	reviewerFence
	browserFence
)

func holdBrowserUpgradeFence(input io.ReadCloser, output io.WriteCloser) error {
	ctx, cancel := context.WithTimeout(context.Background(), 125*time.Second)
	defer cancel()
	return holdSelectedUpgradeFence(ctx, input, output, browserFence)
}

func holdSelectedUpgradeFence(ctx context.Context, input io.ReadCloser, output io.WriteCloser, selection fenceSelection) error {
	denied := errors.New("runtime upgrade fence denied")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	closed := make(chan struct{})
	go func() { defer close(closed); <-ctx.Done(); _ = input.Close(); _ = output.Close() }()
	defer func() { cancel(); <-closed }()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 131072)
	if !scanner.Scan() {
		return denied
	}
	raw := scanner.Bytes()
	var request struct {
		DatabaseURL   string                    `json:"databaseURL"`
		Database      string                    `json:"database"`
		SourceCatalog []store.MigrationIdentity `json:"sourceCatalog"`
	}
	if !exactInstallFields(raw, []string{"databaseURL", "database", "sourceCatalog"}, nil) || authority.DecodeStrict(raw, &request) != nil || request.DatabaseURL == "" || len(request.DatabaseURL) > 16384 || request.Database == "" || len(request.Database) > 63 || store.ValidateMigrationSource(request.SourceCatalog) != nil {
		return denied
	}
	db, err := store.Open(ctx, request.DatabaseURL)
	if err != nil {
		return denied
	}
	defer db.Close()
	type response struct {
		Version             string `json:"version"`
		State               string `json:"state"`
		Nonce               string `json:"nonce"`
		ExpiresAt           int64  `json:"expiresAt"`
		WorkflowRowsWritten bool   `json:"workflowRowsWritten"`
		UpgradeAuthorized   bool   `json:"upgradeAuthorized"`
	}
	nonce := store.NewID()
	var expiry int64
	released := false
	version := "ai-sdlc.upgrade-fence/v1"
	hold := db.WithPreCandidateUpgradeFence
	if selection == browserFence {
		version = "ai-sdlc.browser-upgrade-fence/v1"
		hold = db.WithBrowserDeploymentUpgradeFence
	} else if selection == reviewerFence {
		version = "ai-sdlc.reviewer-upgrade-fence/v1"
		hold = db.WithContainedReviewUpgradeFence
	} else if selection != legacyFence {
		return denied
	}
	emit := func(state string) error {
		return json.NewEncoder(output).Encode(response{Version: version, State: state, Nonce: nonce, ExpiresAt: expiry})
	}
	err = hold(ctx, request.Database, request.SourceCatalog, func(active context.Context, fence *store.UpgradeFence) error {
		// Loss of the held DB session must also unblock a waiting/blocked stream.
		stopped := make(chan struct{})
		stopWatch := make(chan struct{})
		go func() {
			defer close(stopped)
			select {
			case <-active.Done():
				cancel()
			case <-stopWatch:
			}
		}()
		defer func() { close(stopWatch); <-stopped }()
		expiry = fence.ExpiresAt().UnixMilli()
		if fence.Check(active) != nil || emit("held") != nil {
			return denied
		}
		for n := 0; n < 256; n++ {
			if !scanner.Scan() {
				return denied
			}
			var command struct {
				Operation string `json:"operation"`
				Nonce     string `json:"nonce"`
			}
			raw = scanner.Bytes()
			if !exactInstallFields(raw, []string{"operation", "nonce"}, nil) || authority.DecodeStrict(raw, &command) != nil || command.Nonce != nonce || fence.Check(active) != nil {
				return denied
			}
			switch command.Operation {
			case "check":
				if emit("held") != nil {
					return denied
				}
			case "release":
				released = true
				return nil
			default:
				return denied
			}
		}
		return denied
	})
	if errors.Is(err, store.ErrUpgradeFenceHistory) && expiry == 0 && ctx.Err() == nil {
		// No authority was acquired by the client. Do not expose SQL, table
		// names or private input, and never relabel transport/query failures.
		_ = json.NewEncoder(output).Encode(struct {
			Version             string `json:"version"`
			State               string `json:"state"`
			Reason              string `json:"reason"`
			WorkflowRowsWritten bool   `json:"workflowRowsWritten"`
			UpgradeAuthorized   bool   `json:"upgradeAuthorized"`
		}{Version: version, State: "rejected", Reason: "history"})
	}
	if err != nil || !released {
		return denied
	}
	return emit("released")
}

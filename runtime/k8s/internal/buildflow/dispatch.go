package buildflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type DispatchLoader func() (building.Configuration, authority.Identity, error)

// DispatchMerged is an internal release-manager operation. The caller cannot
// provide a merge, source, recipe or execution payload. Only current installed
// policy and immutable journal history can select and authorize one dispatch.
func DispatchMerged(ctx context.Context, db *store.DB, load DispatchLoader) (bool, error) {
	if db == nil || load == nil {
		return false, ErrBuildFlow
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, key, e := load()
	if e != nil || cfg.Validate() != nil || cfg.Inner.Freeze || cfg.Outer.Freeze || key.Role != "release-manager" {
		return false, ErrBuildFlow
	}
	id, e := db.NextMergedBuild(ctx, cfg)
	if e != nil || id == "" {
		return false, e
	}
	input, e := db.ReadBuildInput(ctx, id)
	if e != nil {
		return false, e
	}
	s := input.Source
	now := time.Now()
	c := building.Command{Version: building.Version, MergeID: id, MergeRecordSHA256: s.MergeRecordSHA256, Repository: s.Repository, Environment: cfg.Outer.Environment, IssueNumber: s.IssueNumber, IssueRevision: input.IssueRevision, Generation: input.Generation, SourceSHA: s.SourceSHA, TreeSHA: s.TreeSHA, Recipe: building.Recipe, ProfileSHA256: cfg.ProfileDigest(), IntakeConfigSHA256: cfg.IntakeConfigSHA256, InnerPolicyRevision: cfg.Inner.Revision(), OuterPolicyRevision: cfg.Outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(building.MaxAge).UnixMilli()}
	payload, _ := json.Marshal(c)
	current, currentKey, e := load()
	if e != nil || current.Validate() != nil || current.Inner.Revision() != cfg.Inner.Revision() || current.Outer.Revision() != cfg.Outer.Revision() || current.ProfileDigest() != cfg.ProfileDigest() || current.IntakeConfigSHA256 != cfg.IntakeConfigSHA256 || currentKey.KeyID != key.KeyID || currentKey.Role != key.Role {
		return false, ErrBuildFlow
	}
	signed, e := currentKey.Seal(current.Outer, c.ID(), "build-merged", payload)
	if e != nil {
		return false, ErrBuildFlow
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		if e := store.RecordBuildHandoff(ctx, tx, current, signed, payload); e != nil {
			return e
		}
		return delivery.Append(ctx, tx, current.Outer, currentKey, c.ID(), "build-merged", payload)
	})
	return e == nil, e
}

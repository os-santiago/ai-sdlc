package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
)

// runRepositoryTests is the entrypoint copied into an offline verification
// pod: it reads the reviewed verification settings from a read-only mount,
// runs the repository's test command once in the imported workspace and
// writes exactly one bounded result line to stdout. It opens no database,
// network or cluster connection and loads no signing identity.
//
//	component repository-tests-run <verification.json> <workspace> <scratch>
func runRepositoryTests(args []string, out io.Writer) error {
	if len(args) != 3 {
		return errors.New("usage: repository-tests-run <verification.json> <workspace> <scratch>")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return errors.New("verification settings unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, project.MaxManifestBytes+1))
	if err != nil || len(raw) > project.MaxManifestBytes {
		return errors.New("verification settings unavailable")
	}
	var v project.Verification
	if authority.DecodeStrict(raw, &v) != nil {
		return errors.New("invalid verification settings")
	}
	// Validate through a manifest wrapper so the runner accepts only settings
	// the reviewed manifest rules allow.
	check := project.Manifest{Version: project.ManifestVersion, Repository: "verification/settings", BaseBranch: "main",
		Verification: &v, Publication: project.Publication{Allow: []string{"**"}, MaxChangedFiles: 1, MaxChangedBytes: 1, MaxFileBytes: 1},
		Merge: project.Merge{RulesetID: 1, Required: []project.Check{{Context: "settings", AppID: 1}}}}
	if check.Validate() != nil {
		return errors.New("invalid verification settings")
	}
	result, err := repositorytests.Runner{Verification: v, Workspace: args[1], Scratch: args[2]}.Run(context.Background())
	if err != nil {
		return err
	}
	_, err = out.Write(append(result.Encode(), '\n'))
	return err
}

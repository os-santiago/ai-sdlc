// Package contract loads the subset of a repo's .ai-sdlc.yaml
// (spec/ai-sdlc.schema.json in the repo root) that the k8s/OpenShift control
// plane needs to derive component policy defaults.
//
// This is the integration point that keeps the cloud-native runtime
// spec-first (AGENTS.md: "every behavior is declared in .ai-sdlc.yaml; the
// runtime never hardcodes per-repo policy"). The GHA-native runtime reads
// the same file through its own workflow steps; this package is the
// equivalent entry point for components running as Kubernetes/OpenShift
// workloads instead of GitHub Actions runners.
//
// Only fields actually consumed by a ported component are modeled here —
// this is intentionally not a full mirror of ai-sdlc.schema.json. See
// docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md for the
// fields found to have no schema equivalent yet (a gap surfaced by this
// port, not invented by it).
package contract

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Contract is the decoded subset of .ai-sdlc.yaml this runtime consumes.
type Contract struct {
	Spec     string   `yaml:"spec"`
	Autonomy Autonomy `yaml:"autonomy"`
	Triggers Triggers `yaml:"triggers"`
	Verify   Verify   `yaml:"verify"`
	Review   Review   `yaml:"review"`
	Merge    Merge    `yaml:"merge"`
	Engine   Engine   `yaml:"engine"`
}

type Autonomy struct {
	Level               string `yaml:"level"`
	MergeRequiresChecks *bool  `yaml:"merge_requires_checks"`
	FreezeEnv           string `yaml:"freeze_env"`
}

type Triggers struct {
	IssueLabel         string                   `yaml:"issue_label"`
	SkipLabels         []string                 `yaml:"skip_labels"`
	AuthorizedLabelers []string                 `yaml:"authorized_labelers"`
	MinBodyLength      int                      `yaml:"min_body_length"`
	AcceptanceCriteria AcceptanceCriteriaBounds `yaml:"acceptance_criteria"`
}

// AcceptanceCriteriaBounds mirrors triggers.acceptance_criteria
// (spec/ai-sdlc.schema.json). Max is a pointer: nil means "unbounded",
// matching the schema's null default — the atomicity ceiling is opt-in,
// not retroactively enforced on repos that never configured one.
type AcceptanceCriteriaBounds struct {
	Min int  `yaml:"min"`
	Max *int `yaml:"max"`
}

type Verify struct {
	Commands       []string `yaml:"commands"`
	RequiredChecks []string `yaml:"required_checks"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

type Review struct {
	RiskLabels                 []string `yaml:"risk_labels"`
	CriticalRequiresAcceptance *bool    `yaml:"critical_requires_acceptance"`
	MaxFixIterations           *int     `yaml:"max_fix_iterations"`
	// MaxChangedFiles mirrors review.max_changed_files. nil = unbounded
	// (schema default) — the atomic-blast-radius ceiling is opt-in.
	MaxChangedFiles *int `yaml:"max_changed_files"`
}

type Merge struct {
	MaxRepairs *int `yaml:"max_repairs"`
}

type Engine struct {
	Name       string `yaml:"name"`
	MaxSteps   int    `yaml:"max_steps"`
	MaxSeconds int    `yaml:"max_seconds"`
}

// Default returns the schema-documented defaults (spec/ai-sdlc.schema.json)
// for every field this package models, so a component behaves identically
// whether a repo omits a field or sets it explicitly to the default.
func Default() Contract {
	criticalRequiresAcceptance := true
	maxFixIterations := 3
	maxRepairs := 3
	return Contract{
		Spec: "1.0.0",
		Autonomy: Autonomy{
			// shadow-first default (issue #186): an omitted
			// autonomy.level resolves to observe-only.
			Level:     "shadow",
			FreezeEnv: "AI_SDLC_FREEZE",
		},
		Triggers: Triggers{
			IssueLabel:         "ready-to-implement",
			MinBodyLength:      120, // spec/definition-of-ready.md R4; matches ai-sdlc-intake.yml's existing floor
			AcceptanceCriteria: AcceptanceCriteriaBounds{Min: 1, Max: nil},
		},
		Verify: Verify{
			TimeoutSeconds: 900,
		},
		Review: Review{
			RiskLabels:                 []string{"pr:risk-low", "pr:risk-medium", "pr:risk-high", "pr:risk-critical", "pr:risk-accepted"},
			CriticalRequiresAcceptance: &criticalRequiresAcceptance,
			MaxFixIterations:           &maxFixIterations,
		},
		Merge: Merge{MaxRepairs: &maxRepairs},
		Engine: Engine{
			Name:       "scc",
			MaxSteps:   400,
			MaxSeconds: 1500,
		},
	}
}

// LoadFromEnv reads the .ai-sdlc.yaml path named by AI_SDLC_CONTRACT_PATH.
// Every ported component calls this the same way, so a repo's contract
// drives policy identically regardless of which component reads it. Falls
// back to Default() when the env var is unset or the file cannot be parsed
// — a missing/invalid contract degrades a component to documented defaults,
// it never crashes a running pod.
func LoadFromEnv() Contract {
	path := os.Getenv("AI_SDLC_CONTRACT_PATH")
	if path == "" {
		return Default()
	}
	loaded, err := Load(path)
	if err != nil {
		return Default()
	}
	return loaded
}

// Load reads and parses a .ai-sdlc.yaml file at path, filling any field the
// file omits with its schema-documented default.
func Load(path string) (Contract, error) {
	contract := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return Contract{}, fmt.Errorf("read contract %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &contract); err != nil {
		return Contract{}, fmt.Errorf("parse contract %s: %w", path, err)
	}
	return contract, nil
}

// MaxFixIterationsOrDefault returns review.max_fix_iterations, or 3 (the
// schema default) when unset.
func (c Contract) MaxFixIterationsOrDefault() int {
	if c.Review.MaxFixIterations != nil {
		return *c.Review.MaxFixIterations
	}
	return 3
}

// MaxRepairsOrDefault returns merge.max_repairs, or 3 (the schema default)
// when unset.
func (c Contract) MaxRepairsOrDefault() int {
	if c.Merge.MaxRepairs != nil {
		return *c.Merge.MaxRepairs
	}
	return 3
}

// CriticalRequiresAcceptanceOrDefault returns
// review.critical_requires_acceptance, or true (the schema default) when
// unset.
func (c Contract) CriticalRequiresAcceptanceOrDefault() bool {
	if c.Review.CriticalRequiresAcceptance != nil {
		return *c.Review.CriticalRequiresAcceptance
	}
	return true
}

// MaxChangedFilesOrUnbounded returns review.max_changed_files, or 0 when
// unset. Callers treat 0 as "no ceiling enforced" (schema default: null),
// never as "zero files allowed".
func (c Contract) MaxChangedFilesOrUnbounded() int {
	if c.Review.MaxChangedFiles != nil {
		return *c.Review.MaxChangedFiles
	}
	return 0
}

// MaxAcceptanceCriteriaOrUnbounded returns triggers.acceptance_criteria.max,
// or 0 when unset. Callers treat 0 as "no ceiling enforced" (schema
// default: null), never as "zero criteria allowed".
func (c Contract) MaxAcceptanceCriteriaOrUnbounded() int {
	if c.Triggers.AcceptanceCriteria.Max != nil {
		return *c.Triggers.AcceptanceCriteria.Max
	}
	return 0
}

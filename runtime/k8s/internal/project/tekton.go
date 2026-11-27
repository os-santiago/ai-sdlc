package project

import (
	"regexp"
	"strings"
)

// Tekton declares which reviewed Tekton catalog pipelines run for a
// repository, and when: before generation (their bounded results become
// reviewed context for the generator), as generator tools (the generator may
// request them with a declared target), and after generation (a required CI
// check). It is optional and omitted when absent, so manifests without it keep
// their canonical form and digest. Pipelines and their tasks are installed by
// `./install.sh local --tekton-ci` (docs/architecture/tekton-task-integration.md).
type Tekton struct {
	// Modules are the Maven module directories a Maven pipeline may target,
	// each with its reviewed test services and Maven switches.
	Modules map[string]TektonModule `json:"modules,omitempty"`
	// NpmDirectories are the directories (with package-lock.json) an npm
	// pipeline may target.
	NpmDirectories []string           `json:"npmDirectories,omitempty"`
	Pre            []TektonInvocation `json:"pre,omitempty"`
	Tools          []TektonTool       `json:"tools,omitempty"`
	Post           []TektonInvocation `json:"post,omitempty"`
}

// TektonModule is one Maven module's reviewed test environment.
type TektonModule struct {
	Services  []string `json:"services"`
	MavenArgs []string `json:"mavenArgs,omitempty"`
}

// TektonInvocation runs one pipeline for one declared target. Check names the
// required check a post-generation run reports; it must be one of the merge
// section's required contexts.
type TektonInvocation struct {
	Pipeline string `json:"pipeline"`
	Target   string `json:"target"`
	Check    string `json:"check,omitempty"`
}

// TektonTool exposes one pipeline to the generator under a tool name. The
// generator chooses only a declared target and, for npm, validated package
// specifications; every other input comes from this manifest.
type TektonTool struct {
	Name     string `json:"name"`
	Pipeline string `json:"pipeline"`
}

// Tekton catalog pipelines and their target kinds.
const (
	TektonMavenBuildTest    = "maven-build-test"
	TektonMavenDependencies = "maven-dependencies"
	TektonNpmLockUpdate     = "npm-lock-update"
)

// Bounds of the Tekton section.
const (
	MaxTektonModules     = 16
	MaxTektonDirectories = 16
	MaxTektonInvocations = 8
	MaxTektonTools       = 4
	MaxTektonMavenArgs   = 16
)

var (
	// TektonServices are the sidecar services the catalog's Maven tasks provide.
	TektonServices = map[string]bool{"postgres": true, "mongodb": true}
	tektonPath     = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	tektonToolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// TektonMavenArg admits simple switches such as -Dtest=!UIResourceTests,!*IT.
	TektonMavenArg = regexp.MustCompile(`^-[A-Za-z][A-Za-z0-9_.:=,!*/@+-]{0,199}$`)
)

// TektonTargetPath reports whether value is a relative directory without dot
// segments (or "." when root is allowed).
func TektonTargetPath(value string, root bool) bool {
	if value == "." {
		return root
	}
	if !tektonPath.MatchString(value) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// TektonMavenArgs reports whether args are reviewed switches that change
// neither settings, POM, offline mode nor the local repository.
func TektonMavenArgs(args []string) bool {
	if len(args) > MaxTektonMavenArgs {
		return false
	}
	for _, arg := range args {
		if !TektonMavenArg.MatchString(arg) {
			return false
		}
		for _, prefix := range []string{"-s", "-gs", "-f", "-o", "-Dmaven.repo.local", "--settings", "--file", "--offline"} {
			if strings.HasPrefix(arg, prefix) {
				return false
			}
		}
	}
	return true
}

// TektonServiceList reports whether services is a non-empty, duplicate-free
// list of catalog services, or exactly ["none"].
func TektonServiceList(services []string) bool {
	if len(services) == 1 && services[0] == "none" {
		return true
	}
	seen := map[string]bool{}
	for _, service := range services {
		if !TektonServices[service] || seen[service] {
			return false
		}
		seen[service] = true
	}
	return len(services) > 0
}

// TargetKind is the target a pipeline accepts: a Maven module or an npm directory.
func TektonTargetKind(pipeline string) string {
	switch pipeline {
	case TektonMavenBuildTest, TektonMavenDependencies:
		return "module"
	case TektonNpmLockUpdate:
		return "npm"
	}
	return ""
}

func (t Tekton) validate(m Manifest) error {
	if len(t.Modules) > MaxTektonModules || len(t.NpmDirectories) > MaxTektonDirectories ||
		len(t.Pre) > MaxTektonInvocations || len(t.Post) > MaxTektonInvocations || len(t.Tools) > MaxTektonTools ||
		len(t.Pre)+len(t.Post)+len(t.Tools) == 0 {
		return ErrManifest
	}
	for name, module := range t.Modules {
		if !TektonTargetPath(name, true) || !TektonServiceList(module.Services) || !TektonMavenArgs(module.MavenArgs) {
			return ErrManifest
		}
	}
	directories := map[string]bool{}
	for _, directory := range t.NpmDirectories {
		if !TektonTargetPath(directory, false) || directories[directory] {
			return ErrManifest
		}
		directories[directory] = true
	}
	target := func(pipeline, value string) bool {
		switch TektonTargetKind(pipeline) {
		case "module":
			_, ok := t.Modules[value]
			return ok
		case "npm":
			return directories[value]
		}
		return false
	}
	for _, run := range t.Pre {
		// Dependencies fill the shared cache from trusted base inputs; the
		// pre-generation stage runs on the base revision, so it may use it.
		if !target(run.Pipeline, run.Target) || run.Check != "" {
			return ErrManifest
		}
	}
	checks := map[string]bool{}
	for _, required := range m.Merge.Required {
		checks[required.Context] = true
	}
	for _, run := range t.Post {
		// After generation the pipeline tests the candidate head: never the
		// cache-writing dependencies pipeline.
		if run.Pipeline == TektonMavenDependencies || !target(run.Pipeline, run.Target) || !checks[run.Check] {
			return ErrManifest
		}
	}
	names := map[string]bool{}
	for _, tool := range t.Tools {
		if !tektonToolName.MatchString(tool.Name) || names[tool.Name] || tool.Pipeline == TektonMavenDependencies ||
			TektonTargetKind(tool.Pipeline) == "" {
			return ErrManifest
		}
		if TektonTargetKind(tool.Pipeline) == "module" && len(t.Modules) == 0 ||
			TektonTargetKind(tool.Pipeline) == "npm" && len(directories) == 0 {
			return ErrManifest
		}
		names[tool.Name] = true
	}
	return nil
}

package tektontasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// MaxResultBytes bounds one task result; Tekton keeps at most 4096 bytes of
// results per TaskRun in the termination message.
const MaxResultBytes = 4096

// ErrOutcome reports a run whose status or results are not the bounded shape
// the catalog publishes; nothing from such a run is used.
var ErrOutcome = errors.New("tekton run outcome is not the bounded catalog shape")

// ErrDeadline reports a run that did not finish within the caller's bound.
var ErrDeadline = errors.New("tekton run did not finish within its bound")

var pipelineRuns = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}

// MavenSummary is the bounded result of maven-build-test and maven-dependencies.
type MavenSummary struct {
	Module              string   `json:"module"`
	MavenExitCode       int      `json:"mavenExitCode"`
	Reports             int      `json:"reports"`
	Tests               int      `json:"tests"`
	Failures            int      `json:"failures"`
	Errors              int      `json:"errors"`
	Skipped             int      `json:"skipped"`
	FailingTests        []string `json:"failingTests"`
	CompilerErrors      []string `json:"compilerErrors"`
	MissingDependencies bool     `json:"missingDependencies"`
	Truncated           bool     `json:"truncated"`
}

// Passed reports Maven's verdict; the task itself succeeds whenever it
// published a summary, so callers gate on this.
func (s MavenSummary) Passed() bool { return s.MavenExitCode == 0 }

// NpmChange is one changed lockfile entry.
type NpmChange struct {
	Path string  `json:"path"`
	From *string `json:"from"`
	To   *string `json:"to"`
	// ResolutionChanged marks an entry whose resolved URL or integrity
	// changed at the same version.
	ResolutionChanged bool `json:"resolutionChanged,omitempty"`
}

// NpmSummary is the bounded result of npm-lock-update.
type NpmSummary struct {
	Directory   string      `json:"directory"`
	Mode        string      `json:"mode"`
	NpmExitCode int         `json:"npmExitCode"`
	Changed     int         `json:"changed"`
	Changes     []NpmChange `json:"changes"`
	Truncated   bool        `json:"truncated"`
	// ResolvedHosts are the registry hosts the updated lockfile resolves
	// from; NewResolvedHosts are those it did not resolve from before.
	ResolvedHosts    []string `json:"resolvedHosts"`
	NewResolvedHosts []string `json:"newResolvedHosts"`
	DiffBytes        int      `json:"diffBytes"`
	DiffSHA256       string   `json:"diffSHA256"`
}

// Passed reports npm's verdict.
func (s NpmSummary) Passed() bool { return s.NpmExitCode == 0 }

var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func strict(raw string, value any) error {
	if len(raw) == 0 || len(raw) > MaxResultBytes {
		return ErrOutcome
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.More() {
		return ErrOutcome
	}
	return nil
}

// ParseMavenSummary decodes a Maven summary strictly and checks its bounds.
func ParseMavenSummary(raw string) (MavenSummary, error) {
	var s MavenSummary
	if strict(raw, &s) != nil || s.FailingTests == nil || s.CompilerErrors == nil || len(s.FailingTests) > 10 ||
		len(s.CompilerErrors) > 5 || s.MavenExitCode < 0 || s.MavenExitCode > 255 || s.Reports < 0 || s.Tests < 0 ||
		s.Failures < 0 || s.Errors < 0 || s.Skipped < 0 {
		return MavenSummary{}, ErrOutcome
	}
	return s, nil
}

// ParseNpmSummary decodes an npm summary strictly and checks its bounds.
func ParseNpmSummary(raw string) (NpmSummary, error) {
	var s NpmSummary
	if strict(raw, &s) != nil || s.Changes == nil || len(s.Changes) > 25 || s.Changed < len(s.Changes) ||
		s.ResolvedHosts == nil || s.NewResolvedHosts == nil || len(s.ResolvedHosts) > 10 || len(s.NewResolvedHosts) > 10 ||
		(s.Mode != "update" && s.Mode != "audit-fix") || !digest.MatchString(s.DiffSHA256) || s.DiffBytes < 0 {
		return NpmSummary{}, ErrOutcome
	}
	return s, nil
}

// Outcome is a finished run: its identity, verdict, checked-out commit and
// raw bounded summary (decode it with the pipeline's parser).
type Outcome struct {
	Name, UID string
	Succeeded bool
	Reason    string
	Commit    string
	Summary   string
}

// observe returns the outcome of a finished run, (false, nil) while running.
func observe(object map[string]any) (Outcome, bool, error) {
	run := unstructured.Unstructured{Object: object}
	conditions, _, _ := unstructured.NestedSlice(object, "status", "conditions")
	var status, reason string
	for _, entry := range conditions {
		condition, _ := entry.(map[string]any)
		if condition["type"] == "Succeeded" {
			status, _ = condition["status"].(string)
			reason, _ = condition["reason"].(string)
		}
	}
	if status != "True" && status != "False" {
		return Outcome{}, false, nil
	}
	outcome := Outcome{Name: run.GetName(), UID: string(run.GetUID()), Succeeded: status == "True", Reason: reason}
	results, _, _ := unstructured.NestedSlice(object, "status", "results")
	for _, entry := range results {
		result, _ := entry.(map[string]any)
		value, ok := result["value"].(string)
		if !ok || len(value) > MaxResultBytes {
			return Outcome{}, true, ErrOutcome
		}
		switch result["name"] {
		case "commit":
			outcome.Commit = value
		case "summary":
			outcome.Summary = value
		}
	}
	if outcome.Commit != "" && !revision.MatchString(outcome.Commit) {
		return Outcome{}, true, ErrOutcome
	}
	return outcome, true, nil
}

// Cluster creates, reads and cancels PipelineRuns in the CI namespace.
type Cluster interface {
	Create(ctx context.Context, object map[string]any) (string, error)
	Get(ctx context.Context, name string) (map[string]any, error)
	Cancel(ctx context.Context, name string) error
}

// DynamicCluster implements Cluster with a dynamic client whose identity may
// only create and read PipelineRuns in the CI namespace.
type DynamicCluster struct{ Client dynamic.Interface }

func (d DynamicCluster) Create(ctx context.Context, object map[string]any) (string, error) {
	created, err := d.Client.Resource(pipelineRuns).Namespace(Namespace).Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return created.GetName(), nil
}

// Cancel asks Tekton to stop the run (spec.status Cancelled).
func (d DynamicCluster) Cancel(ctx context.Context, name string) error {
	_, err := d.Client.Resource(pipelineRuns).Namespace(Namespace).Patch(ctx, name, types.MergePatchType,
		[]byte(`{"spec":{"status":"Cancelled"}}`), metav1.PatchOptions{})
	return err
}

func (d DynamicCluster) Get(ctx context.Context, name string) (map[string]any, error) {
	run, err := d.Client.Resource(pipelineRuns).Namespace(Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return run.Object, nil
}

// Run creates the request's PipelineRun once and polls it until it finishes
// or ctx ends, then cancels it so candidate code does not keep running
// unobserved. A failed read is retried at the next poll; the deadline bounds
// everything. A succeeded run must report exactly the requested commit, and a
// failed one may report none (checkout failed) but never another. Outcome
// Succeeded is the pipeline's status; the tool's verdict is in the summary
// (MavenSummary.Passed, NpmSummary.Passed).
func Run(ctx context.Context, cluster Cluster, r Request, runtimeClass string, poll time.Duration) (Outcome, error) {
	object, err := r.PipelineRun(runtimeClass)
	if err != nil {
		return Outcome{}, err
	}
	name, err := cluster.Create(ctx, object)
	if err != nil || name == "" {
		return Outcome{}, errors.Join(ErrOutcome, err)
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if current, err := cluster.Get(ctx, name); err == nil {
			outcome, done, err := observe(current)
			if err != nil {
				return Outcome{}, err
			}
			if done {
				if outcome.Name != name || (outcome.Succeeded && outcome.Commit != r.Revision) ||
					(outcome.Commit != "" && outcome.Commit != r.Revision) {
					return Outcome{}, ErrOutcome
				}
				return outcome, nil
			}
		}
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := cluster.Cancel(cancelCtx, name)
			cancel()
			return Outcome{}, errors.Join(ErrDeadline, err)
		case <-ticker.C:
		}
	}
}

package provenance

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
)

func bindImage(raw []byte, r buildrecord.Record) error {
	var p struct {
		Critical struct {
			Identity struct {
				Reference string `json:"docker-reference"`
			} `json:"identity"`
			Image struct {
				Digest string `json:"docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
		Optional map[string]json.RawMessage `json:"optional"`
	}
	if strictObject(raw, &p) != nil || p.Critical.Type != "cosign container image signature" || p.Critical.Identity.Reference != r.Result.ImageURL || p.Critical.Image.Digest != r.Result.ImageDigest {
		return ErrProvenance
	}
	return nil
}

type material struct {
	URI    string            `json:"uri"`
	Digest map[string]string `json:"digest"`
}
type signedStep struct {
	Annotations json.RawMessage   `json:"annotations"`
	Arguments   []string          `json:"arguments"`
	EntryPoint  string            `json:"entryPoint"`
	Environment map[string]string `json:"environment"`
}
type invocation struct {
	ConfigSource json.RawMessage   `json:"configSource"`
	Parameters   map[string]string `json:"parameters"`
	Environment  struct {
		Annotations map[string]string `json:"annotations"`
		Labels      map[string]string `json:"labels"`
	} `json:"environment"`
}
type task struct {
	FinishedOn string          `json:"finishedOn"`
	Invocation invocation      `json:"invocation"`
	Name       string          `json:"name"`
	Ref        json.RawMessage `json:"ref"`
	Results    []struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"results"`
	ServiceAccountName string       `json:"serviceAccountName"`
	StartedOn          string       `json:"startedOn"`
	Status             string       `json:"status"`
	Steps              []signedStep `json:"steps"`
}
type statement struct {
	Type    string `json:"_type"`
	Subject []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	PredicateType string `json:"predicateType"`
	Predicate     struct {
		BuildConfig struct {
			Tasks []task `json:"tasks"`
		} `json:"buildConfig"`
		BuildType  string            `json:"buildType"`
		Builder    map[string]string `json:"builder"`
		Invocation json.RawMessage   `json:"invocation"`
		Materials  []material        `json:"materials"`
		Metadata   json.RawMessage   `json:"metadata"`
	} `json:"predicate"`
}

func bindStatement(raw []byte, r buildrecord.Record) error {
	var s statement
	if strictObject(raw, &s) != nil || s.Type != "https://in-toto.io/Statement/v0.1" || s.PredicateType != PredicateType || len(s.Subject) != 1 ||
		s.Subject[0].Name != r.Result.ImageURL || !reflect.DeepEqual(s.Subject[0].Digest, map[string]string{"sha256": strings.TrimPrefix(r.Result.ImageDigest, "sha256:")}) {
		return ErrProvenance
	}
	p := s.Predicate
	if p.BuildType != "tekton.dev/v1/PipelineRun" || !reflect.DeepEqual(p.Builder, map[string]string{"id": "https://tekton.dev/chains/v2"}) || len(p.BuildConfig.Tasks) != 1 {
		return ErrProvenance
	}
	t := p.BuildConfig.Tasks[0]
	expectedResults := map[string]string{"IMAGE_URL": r.Result.ImageURL, "IMAGE_DIGEST": r.Result.ImageDigest, "CHAINS-GIT_URL": "https://github.com/" + r.Command.Repository, "CHAINS-GIT_COMMIT": r.Command.SourceSHA, "DEFINITION_DIGEST": "sha256:" + r.Plan.DefinitionSHA256, "RUN_UID": r.Publish.PipelineRunUID}
	results := map[string]string{}
	for _, item := range t.Results {
		if item.Type != "string" {
			return ErrProvenance
		}
		if _, exists := results[item.Name]; exists {
			return ErrProvenance
		}
		results[item.Name] = item.Value
	}
	if t.Name != "publish" || t.Status != "Succeeded" || t.ServiceAccountName != "publish" || !reflect.DeepEqual(results, expectedResults) ||
		!reflect.DeepEqual(t.Invocation.Parameters, map[string]string{"run-uid": r.Publish.PipelineRunUID}) || t.Invocation.Environment.Labels["tekton.dev/pipelineRunUID"] != r.Publish.PipelineRunUID {
		return ErrProvenance
	}
	publisher := strings.Split(r.Result.PublisherImage, "@sha256:")
	if len(publisher) != 2 || len(p.Materials) != 2 {
		return ErrProvenance
	}
	expectedMaterials := []material{{"oci://" + publisher[0], map[string]string{"sha256": publisher[1]}}, {"git+https://github.com/" + r.Command.Repository + ".git", map[string]string{"sha1": r.Command.SourceSHA}}}
	if !(reflect.DeepEqual(p.Materials, expectedMaterials) || reflect.DeepEqual(p.Materials, []material{expectedMaterials[1], expectedMaterials[0]})) {
		return ErrProvenance
	}
	// Expectations come from the validated closed compiler bytes, not the signed
	// statement's own claims. Historical validation binds the build's source tree.
	var request struct {
		Spec struct {
			PipelineSpec struct {
				Tasks []struct {
					TaskSpec struct {
						Steps []struct {
							Name    string   `json:"name"`
							Image   string   `json:"image"`
							Command []string `json:"command"`
							Args    []string `json:"args"`
						} `json:"steps"`
					} `json:"taskSpec"`
				} `json:"tasks"`
			} `json:"pipelineSpec"`
		} `json:"spec"`
	}
	if json.Unmarshal(r.Context.Publish, &request) != nil || len(request.Spec.PipelineSpec.Tasks) != 1 {
		return ErrProvenance
	}
	expected := request.Spec.PipelineSpec.Tasks[0].TaskSpec.Steps
	if len(expected) != 2 || len(t.Steps) != 2 {
		return ErrProvenance
	}
	for i, step := range t.Steps {
		wanted := expected[i]
		args := append([]string(nil), wanted.Args...)
		for n, arg := range args {
			args[n] = strings.ReplaceAll(arg, "$(params.run-uid)", r.Publish.PipelineRunUID)
		}
		if !reflect.DeepEqual(wanted.Command, []string{"/component"}) || step.EntryPoint != "/component" || !reflect.DeepEqual(step.Arguments, args) ||
			!reflect.DeepEqual(step.Environment, map[string]string{"container": wanted.Name, "image": "oci://" + wanted.Image}) {
			return ErrProvenance
		}
	}
	return nil
}

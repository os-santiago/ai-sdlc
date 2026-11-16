// Package buildrecord describes authenticated historical build evidence.
// Only the store's role-checked reader establishes its authenticity; constructing
// this value or validating its structure never grants execution authority.
package buildrecord

import (
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

type Record struct {
	Context        buildworkload.RetainedContext
	Command        building.Command
	Plan           building.Plan
	Build, Publish building.Termination
	Result         building.Result
	RecordedAt     time.Time
}

func (r Record) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }

func (r Record) Validate() error {
	if r.Context.ValidateHistorical(r.Command, r.Plan) != nil ||
		r.Build.Phase != "build" || r.Publish.Phase != "publish" ||
		r.Build.Validate(r.Command, r.Plan, r.Context.Slot, time.UnixMilli(r.Build.ObservedAt)) != nil ||
		r.Publish.Validate(r.Command, r.Plan, r.Context.Slot, time.UnixMilli(r.Publish.ObservedAt)) != nil ||
		r.Build.PipelineRunUID == r.Publish.PipelineRunUID || r.Build.TaskRunUID == r.Publish.TaskRunUID || r.Build.PodUID == r.Publish.PodUID ||
		r.Build.ObservedAt > r.Publish.ObservedAt || r.RecordedAt.Before(time.UnixMilli(r.Publish.ObservedAt)) ||
		r.RecordedAt.Before(time.UnixMilli(r.Command.IssuedAt)) || !r.RecordedAt.Before(time.UnixMilli(r.Command.ExpiresAt)) ||
		r.Result.Validate(r.Command, r.Plan, r.Build, r.Publish) != nil {
		return building.ErrBuild
	}
	objects := map[string]bool{r.Build.PipelineRunUID: true, r.Build.TaskRunUID: true, r.Build.PodUID: true}
	for _, id := range []string{r.Publish.PipelineRunUID, r.Publish.TaskRunUID, r.Publish.PodUID} {
		if objects[id] {
			return building.ErrBuild
		}
	}
	finished := int64(0)
	containers := map[string]bool{}
	for _, c := range r.Build.Containers {
		containers[c.ContainerID] = true
		if c.FinishedAt > finished {
			finished = c.FinishedAt
		}
	}
	for _, c := range r.Publish.Containers {
		if containers[c.ContainerID] || c.StartedAt < finished {
			return building.ErrBuild
		}
	}
	return nil
}

package podtransport

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

var runtimeContainerID = regexp.MustCompile(`^containerd://[a-f0-9]{64}$`)
var runtimeImageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type buildPins struct {
	Terminated      map[string]bool
	TaskUID, PodUID string
	Containers      map[string]string
}

func normalizedImageID(image string) (string, string, error) {
	pieces := strings.Split(image, "@")
	if len(pieces) != 2 || !runtimeImageDigest.MatchString(pieces[1]) {
		return "", "", ErrBuildTransport
	}
	repository := pieces[0]
	if index := strings.LastIndex(repository, ":"); index > strings.LastIndex(repository, "/") {
		repository = repository[:index]
	}
	return repository + "@" + pieces[1], pieces[1], nil
}

// inspectContainerStates retains first observed process identities. Incomplete
// status is pending; a changed identity, restart or failed exit is an error.
func inspectContainerStates(spec, status map[string]any, pins *buildPins) ([]building.ContainerExit, bool, error) {
	if pins == nil {
		return nil, false, ErrBuildTransport
	}
	if pins.Containers == nil {
		pins.Containers = map[string]string{}
	}
	if pins.Terminated == nil {
		pins.Terminated = map[string]bool{}
	}
	exits := []building.ContainerExit{}
	complete := true
	for _, row := range []struct{ spec, status, kind string }{{"initContainers", "initContainerStatuses", "init"}, {"containers", "containerStatuses", "normal"}} {
		rawDefinitions, ok := spec[row.spec].([]any)
		if !ok || len(rawDefinitions) == 0 || len(rawDefinitions) > 8 {
			return nil, false, ErrBuildTransport
		}
		definitions := map[string]map[string]any{}
		for _, raw := range rawDefinitions {
			definition, ok := raw.(map[string]any)
			if !ok {
				return nil, false, ErrBuildTransport
			}
			name, ok := definition["name"].(string)
			if !ok || definitions[name] != nil {
				return nil, false, ErrBuildTransport
			}
			definitions[name] = definition
		}
		value, exists := status[row.status]
		if !exists {
			for name := range definitions {
				if pins.Containers[name] != "" {
					return nil, false, ErrBuildTransport
				}
			}
			complete = false
			continue
		}
		statuses, ok := value.([]any)
		if !ok || len(statuses) > len(definitions) {
			return nil, false, ErrBuildTransport
		}
		seen := map[string]bool{}
		for _, raw := range statuses {
			s, ok := raw.(map[string]any)
			if !ok {
				return nil, false, ErrBuildTransport
			}
			name, ok := s["name"].(string)
			definition := definitions[name]
			if !ok || definition == nil || seen[name] || s["restartCount"] != float64(0) {
				return nil, false, ErrBuildTransport
			}
			seen[name] = true
			if last, exists := s["lastState"]; exists {
				m, ok := last.(map[string]any)
				if !ok || len(m) > 0 {
					return nil, false, ErrBuildTransport
				}
			}
			id, _ := s["containerID"].(string)
			imageID, _ := s["imageID"].(string)
			expectedImage, expectedDigest, e := normalizedImageID(definition["image"].(string))
			if e != nil {
				return nil, false, e
			}
			if id != "" {
				if !runtimeContainerID.MatchString(id) || imageID != expectedImage {
					return nil, false, ErrBuildTransport
				}
				if previous := pins.Containers[name]; previous != "" && previous != id {
					return nil, false, ErrBuildTransport
				}
				for previousName, previousID := range pins.Containers {
					if previousName != name && previousID == id {
						return nil, false, ErrBuildTransport
					}
				}
				pins.Containers[name] = id
			} else if pins.Containers[name] != "" {
				return nil, false, ErrBuildTransport
			}
			if imageID != "" && imageID != expectedImage {
				return nil, false, ErrBuildTransport
			}
			state, ok := s["state"].(map[string]any)
			if !ok || len(state) != 1 {
				return nil, false, ErrBuildTransport
			}
			terminated, done := state["terminated"]
			if !done {
				if pins.Terminated[name] {
					return nil, false, ErrBuildTransport
				}
				if _, waiting := state["waiting"]; !waiting {
					if _, running := state["running"]; !running || id == "" {
						return nil, false, ErrBuildTransport
					}
				}
				complete = false
				continue
			}
			finish, ok := terminated.(map[string]any)
			if !ok || id == "" || finish["containerID"] != id || finish["exitCode"] != float64(0) {
				return nil, false, ErrBuildTransport
			}
			if signal, exists := finish["signal"]; exists && signal != float64(0) {
				return nil, false, ErrBuildTransport
			}
			startText, ok := finish["startedAt"].(string)
			if !ok {
				return nil, false, ErrBuildTransport
			}
			endText, ok := finish["finishedAt"].(string)
			if !ok {
				return nil, false, ErrBuildTransport
			}
			started, e := time.Parse(time.RFC3339Nano, startText)
			if e != nil {
				return nil, false, ErrBuildTransport
			}
			ended, e := time.Parse(time.RFC3339Nano, endText)
			if e != nil || ended.Before(started) {
				return nil, false, ErrBuildTransport
			}
			pins.Terminated[name] = true
			exits = append(exits, building.ContainerExit{Name: name, Kind: row.kind, ImageDigest: expectedDigest, ContainerID: id, StartedAt: started.UnixMilli(), FinishedAt: ended.UnixMilli()})
		}
		if len(seen) != len(definitions) {
			complete = false
			for name := range definitions {
				if !seen[name] && pins.Containers[name] != "" {
					return nil, false, ErrBuildTransport
				}
			}
		}
	}
	return exits, complete, nil
}
func succeededStatus(obj map[string]any) (bool, error) {
	value, exists := obj["status"]
	if !exists {
		return false, nil
	}
	status, ok := value.(map[string]any)
	if !ok {
		return false, ErrBuildTransport
	}
	value, exists = status["conditions"]
	if !exists {
		return false, nil
	}
	conditions, ok := value.([]any)
	if !ok || len(conditions) > 16 {
		return false, ErrBuildTransport
	}
	seen := false
	done := false
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return false, ErrBuildTransport
		}
		if condition["type"] != "Succeeded" {
			continue
		}
		if seen {
			return false, ErrBuildTransport
		}
		seen = true
		switch condition["status"] {
		case "True":
			done = true
		case "Unknown":
		case "False":
			return false, ErrBuildTransport
		default:
			return false, ErrBuildTransport
		}
	}
	return done, nil
}
func marshalObject(obj map[string]any) []byte { raw, _ := json.Marshal(obj); return raw }

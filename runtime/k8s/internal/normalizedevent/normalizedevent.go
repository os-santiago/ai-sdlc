// Package normalizedevent decodes spec/normalized-event.schema.json payloads
// (the forge-neutral event this repo's intake stage already produces for the
// GHA runtime) and projects a work_item entity into the admission
// component's Issue type.
//
// This is the k8s/OpenShift control plane's intake boundary: it consumes the
// same canonical event the GHA ai-sdlc-intake workflow emits, so a repo's
// trigger surface (GitHub webhook, GitLab, forge-neutral future adapters)
// does not change based on which control plane (GHA or k8s) is executing
// the pipeline.
package normalizedevent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Entity mirrors the schema's "entity" object for the work_item kind.
type Entity struct {
	Kind        string   `json:"kind"`
	ID          string   `json:"id"`
	Number      *int     `json:"number"`
	Title       string   `json:"title"`
	Description *string  `json:"description"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
}

// Actor mirrors the schema's "actor" object.
type Actor struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"`
	Login *string `json:"login"`
}

// Repository mirrors the schema's "repository" object.
type Repository struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Event is the subset of spec/normalized-event.schema.json this package
// decodes. Unknown fields are ignored per the contract's forward-compat
// rule ("Consumers must ignore unknown JSON properties.").
type Event struct {
	Version    string      `json:"version"`
	Entity     Entity      `json:"entity"`
	Transition string      `json:"transition"`
	Actor      Actor       `json:"actor"`
	Repository *Repository `json:"repository"`
}

// Decode parses a normalized-event JSON payload.
func Decode(raw []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return Event{}, fmt.Errorf("decode normalized event: %w", err)
	}
	if event.Entity.Kind != "work_item" {
		return Event{}, fmt.Errorf("normalized event entity.kind %q is not work_item", event.Entity.Kind)
	}
	return event, nil
}

var acceptanceSectionHeading = regexp.MustCompile(`(?im)^#{1,6}\s*(acceptance criteria|ac)\s*$`)
var bulletLine = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s*(?:\[[ xX]\]\s*)?(.+?)\s*$`)
var nextHeading = regexp.MustCompile(`(?m)^#{1,6}\s`)

// ExtractAcceptanceCriteria pulls the checklist/bullet lines out of the
// "## Acceptance criteria" (or "## AC") section of an issue body, per
// spec/definition-of-ready.md rules R2/R3. Returns nil when the issue has no
// such section (callers MUST treat that as DoR failure, not an empty-but-ok
// list — spec/definition-of-ready.md: "The agent MUST NOT silently implement
// a best-guess interpretation.").
func ExtractAcceptanceCriteria(body string) []string {
	loc := acceptanceSectionHeading.FindStringIndex(body)
	if loc == nil {
		return nil
	}
	rest := body[loc[1]:]
	if end := nextHeading.FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}

	var criteria []string
	for _, line := range strings.Split(rest, "\n") {
		if match := bulletLine.FindStringSubmatch(line); match != nil {
			criterion := strings.TrimSpace(match[1])
			if criterion != "" {
				criteria = append(criteria, criterion)
			}
		}
	}
	return criteria
}

// IssueView is the normalized-event projection consumed by the admission
// component. It intentionally mirrors admission.Issue's field names so
// callers can convert with a one-line struct literal — this package does not
// import the admission package to keep the dependency direction
// intake-shaped-event -> component, never the reverse.
type IssueView struct {
	Repository         string
	Number             int
	Revision           string
	Title              string
	Body               string
	AcceptanceCriteria []string
	RequestedBy        string
	AuthorizedBy       string
}

// ToIssueView projects a decoded work_item event into an IssueView.
// authorizedBy is resolved by the caller against triggers.authorized_labelers
// (contract package) before this call — the normalized event only carries
// the raw actor, not an authorization verdict.
func ToIssueView(event Event, authorizedBy string) (IssueView, error) {
	if event.Entity.Kind != "work_item" {
		return IssueView{}, fmt.Errorf("entity.kind %q is not work_item", event.Entity.Kind)
	}
	if event.Repository == nil {
		return IssueView{}, fmt.Errorf("work_item event is missing repository")
	}
	if event.Entity.Number == nil {
		return IssueView{}, fmt.Errorf("work_item event is missing entity.number")
	}

	body := ""
	if event.Entity.Description != nil {
		body = *event.Entity.Description
	}
	requestedBy := event.Actor.ID
	if event.Actor.Login != nil && *event.Actor.Login != "" {
		requestedBy = *event.Actor.Login
	}

	return IssueView{
		Repository:         event.Repository.Name,
		Number:             *event.Entity.Number,
		Revision:           event.Entity.ID,
		Title:              event.Entity.Title,
		Body:               body,
		AcceptanceCriteria: ExtractAcceptanceCriteria(body),
		RequestedBy:        requestedBy,
		AuthorizedBy:       authorizedBy,
	}, nil
}

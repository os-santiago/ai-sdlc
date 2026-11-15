// Package authority defines the reviewed, deterministic component capability
// boundary. Work-item content never supplies or modifies this policy.
package authority

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var environmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

var levels = map[string]int{"shadow": 0, "suggest": 1, "auto-pr": 2, "auto-merge-low": 3, "auto-merge-all": 4, "auto-deploy": 5}

type PublicKey struct {
	Role  string `json:"role"`
	Value string `json:"value"`
}

type Grant struct {
	Caller       string `json:"caller"`
	Target       string `json:"target"`
	Operation    string `json:"operation"`
	MinimumLevel string `json:"minimumLevel"`
	EventType    string `json:"eventType"`
}

// Policy is mounted from reviewed configuration, outside generated workspaces.
// Keys are public; private identities are separate, per-component Secrets.
type Policy struct {
	Version     string               `json:"version"`
	Repository  string               `json:"repository"`
	Environment string               `json:"environment"`
	Autonomy    string               `json:"autonomy"`
	Freeze      bool                 `json:"freeze"`
	Keys        map[string]PublicKey `json:"keys"`
	Grants      []Grant              `json:"grants"`
}

// Operations are finite capabilities, not caller-controlled URLs or shell text.
var operations = map[string][2]string{
	"admit-issue":       {"intake", "admission-controller"},
	"plan-admission":    {"admission-controller", "orchestrator"},
	"execute-work":      {"orchestrator", "worker"},
	"record-attempt":    {"worker", "orchestrator"},
	"verify-artifact":   {"worker", "verifier"},
	"publish-candidate": {"verifier", "release-manager"},
	"merge-publication": {"release-manager", "release-manager"},
	"record-release":    {"release-manager", "orchestrator"},
	"deploy-release":    {"release-manager", "deployer"},
	"record-deployment": {"deployer", "release-manager"},
}

// A reviewed policy may tighten these floors, never lower them. Risk, checks
// and rollback evidence are additional gates; a level alone never proves them.
var floors = map[string]string{
	"admit-issue": "shadow", "plan-admission": "shadow",
	"execute-work": "auto-pr", "record-attempt": "auto-pr",
	"verify-artifact": "auto-pr", "publish-candidate": "auto-pr",
	"record-release": "auto-pr", "deploy-release": "auto-deploy",
	"merge-publication": "auto-merge-low",
	"record-deployment": "auto-deploy",
}
var eventTypes = map[string]string{
	"admit-issue": "IssueObserved", "plan-admission": "IssueAdmissionDecided",
	"execute-work": "WorkItemReady", "record-attempt": "ImplementationAttemptDecided",
	"verify-artifact": "CandidateProduced", "publish-candidate": "CandidateVerified",
	"record-release": "ReleaseGateDecided", "deploy-release": "ReleaseReady",
	"merge-publication": "PublicationMergeRequested",
	"record-deployment": "DeploymentVerified",
}

var roles = map[string]bool{
	"intake": true, "admission-controller": true, "orchestrator": true,
	"worker": true, "verifier": true, "release-manager": true,
	"relay": true, "deployer": true,
}

func DecodeStrict(raw []byte, value any) error {
	if err := rejectAmbiguousJSON(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid authority JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("authority JSON must contain one value")
	}
	return nil
}

func ParsePolicy(raw []byte) (Policy, error) {
	var p Policy
	if len(raw) == 0 || len(raw) > 128*1024 {
		return p, errors.New("invalid policy size")
	}
	if err := DecodeStrict(raw, &p); err != nil {
		return p, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Policy{}, errors.New("invalid policy object")
	}
	for _, required := range []string{"version", "repository", "environment", "autonomy", "freeze", "keys", "grants"} {
		if _, ok := fields[required]; !ok {
			return Policy{}, errors.New("missing policy field")
		}
	}
	if string(fields["freeze"]) != "true" && string(fields["freeze"]) != "false" {
		return Policy{}, errors.New("freeze must be explicit boolean")
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (p Policy) Validate() error {
	if p.Version != "1" || !repositoryPattern.MatchString(p.Repository) || !environmentPattern.MatchString(p.Environment) {
		return errors.New("unsupported policy version or missing repository/environment")
	}
	for _, part := range strings.Split(p.Repository, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\? #\n\r\t:") {
			return errors.New("invalid repository scope")
		}
	}
	if _, ok := levels[p.Autonomy]; !ok {
		return errors.New("unknown autonomy ceiling")
	}
	if len(p.Keys) == 0 || len(p.Grants) == 0 {
		return errors.New("policy must declare keys and grants")
	}
	keyOwners := map[string]string{}
	for id, key := range p.Keys {
		decoded, err := base64.StdEncoding.Strict().DecodeString(key.Value)
		if !keyIDPattern.MatchString(id) || !roles[key.Role] || err != nil || len(decoded) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(decoded) != key.Value {
			return errors.New("invalid component verification key")
		}
		if owner, found := keyOwners[string(decoded)]; found && owner != key.Role {
			return errors.New("verification key shared across roles")
		}
		keyOwners[string(decoded)] = key.Role
	}
	seen := map[string]bool{}
	for _, grant := range p.Grants {
		pair, ok := operations[grant.Operation]
		if !ok || pair != [2]string{grant.Caller, grant.Target} {
			return errors.New("grant does not match the operation owner and target")
		}
		if _, ok := levels[grant.MinimumLevel]; !ok {
			return errors.New("unknown operation autonomy level")
		}
		if levels[grant.MinimumLevel] < levels[floors[grant.Operation]] || grant.EventType != eventTypes[grant.Operation] {
			return errors.New("grant weakens autonomy floor or changes event type")
		}
		if seen[grant.Operation] {
			return errors.New("duplicate operation grant")
		}
		seen[grant.Operation] = true
	}
	return nil
}

func (p Policy) Revision() string {
	raw, _ := json.Marshal(p)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// Allows is fail-closed for unknown operations, scope, roles and ceilings.
// A freeze rejects all operational transitions; observations use a separate API.
func (p Policy) Allows(caller, target, operation, repository, environment string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Freeze {
		return errors.New("runtime frozen")
	}
	if repository != p.Repository || environment != p.Environment {
		return errors.New("scope denied")
	}
	for _, grant := range p.Grants {
		if grant.Caller == caller && grant.Target == target && grant.Operation == operation {
			if levels[p.Autonomy] < levels[grant.MinimumLevel] {
				return errors.New("autonomy ceiling denies operation")
			}
			return nil
		}
	}
	return errors.New("operation denied")
}

func (p Policy) verificationKey(id, role string) (ed25519.PublicKey, error) {
	key, ok := p.Keys[id]
	if !ok || key.Role != role {
		return nil, errors.New("unknown, revoked or wrong-role key")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(key.Value)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid verification key")
	}
	return ed25519.PublicKey(raw), nil
}

func OperationOwner(operation string) (caller, target string, err error) {
	pair, ok := operations[operation]
	if !ok {
		return "", "", fmt.Errorf("unsupported operation %q", operation)
	}
	return pair[0], pair[1], nil
}

// JSON object keys must be unique at every depth. The standard decoder accepts
// last-key-wins, which is ambiguous for reviewed authorization configuration.
func rejectAmbiguousJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("authority JSON nesting too deep")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("invalid authority JSON")
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return errors.New("invalid authority JSON")
				}
				name, ok := key.(string)
				name = strings.ToLower(name)
				if !ok || seen[name] {
					return errors.New("duplicate authority JSON key")
				}
				seen[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid authority JSON")
			}
		case json.Delim('['):
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid authority JSON")
			}
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("authority JSON must contain one value")
	}
	return nil
}

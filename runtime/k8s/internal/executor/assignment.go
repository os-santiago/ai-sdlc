// Package executor defines the hidden, bounded SCC workload boundary. Its input
// is prepared by a trusted controller; parsing it is not execution authority.
package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const (
	AssignmentVersion  = "ai-sdlc.executor-assignment/v1"
	ProfileVersion     = "ai-sdlc.executor-profile/v1"
	MaxAssignmentBytes = 64 * 1024
	MaxProfileBytes    = 16 * 1024
)

var ErrInvalid = errors.New("invalid executor assignment or profile")
var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

type Assignment struct {
	Version            string   `json:"version"`
	WorkItemID         string   `json:"workItemId"`
	Attempt            int      `json:"attempt"`
	Repository         string   `json:"repository"`
	SourceSHA          string   `json:"sourceSHA"`
	TreeSHA256         string   `json:"treeSHA256"`
	ProfileSHA256      string   `json:"profileSHA256"`
	AuthorityRevision  string   `json:"authorityRevision"`
	IssueNumbers       []int    `json:"issueNumbers"`
	Title              string   `json:"title"`
	Body               string   `json:"body"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	ExpiresAt          int64    `json:"expiresAt"`
}

// Profile contains reviewed local bounds, never arbitrary shell commands,
// URLs, mounts, environment or credentials. Durable provider quotas are separate.
type Profile struct {
	Version    string `json:"version"`
	Model      string `json:"model"`
	MaxSteps   int    `json:"maxSteps"`
	MaxSeconds int    `json:"maxSeconds"`
	MaxTokens  int    `json:"maxTokens"`
	ADevSHA256 string `json:"aDevSHA256"`
}

func strictBound(raw []byte, expected string, maximum int, keys []string, value any) error {
	if len(raw) == 0 || len(raw) > maximum || !utf8.Valid(raw) || !workspacebundle.ValidDigest(expected) || workspacebundle.Digest(raw) != expected || authority.DecodeStrict(raw, value) != nil {
		return ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return ErrInvalid
	}
	for _, key := range keys {
		v, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return ErrInvalid
		}
	}
	return nil
}
func parseProfile(raw []byte, expected, version string) (Profile, error) {
	var p Profile
	keys := []string{"version", "model", "maxSteps", "maxSeconds", "maxTokens", "aDevSHA256"}
	if strictBound(raw, expected, MaxProfileBytes, keys, &p) != nil || p.Version != version || !modelPattern.MatchString(p.Model) || p.MaxSteps < 1 || p.MaxSteps > 32 || p.MaxSeconds < 1 || p.MaxSeconds > 900 || p.MaxTokens < 1 || p.MaxTokens > 8192 || !workspacebundle.ValidDigest(p.ADevSHA256) {
		return Profile{}, ErrInvalid
	}
	return p, nil
}

// Generation parsing remains purpose-specific: a reviewer profile cannot be
// substituted into an installed generation controller.
func ParseProfile(raw []byte, expected string) (Profile, error) {
	return parseProfile(raw, expected, ProfileVersion)
}

const ReviewProfileVersion = "ai-sdlc.reviewer-profile/v1"

func ParseReviewProfile(raw []byte, expected string) (Profile, error) {
	p, err := parseProfile(raw, expected, ReviewProfileVersion)
	if err != nil || p.MaxSteps > 8 || p.MaxSeconds > 180 {
		return Profile{}, ErrInvalid
	}
	return p, nil
}
func parseWorkloadProfile(raw []byte, expected string) (Profile, error) {
	if p, err := ParseProfile(raw, expected); err == nil {
		return p, nil
	}
	return ParseReviewProfile(raw, expected)
}

func ParseAssignment(raw []byte, expected string, now time.Time) (Assignment, error) {
	var a Assignment
	keys := []string{"version", "workItemId", "attempt", "repository", "sourceSHA", "treeSHA256", "profileSHA256", "authorityRevision", "issueNumbers", "title", "body", "acceptanceCriteria", "expiresAt"}
	if strictBound(raw, expected, MaxAssignmentBytes, keys, &a) != nil || a.Version != AssignmentVersion || !uuidPattern.MatchString(a.WorkItemID) || a.Attempt < 1 || a.Attempt > 16 || len(a.Repository) > 256 || !repoPattern.MatchString(a.Repository) || !workspacebundle.ValidSourceSHA(a.SourceSHA) || !workspacebundle.ValidDigest(a.TreeSHA256) || !workspacebundle.ValidDigest(a.ProfileSHA256) || !workspacebundle.ValidDigest(a.AuthorityRevision) {
		return Assignment{}, ErrInvalid
	}
	for _, part := range strings.Split(a.Repository, "/") {
		if part == "." || part == ".." {
			return Assignment{}, ErrInvalid
		}
	}
	if len(a.IssueNumbers) < 1 || len(a.IssueNumbers) > 16 || len(a.AcceptanceCriteria) < 1 || len(a.AcceptanceCriteria) > 8 || len(a.Title) > 256 || strings.TrimSpace(a.Title) == "" || len(a.Body) > 32*1024 || strings.TrimSpace(a.Body) == "" {
		return Assignment{}, ErrInvalid
	}
	seen := map[int]bool{}
	for _, issue := range a.IssueNumbers {
		if issue < 1 || issue > 2147483647 || seen[issue] {
			return Assignment{}, ErrInvalid
		}
		seen[issue] = true
	}
	for _, criterion := range a.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" || len(criterion) > 4096 {
			return Assignment{}, ErrInvalid
		}
	}
	expires := time.UnixMilli(a.ExpiresAt)
	if !expires.After(now) || expires.After(now.Add(time.Hour)) {
		return Assignment{}, ErrInvalid
	}
	return a, nil
}

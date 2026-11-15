// Package forge exposes bounded GitHub reads for trusted intake. It never
// accepts an endpoint, repository or credential from issue content.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

type Credential struct {
	Version    int    `json:"version"`
	Mode       string `json:"mode"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	ActorID    int64  `json:"actorID"`
	ActorLogin string `json:"actorLogin"`
	Token      string `json:"token"`
}

type Client struct {
	repository string
	token      string
	base       string
	http       *http.Client
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_]{20,512}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func Load(path, repository string) (*Client, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("GitHub credential unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(raw) > 16384 {
		return nil, errors.New("GitHub credential exceeds bound")
	}
	var credential Credential
	if authority.DecodeStrict(raw, &credential) != nil {
		return nil, errors.New("invalid GitHub credential")
	}
	return New(credential, repository)
}

func New(credential Credential, repository string) (*Client, error) {
	if !repoPattern.MatchString(repository) {
		return nil, errors.New("invalid repository")
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("invalid repository")
		}
	}
	if credential.Version != 1 || credential.Mode != "gh-cli" || credential.Host != "github.com" || credential.Repository != repository || credential.ActorID <= 0 || credential.ActorLogin == "" || !tokenPattern.MatchString(credential.Token) {
		return nil, errors.New("GitHub credential scope or mode mismatch")
	}
	return &Client{repository: repository, token: credential.Token, base: "https://api.github.com", http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// APIError omits response bodies, URLs and credentials. RetryAfter is a floor,
// not an instruction to retry immediately or bypass a durable retry budget.
type APIError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub request rejected (status %d)", e.Status)
}

func (c *Client) get(ctx context.Context, suffix string, query url.Values, out any) error {
	endpoint := c.base + "/repos/" + c.repository + suffix
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("GitHub request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := c.http.Do(req)
	if err != nil {
		return errors.New("GitHub transport unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		delay := time.Minute
		if seconds, err := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds <= 86400 {
			delay = time.Duration(seconds) * time.Second
		}
		if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && response.Header.Get("X-RateLimit-Remaining") == "0" {
			until := time.Until(time.Unix(reset, 0))
			if until > delay && until <= 24*time.Hour {
				delay = until
			}
		}
		return &APIError{Status: response.StatusCode, RetryAfter: delay}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 {
		return errors.New("GitHub response exceeds bound")
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("invalid GitHub response")
	}
	return nil
}

type Actor struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type Label struct {
	Name string `json:"name"`
}
type Issue struct {
	ID          int64           `json:"id"`
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	User        Actor           `json:"user"`
	Labels      []Label         `json:"labels"`
	UpdatedAt   time.Time       `json:"updated_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (i Issue) IsPR() bool { return len(i.PullRequest) > 0 && string(i.PullRequest) != "null" }
func (i Issue) HasLabel(label string) bool {
	for _, value := range i.Labels {
		if value.Name == label {
			return true
		}
	}
	return false
}
func (i Issue) Validate() error {
	if i.ID <= 0 || i.Number <= 0 || i.User.ID <= 0 || i.User.Login == "" || i.UpdatedAt.IsZero() || len(i.Title) > 1024 || len(i.Body) > 128*1024 || len(i.Labels) > 100 || (i.State != "open" && i.State != "closed") {
		return errors.New("invalid issue snapshot")
	}
	return nil
}

type LabelEvent struct {
	ID    int64  `json:"id"`
	Event string `json:"event"`
	Actor Actor  `json:"actor"`
	Label Label  `json:"label"`
}

func (c *Client) Issues(ctx context.Context, since time.Time, page int) ([]Issue, error) {
	if page < 1 || page > 100 {
		return nil, errors.New("issue pagination bound exceeded")
	}
	query := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"asc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	var issues []Issue
	if err := c.get(ctx, "/issues", query, &issues); err != nil {
		return nil, err
	}
	if issues == nil || len(issues) > 100 {
		return nil, errors.New("issue page exceeds bound")
	}
	for _, issue := range issues {
		if err := issue.Validate(); err != nil {
			return nil, err
		}
	}
	return issues, nil
}
func (c *Client) Issue(ctx context.Context, number int) (Issue, error) {
	var issue Issue
	if number < 1 {
		return issue, errors.New("invalid issue number")
	}
	if err := c.get(ctx, "/issues/"+strconv.Itoa(number), nil, &issue); err != nil {
		return issue, err
	}
	if issue.Number != number || issue.IsPR() {
		return Issue{}, errors.New("issue identity mismatch")
	}
	return issue, issue.Validate()
}
func (c *Client) LabelActor(ctx context.Context, number int, label string) (Actor, int64, error) {
	if number < 1 || label == "" || len(label) > 100 {
		return Actor{}, 0, errors.New("invalid label lookup")
	}
	var latest LabelEvent
	for page := 1; page <= 100; page++ {
		var events []LabelEvent
		if err := c.get(ctx, "/issues/"+strconv.Itoa(number)+"/events", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, &events); err != nil {
			return Actor{}, 0, err
		}
		if events == nil || len(events) > 100 {
			return Actor{}, 0, errors.New("event page exceeds bound")
		}
		for _, event := range events {
			if event.Label.Name == label && (event.Event == "labeled" || event.Event == "unlabeled") && event.ID > latest.ID {
				latest = event
			}
		}
		if len(events) < 100 {
			if latest.Event != "labeled" || latest.Actor.ID <= 0 || latest.Actor.Login == "" {
				return Actor{}, 0, nil
			}
			return latest.Actor, latest.ID, nil
		}
	}
	return Actor{}, 0, errors.New("event pagination bound exceeded")
}

func (c *Client) BaseSHA(ctx context.Context, branch string) (string, error) {
	// One reviewed branch name; slash-containing refs are encoded as one path
	// segment and cannot alter the fixed API route.
	if branch == "" || len(branch) > 200 || strings.ContainsAny(branch, "\r\n?#\\") || strings.Contains(branch, "..") {
		return "", errors.New("invalid base branch")
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := c.get(ctx, "/commits/"+url.PathEscape(branch), nil, &commit); err != nil {
		return "", err
	}
	if !shaPattern.MatchString(commit.SHA) {
		return "", errors.New("invalid base revision")
	}
	return commit.SHA, nil
}

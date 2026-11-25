package publication

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Observation states. Only ObservedExact and ObservedAbsent change a journal;
// ObservedDifferent and every error keep the publication quarantined.
const (
	ObservedExact     = "exact"
	ObservedAbsent    = "absent"
	ObservedDifferent = "different"
)

type Observation struct {
	State       string
	PullRequest int
}

// Reader is the read-only forge transport for reconciling an uncertain
// effect. It issues only GET requests to a fixed, closed set of paths derived
// from a validated Expectation; it has no method that can write, follow a
// redirect or retry. No response body, credential or URL appears in an error.
type Reader struct {
	repository, token, base string
	http                    *http.Client
}

func NewReader(credential forge.Credential) (*Reader, error) {
	if !project.Supported(credential.Repository) {
		return nil, ErrPublication
	}
	if _, e := forge.New(credential, credential.Repository); e != nil {
		return nil, ErrPublication
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 8 * time.Second, MaxResponseHeaderBytes: 8192}
	return &Reader{credential.Repository, credential.Token, "https://api.github.com", &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// get returns (true, nil) for a decoded 200, (false, nil) for a 404, and an
// error for anything else. Only a 404 is ever read as absence.
func (r *Reader) get(ctx context.Context, path string, query url.Values, out any) (bool, error) {
	if r == nil || r.http == nil || !project.Supported(r.repository) || ctx.Err() != nil {
		return false, ErrPublication
	}
	target := r.base + "/repos/" + r.repository + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if e != nil {
		return false, ErrPublication
	}
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, e := r.http.Do(req)
	if e != nil {
		return false, ErrPublication
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK || response.ContentLength > 1024*1024 || response.Header.Get("Content-Encoding") != "" {
		return false, ErrPublication
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if e != nil || len(raw) < 1 || len(raw) > 1024*1024 || !utf8.Valid(raw) || ctx.Err() != nil {
		return false, ErrPublication
	}
	// Reject duplicate keys before projecting onto the finite fields we compare.
	var shape any
	if authority.DecodeStrict(raw, &shape) != nil || json.Unmarshal(raw, out) != nil {
		return false, ErrPublication
	}
	return true, nil
}

func (r *Reader) valid(x Expectation) bool {
	if r == nil || x.Repository != r.repository || !workspacebundle.ValidSourceSHA(x.Source) || !workspacebundle.ValidSourceSHA(x.SHA) || !workspacebundle.ValidDigest(x.RequestDigest) {
		return false
	}
	switch x.Kind {
	case EffectTree:
		return true
	case EffectCommit:
		return workspacebundle.ValidSourceSHA(x.Tree) && x.Message != "" && !x.Date.IsZero()
	case EffectBranch:
		return pilotBranch.MatchString(x.Branch)
	case EffectPullRequest:
		return pilotBranch.MatchString(x.Branch) && workspacebundle.ValidDigest(x.PlanDigest)
	}
	return false
}

type gitObject struct {
	SHA string `json:"sha"`
}

// control proves the credential can read this repository's Git objects: the
// publication's parent commit must be visible before a 404 counts as absence.
func (r *Reader) control(ctx context.Context, source string) error {
	var commit gitObject
	found, e := r.get(ctx, "/git/commits/"+source, nil, &commit)
	if e != nil || !found || commit.SHA != source {
		return ErrPublication
	}
	return nil
}

// Observe reports whether the planned effect exists exactly, is absent, or
// exists differently. Any transport, status or shape doubt is an error.
func (r *Reader) Observe(ctx context.Context, x Expectation) (Observation, error) {
	if !r.valid(x) {
		return Observation{}, ErrPublication
	}
	if r.control(ctx, x.Source) != nil {
		return Observation{}, ErrPublication
	}
	exact := func(ok bool) (Observation, error) {
		if ok {
			return Observation{State: ObservedExact}, nil
		}
		return Observation{State: ObservedDifferent}, nil
	}
	switch x.Kind {
	case EffectTree:
		var tree struct {
			SHA       string `json:"sha"`
			Truncated bool   `json:"truncated"`
		}
		found, e := r.get(ctx, "/git/trees/"+x.SHA, nil, &tree)
		if e != nil {
			return Observation{}, e
		}
		if !found {
			return Observation{State: ObservedAbsent}, nil
		}
		return exact(tree.SHA == x.SHA && !tree.Truncated)
	case EffectCommit:
		var commit struct {
			SHA       string       `json:"sha"`
			Message   string       `json:"message"`
			Tree      gitObject    `json:"tree"`
			Parents   []gitObject  `json:"parents"`
			Author    commitPerson `json:"author"`
			Committer commitPerson `json:"committer"`
		}
		found, e := r.get(ctx, "/git/commits/"+x.SHA, nil, &commit)
		if e != nil {
			return Observation{}, e
		}
		if !found {
			return Observation{State: ObservedAbsent}, nil
		}
		person := commitPerson{"Sergio Canales", "sergio.canales.e@gmail.com", x.Date.UTC().Format(time.RFC3339)}
		return exact(commit.SHA == x.SHA && commit.Tree.SHA == x.Tree && len(commit.Parents) == 1 && commit.Parents[0].SHA == x.Source && strings.TrimSuffix(commit.Message, "\n") == strings.TrimSuffix(x.Message, "\n") && sameInstant(commit.Author, person) && sameInstant(commit.Committer, person))
	case EffectBranch:
		var ref struct {
			Ref    string `json:"ref"`
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		found, e := r.get(ctx, "/git/ref/heads/"+x.Branch, nil, &ref)
		if e != nil {
			return Observation{}, e
		}
		if !found {
			return Observation{State: ObservedAbsent}, nil
		}
		return exact(ref.Ref == "refs/heads/"+x.Branch && ref.Object.Type == "commit" && ref.Object.SHA == x.SHA)
	case EffectPullRequest:
		owner, _, _ := strings.Cut(r.repository, "/")
		type end struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		}
		var pulls []struct {
			Number int     `json:"number"`
			State  string  `json:"state"`
			Draft  bool    `json:"draft"`
			Body   *string `json:"body"`
			Head   end     `json:"head"`
			Base   end     `json:"base"`
		}
		// state=all: a closed pull request for this head is still a sent effect.
		found, e := r.get(ctx, "/pulls", url.Values{"state": {"all"}, "head": {owner + ":" + x.Branch}, "per_page": {"100"}}, &pulls)
		if e != nil || !found || pulls == nil {
			return Observation{}, ErrPublication
		}
		if len(pulls) == 0 {
			return Observation{State: ObservedAbsent}, nil
		}
		// Exactly one pull request may exist for a branch only this runtime creates.
		p := pulls[0]
		if len(pulls) != 1 || p.Number < 1 || p.Number > 2147483647 || p.State != "open" || p.Draft || p.Body == nil || !strings.Contains(*p.Body, proofMarker(x.PlanDigest)) || p.Head.Ref != x.Branch || p.Head.SHA != x.SHA || p.Head.Repo == nil || p.Head.Repo.FullName != r.repository || p.Base.Ref != "main" || p.Base.Repo == nil || p.Base.Repo.FullName != r.repository {
			return Observation{State: ObservedDifferent}, nil
		}
		return Observation{State: ObservedExact, PullRequest: p.Number}, nil
	}
	return Observation{}, ErrPublication
}

// sameInstant compares identities and instants; GitHub may render the planned
// UTC date with an explicit offset.
func sameInstant(got, want commitPerson) bool {
	a, e1 := time.Parse(time.RFC3339, got.Date)
	b, e2 := time.Parse(time.RFC3339, want.Date)
	return e1 == nil && e2 == nil && got.Name == want.Name && got.Email == want.Email && a.Equal(b)
}

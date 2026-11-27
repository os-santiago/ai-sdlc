package mergewriter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Observation states of a planned merge. Only ObservedMerged and
// ObservedUnmerged change a journal; ObservedDifferent and every error keep
// merges quarantined.
const (
	// ObservedMerged: the pull request is merged at the planned head by a merge
	// commit with exactly the planned tree, parents and message.
	ObservedMerged = "merged"
	// ObservedUnmerged: the pull request is still open, not a draft, at the
	// planned head, and either the base branch still points at the planned base
	// or GitHub's comparison shows the planned head is not contained in it.
	ObservedUnmerged = "unmerged"
	// ObservedDifferent: anything else the forge answered with.
	ObservedDifferent = "different"
)

// Observation is what the forge shows for a planned merge. CommitSHA is the
// merge commit, set only for ObservedMerged.
type Observation struct {
	State, CommitSHA string
}

// Reader is the read-only forge view for reconciling an uncertain merge
// effect. It issues only GET requests to a closed set of paths derived from a
// validated Plan; it cannot write, follow a redirect or retry. Every status
// other than 200, including 404 and 403, is an error: no response is ever read
// as absence. Errors carry no body, credential or URL.
type Reader struct {
	repository, token, base string
	http                    *http.Client
}

func NewReader(credential forge.Credential) (*Reader, error) {
	if !project.Supported(credential.Repository) {
		return nil, ErrMerge
	}
	if _, e := forge.New(credential, credential.Repository); e != nil {
		return nil, ErrMerge
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 8 * time.Second, MaxResponseHeaderBytes: 8192}
	return &Reader{credential.Repository, credential.Token, "https://api.github.com", &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// get decodes one 200 response; anything else is an error.
func (r *Reader) get(ctx context.Context, path string, out any) error {
	if r == nil || r.http == nil || !project.Supported(r.repository) || ctx.Err() != nil {
		return ErrMerge
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/repos/"+r.repository+path, nil)
	if e != nil {
		return ErrMerge
	}
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, e := r.http.Do(req)
	if e != nil {
		return ErrMerge
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > 1024*1024 || response.Header.Get("Content-Encoding") != "" {
		return ErrMerge
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if e != nil || len(raw) < 1 || len(raw) > 1024*1024 || !utf8.Valid(raw) || ctx.Err() != nil {
		return ErrMerge
	}
	// Reject duplicate keys before projecting onto the finite fields compared.
	var object map[string]json.RawMessage
	if authority.DecodeStrict(raw, &object) != nil || object == nil || json.Unmarshal(raw, out) != nil {
		return ErrMerge
	}
	return nil
}

type end struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

// Observe reports whether the planned merge happened exactly as planned,
// provably did not happen, or neither. Any transport, status or shape doubt is
// an error.
func (r *Reader) Observe(ctx context.Context, p Plan) (Observation, error) {
	if r == nil || p.Expected.Repository != r.repository {
		return Observation{}, ErrMerge
	}
	// The plan must be one PlanMerge builds, never a hand-made expectation.
	if planned, e := PlanMerge(p.Expected, p.Issue); e != nil || planned.RequestDigest != p.RequestDigest || planned.Title != p.Title || planned.Message != p.Message {
		return Observation{}, ErrMerge
	}
	x := p.Expected
	var pull struct {
		Number         *int    `json:"number"`
		State          string  `json:"state"`
		Draft          *bool   `json:"draft"`
		Merged         *bool   `json:"merged"`
		MergeCommitSHA *string `json:"merge_commit_sha"`
		Head           end     `json:"head"`
		Base           end     `json:"base"`
	}
	if e := r.get(ctx, "/pulls/"+strconv.Itoa(x.PullRequest), &pull); e != nil {
		return Observation{}, e
	}
	if pull.Number == nil || pull.Merged == nil || pull.Draft == nil {
		return Observation{}, ErrMerge
	}
	different := Observation{State: ObservedDifferent}
	// The pull request the runtime published: its number, its head branch in
	// this repository at the planned head, into main of this repository. A head
	// that moved is never the planned merge, merged or not.
	if *pull.Number != x.PullRequest || pull.Head.Ref != x.Branch || pull.Head.SHA != x.HeadSHA || pull.Head.Repo == nil || pull.Head.Repo.FullName != r.repository || pull.Base.Ref != "main" || pull.Base.Repo == nil || pull.Base.Repo.FullName != r.repository {
		return different, nil
	}
	if *pull.Merged {
		if pull.State != "closed" || pull.MergeCommitSHA == nil || !workspacebundle.ValidSourceSHA(*pull.MergeCommitSHA) {
			return different, nil
		}
		sha := *pull.MergeCommitSHA
		var commit struct {
			SHA     string `json:"sha"`
			Message string `json:"message"`
			Tree    struct {
				SHA string `json:"sha"`
			} `json:"tree"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if e := r.get(ctx, "/git/commits/"+sha, &commit); e != nil {
			return Observation{}, e
		}
		parents := make([]string, 0, len(commit.Parents))
		for _, parent := range commit.Parents {
			parents = append(parents, parent.SHA)
		}
		if commit.SHA != sha || !p.mergedAsPlanned(sha, commit.Tree.SHA, parents, commit.Message) {
			return different, nil
		}
		return Observation{State: ObservedMerged, CommitSHA: sha}, nil
	}
	// Not merged. Only an open, ready pull request at the planned head is the
	// state before the send; a closed one or a draft leaves the outcome to an
	// operator.
	if pull.State != "open" || *pull.Draft {
		return different, nil
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if e := r.get(ctx, "/git/ref/heads/main", &ref); e != nil {
		return Observation{}, e
	}
	if ref.Ref != "refs/heads/main" || ref.Object.Type != "commit" || !workspacebundle.ValidSourceSHA(ref.Object.SHA) {
		return different, nil
	}
	if ref.Object.SHA == x.BaseSHA {
		return Observation{State: ObservedUnmerged}, nil
	}
	// main moved. The planned head is still not part of main only if GitHub's
	// comparison of main with it is "ahead" or "diverged"; "behind" or
	// "identical" means main contains the head, which this merge, or another,
	// may have brought there.
	var compare struct {
		Status     string `json:"status"`
		AheadBy    *int   `json:"ahead_by"`
		BaseCommit struct {
			SHA string `json:"sha"`
		} `json:"base_commit"`
	}
	if e := r.get(ctx, "/compare/"+ref.Object.SHA+"..."+x.HeadSHA+"?per_page=1", &compare); e != nil {
		return Observation{}, e
	}
	if compare.BaseCommit.SHA != ref.Object.SHA || compare.AheadBy == nil || *compare.AheadBy < 1 || (compare.Status != "ahead" && compare.Status != "diverged") {
		return different, nil
	}
	return Observation{State: ObservedUnmerged}, nil
}

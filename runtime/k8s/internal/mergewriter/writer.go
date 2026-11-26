// Package mergewriter is a single-send GitHub merge transport.
// It accepts only its owning controller. Its trusted fence must
// commit effect intent and revalidate current authority before any send.
package mergewriter

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

var ErrMerge = errors.New("merge transport unavailable or denied")

var branch = regexp.MustCompile(`^ai-sdlc/issue-([1-9][0-9]{0,9})-[a-f0-9]{32}$`)

type Writer struct {
	token, base, repository string
	http                    *http.Client
}
type Fence func(context.Context, string) error
type Receipt struct {
	Version     string `json:"version"`
	CommitSHA   string `json:"commitSHA"`
	HeadSHA     string `json:"headSHA"`
	BaseSHA     string `json:"baseSHA"`
	TreeSHA     string `json:"treeSHA"`
	PullRequest int    `json:"pullRequest"`
}

func New(credential forge.Credential) (*Writer, error) {
	if !project.Supported(credential.Repository) {
		return nil, ErrMerge
	}
	if _, e := forge.New(credential, credential.Repository); e != nil {
		return nil, ErrMerge
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, DialContext: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 8192}
	return &Writer{token: credential.Token, base: "https://api.github.com", repository: credential.Repository, http: &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// mergeMessage is the merge commit body. It carries no Signed-off-by: GitHub
// sets the merge commit's author and committer from the credential's account,
// so a trailer naming any other identity would attest a sign-off by someone
// who did not create the commit. The merged change keeps the publication
// commit's sign-off by the installation's configured commit identity.
func mergeMessage(issue int) string { return "Refs #" + strconv.Itoa(issue) }

// Merge sends exactly one PUT, guarded by the expected head SHA and native
// protection. The caller must first verify a strict applicable ruleset with no
// bypass actors. This transport never treats a failure as proof of no effect.
// It does not retry, adopt an already-merged PR, close an issue or deploy.
func (w *Writer) Merge(ctx context.Context, e mergegate.Expected, issue int, fence Fence) (Receipt, error) {
	deny := func() (Receipt, error) { return Receipt{}, ErrMerge }
	if w == nil || w.http == nil || e.Repository != w.repository || !project.Supported(w.repository) || fence == nil || issue < 1 || issue > 2147483647 || e.PullRequest < 1 || e.PullRequest > 2147483647 || e.HeadSHA == e.BaseSHA {
		return deny()
	}
	match := branch.FindStringSubmatch(e.Branch)
	if len(match) != 2 || match[1] != strconv.Itoa(issue) {
		return deny()
	}
	for _, sha := range []string{e.HeadSHA, e.BaseSHA, e.TreeSHA} {
		if !workspacebundle.ValidSourceSHA(sha) {
			return deny()
		}
	}
	// The fresh authority owns a tighter deadline; the transport cannot create one
	// on behalf of an unbounded caller or outlive its original context.
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) || deadline.After(time.Now().Add(90*time.Second)) {
		return deny()
	}
	title := "fix(ai-sdlc): integrate issue #" + strconv.Itoa(issue) + " [skip release]"
	value := struct {
		SHA     string `json:"sha"`
		Method  string `json:"merge_method"`
		Title   string `json:"commit_title"`
		Message string `json:"commit_message"`
	}{e.HeadSHA, "merge", title, mergeMessage(issue)}
	body, _ := json.Marshal(value)
	path := "/repos/" + w.repository + "/pulls/" + strconv.Itoa(e.PullRequest) + "/merge"
	binding, _ := json.Marshal(struct {
		Method, Path string
		Body         json.RawMessage
	}{http.MethodPut, path, body})
	digest := authority.Digest(binding)
	if ctx.Err() != nil || fence(ctx, digest) != nil {
		return deny()
	}
	var response struct {
		Merged *bool
		SHA    string
	}
	if w.request(ctx, http.MethodPut, path, body, &response) != nil || response.Merged == nil || !*response.Merged || !workspacebundle.ValidSourceSHA(response.SHA) || response.SHA == e.HeadSHA || response.SHA == e.BaseSHA {
		return deny()
	}
	var commit struct {
		SHA, Message string
		Tree         struct{ SHA string }
		Parents      []struct{ SHA string }
	}
	if w.request(ctx, http.MethodGet, "/repos/"+w.repository+"/git/commits/"+response.SHA, nil, &commit) != nil || commit.SHA != response.SHA || commit.Tree.SHA != e.TreeSHA || len(commit.Parents) != 2 || commit.Parents[0].SHA != e.BaseSHA || commit.Parents[1].SHA != e.HeadSHA || !strings.HasPrefix(commit.Message, title+"\n") {
		return deny()
	}
	if ctx.Err() != nil || fence(ctx, digest) != nil {
		return deny()
	}
	return Receipt{"ai-sdlc.merge-receipt/v1", response.SHA, e.HeadSHA, e.BaseSHA, e.TreeSHA, e.PullRequest}, nil
}
func (w *Writer) request(ctx context.Context, method, path string, body []byte, out any) error {
	request, e := http.NewRequestWithContext(ctx, method, w.base+path, bytes.NewReader(body))
	if e != nil {
		return ErrMerge
	}
	request.GetBody = nil
	request.Close = true
	request.Header.Set("Authorization", "Bearer "+w.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, e := w.http.Do(request)
	if e != nil {
		return ErrMerge
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength > 65536 || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Link") != "" {
		return ErrMerge
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 65537))
	if e != nil || len(raw) == 0 || len(raw) > 65536 || !utf8.Valid(raw) || ctx.Err() != nil {
		return ErrMerge
	}
	var object map[string]json.RawMessage
	if authority.DecodeStrict(raw, &object) != nil || object == nil || json.Unmarshal(raw, out) != nil {
		return ErrMerge
	}
	return nil
}

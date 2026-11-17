// Package terminalwriter publishes only an authenticated, deployed source.
package terminalwriter

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
)

var ErrRelease = errors.New("terminal release transport unavailable or denied")

type Writer struct {
	token, base, repository string
	http                    *http.Client
}
type Fence func(context.Context, releasecompletion.EffectRequest) error

func New(credential forge.Credential) (*Writer, error) {
	if credential.Repository != "os-santiago/saferus" {
		return nil, ErrRelease
	}
	if _, e := forge.New(credential, credential.Repository); e != nil {
		return nil, ErrRelease
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, DialContext: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 8192}
	return &Writer{token: credential.Token, base: "https://api.github.com", repository: credential.Repository, http: &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (w *Writer) request(ctx context.Context, method, path string, body []byte, status int, out any) error {
	request, e := http.NewRequestWithContext(ctx, method, w.base+path, bytes.NewReader(body))
	if e != nil {
		return ErrRelease
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
		return ErrRelease
	}
	defer response.Body.Close()
	if response.StatusCode != status || response.ContentLength > 65536 || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Link") != "" {
		return ErrRelease
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 65537))
	if e != nil || len(raw) == 0 || len(raw) > 65536 || !utf8.Valid(raw) || ctx.Err() != nil {
		return ErrRelease
	}
	var object map[string]json.RawMessage
	if authority.DecodeStrict(raw, &object) != nil || object == nil || json.Unmarshal(raw, out) != nil {
		return ErrRelease
	}
	return nil
}

type reference struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}
type release struct {
	ID          int64      `json:"id"`
	Tag         string     `json:"tag_name"`
	Target      string     `json:"target_commitish"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	Draft       *bool      `json:"draft"`
	Prerelease  *bool      `json:"prerelease"`
	URL         string     `json:"html_url"`
	PublishedAt *time.Time `json:"published_at"`
}
type issue struct {
	ID     int64           `json:"id"`
	Number int             `json:"number"`
	State  string          `json:"state"`
	Reason string          `json:"state_reason"`
	Pull   json.RawMessage `json:"pull_request"`
}

func validRef(r reference, p releasecompletion.Plan) bool {
	return r.Ref == "refs/tags/"+p.Tag && r.Object.Type == "commit" && r.Object.SHA == p.SourceSHA
}
func validRelease(r release, p releasecompletion.Plan, id int64, draft bool) bool {
	if r.ID <= 0 || (id > 0 && r.ID != id) || r.Tag != p.Tag || r.Target != p.SourceSHA || r.Name != p.Title || r.Body != p.Body || r.Draft == nil || *r.Draft != draft || r.Prerelease == nil || *r.Prerelease {
		return false
	}
	if draft {
		return r.PublishedAt == nil
	}
	return r.PublishedAt != nil && !r.PublishedAt.After(time.Now().Add(5*time.Second)) && releasecompletion.ValidReleaseURL(r.URL, p)
}
func (w *Writer) ref(ctx context.Context, p releasecompletion.Plan) error {
	var r reference
	if w.request(ctx, http.MethodGet, "/repos/"+p.Repository+"/git/ref/tags/"+url.PathEscape(p.Tag), nil, 200, &r) != nil || !validRef(r, p) {
		return ErrRelease
	}
	return nil
}

// Execute performs one mutation. The fence must atomically consume the original
// journal send authority. An error after that fence is ambiguous, never retryable.
// Each later step requires its own persisted predecessor receipt in the caller.
func (w *Writer) Execute(ctx context.Context, p releasecompletion.Plan, step string, releaseID int64, fence Fence) (releasecompletion.EffectReceipt, error) {
	deny := func() (releasecompletion.EffectReceipt, error) { return releasecompletion.EffectReceipt{}, ErrRelease }
	now := time.Now()
	deadline, ok := ctx.Deadline()
	if w == nil || w.http == nil || w.repository != p.Repository || fence == nil || !ok || !deadline.After(now) || deadline.After(time.UnixMilli(p.ExpiresAt)) || deadline.After(now.Add(releasecompletion.MaxAge)) {
		return deny()
	}
	req, e := releasecompletion.Request(p, step, releaseID, now)
	if e != nil {
		return deny()
	}
	var old release
	var original issue
	base := "/repos/" + p.Repository
	// POST /git/refs itself rejects an existing ref, including an equal SHA. No
	// successful lookup is ever adopted as proof of our own create.
	if step != "tag" && w.ref(ctx, p) != nil {
		return deny()
	}
	if step == "publish" || step == "close" {
		if w.request(ctx, http.MethodGet, base+"/releases/"+strconv.FormatInt(releaseID, 10), nil, 200, &old) != nil || !validRelease(old, p, releaseID, step == "publish") {
			return deny()
		}
	}
	if step == "close" {
		if w.request(ctx, http.MethodGet, base+"/issues/"+strconv.Itoa(p.IssueNumber), nil, 200, &original) != nil || original.ID <= 0 || original.Number != p.IssueNumber || original.State != "open" || len(original.Pull) != 0 {
			return deny()
		}
	}
	if ctx.Err() != nil || p.ValidateStructure(time.Now()) != nil || fence(ctx, req) != nil {
		return deny()
	}
	r := releasecompletion.EffectReceipt{Step: step, PlanSHA256: p.Digest(), RequestSHA256: req.Digest(), Tag: p.Tag, CommitSHA: p.SourceSHA}
	switch step {
	case "tag":
		var response reference
		if w.request(ctx, req.Method, req.Path, req.Body, 201, &response) != nil || !validRef(response, p) {
			return deny()
		}
	case "draft", "publish":
		status := 200
		if step == "draft" {
			status = 201
		}
		var response release
		if w.request(ctx, req.Method, req.Path, req.Body, status, &response) != nil || !validRelease(response, p, releaseID, step == "draft") {
			return deny()
		}
		r.ReleaseID = response.ID
		r.BodySHA256 = authority.Digest([]byte(p.Body))
		if step == "publish" {
			r.ReleaseURL = response.URL
		}
	case "close":
		var response issue
		if w.request(ctx, req.Method, req.Path, req.Body, 200, &response) != nil || response.ID != original.ID || response.Number != p.IssueNumber || response.State != "closed" || response.Reason != "completed" || len(response.Pull) != 0 {
			return deny()
		}
		r.ReleaseID = releaseID
		r.ReleaseURL = old.URL
		r.BodySHA256 = authority.Digest([]byte(p.Body))
		r.IssueID = response.ID
		r.IssueNumber = p.IssueNumber
	default:
		return deny()
	}
	if w.ref(ctx, p) != nil || ctx.Err() != nil || p.ValidateStructure(time.Now()) != nil {
		return deny()
	}
	r.ObservedAt = time.Now().UnixMilli()
	return r, nil
}

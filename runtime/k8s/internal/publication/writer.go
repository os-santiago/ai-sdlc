package publication

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Writer is an inactive transport primitive. Its trusted caller MUST commit a
// fresh effect intent before calling, revalidate current durable authority via
// fence, and quarantine every ambiguous result. It cannot resume/adopt a branch,
// update a ref, merge, release, deploy or derive authority from a model response.
// Only supported repositories are accepted and URLs are fixed. The commit
// identity comes from the plan, which resolves it from the installed manifest.
type Writer struct {
	repository, token, base string
	http                    *http.Client
}
type WriteFence func(context.Context, string) error

var pilotBranch = regexp.MustCompile(`^ai-sdlc/issue-([1-9][0-9]{0,9})-[a-f0-9]{32}$`)

func NewWriter(credential forge.Credential) (*Writer, error) {
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
	return &Writer{credential.Repository, credential.Token, "https://api.github.com", &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Exactly one non-replayable POST. No response body, credential, URL or generated
// source appears in an error. A non-201 response is not proof that nothing happened.
func (w *Writer) post(ctx context.Context, fence WriteFence, suffix string, value, out any) error {
	if w == nil || w.http == nil || fence == nil || !project.Supported(w.repository) {
		return ErrPublication
	}
	switch suffix {
	case "/git/trees", "/git/commits", "/git/refs", "/pulls":
	default:
		return ErrPublication
	}
	raw, e := json.Marshal(value)
	if e != nil || len(raw) < 1 || len(raw) > 2*1024*1024 {
		return ErrPublication
	}
	requestDigest := authority.Digest(raw)
	if ctx.Err() != nil || fence(ctx, requestDigest) != nil {
		return ErrPublication
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, w.base+"/repos/"+w.repository+suffix, bytes.NewReader(raw))
	if e != nil {
		return ErrPublication
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, e := w.http.Do(req)
	if e != nil {
		return ErrPublication
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated || response.ContentLength > 1024*1024 || response.Header.Get("Content-Encoding") != "" {
		return ErrPublication
	}
	raw, e = io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if e != nil || len(raw) < 1 || len(raw) > 1024*1024 || !utf8.Valid(raw) {
		return ErrPublication
	}
	// GitHub may add metadata fields; retain strict duplicate-key rejection before
	// projecting its response onto the finite fields that bind this operation.
	var object map[string]json.RawMessage
	if authority.DecodeStrict(raw, &object) != nil || object == nil || json.Unmarshal(raw, out) != nil || ctx.Err() != nil || fence(ctx, requestDigest) != nil {
		return ErrPublication
	}
	return nil
}
func validatePlan(repository string, p TreePlan) (int, error) {
	manifest, ok := project.Lookup(repository)
	if !ok || !project.Supported(repository) || p.Repository != repository {
		return 0, ErrPublication
	}
	// Reference recipes change an exact file set; other repositories change
	// between one file and the manifest's limit.
	countOK := len(p.Request.Tree) >= 1 && len(p.Request.Tree) <= manifest.Publication.MaxChangedFiles
	if len(manifest.Recipes) > 0 {
		countOK = len(p.Request.Tree) == manifest.Publication.MaxChangedFiles
	}
	if !countOK {
		return 0, ErrPublication
	}
	match := pilotBranch.FindStringSubmatch(p.Branch)
	if match == nil {
		return 0, ErrPublication
	}
	n, e := strconv.Atoi(match[1])
	if e != nil || n > 2147483647 || p.CommitMessage != "fix(ai-sdlc): address issue #"+match[1]+" [skip release]" || p.CommitIdentity.Validate() != nil || !workspacebundle.ValidSourceSHA(p.Request.BaseTree) || !workspacebundle.ValidSourceSHA(p.ExpectedTreeSHA) {
		return 0, ErrPublication
	}
	paths, total := map[string]bool{}, 0
	for _, v := range p.Request.Tree {
		total += len(v.Content)
		if !manifest.Publication.Allows(v.Path) || paths[v.Path] || v.Mode != "100644" || v.Type != "blob" || len(v.Content) == 0 || len(v.Content) > 512*1024 || len(v.Content) > manifest.Publication.MaxFileBytes || total > manifest.Publication.MaxChangedBytes || !utf8.ValidString(v.Content) || strings.ContainsRune(v.Content, 0) {
			return 0, ErrPublication
		}
		paths[v.Path] = true
	}
	return n, nil
}
func (w *Writer) validatePlan(p TreePlan) (int, error) {
	if w == nil {
		return 0, ErrPublication
	}
	return validatePlan(w.repository, p)
}
func (w *Writer) CreateTree(ctx context.Context, fence WriteFence, p TreePlan) (string, error) {
	if _, e := w.validatePlan(p); e != nil {
		return "", e
	}
	var result struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
	}
	if w.post(ctx, fence, "/git/trees", p.Request, &result) != nil || result.SHA != p.ExpectedTreeSHA || result.Truncated {
		return "", ErrPublication
	}
	return result.SHA, nil
}

type commitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}
type commitRequest struct {
	Message   string       `json:"message"`
	Tree      string       `json:"tree"`
	Parents   []string     `json:"parents"`
	Author    commitPerson `json:"author"`
	Committer commitPerson `json:"committer"`
}

// commitBody is the single builder of the commit request a publication
// sends. The identity is the author, the committer and the Signed-off-by
// trailer; the date is the claim's commit time.
func commitBody(subject, tree, parent string, identity project.CommitIdentity, date time.Time) commitRequest {
	person := commitPerson{identity.Name, identity.Email, date.UTC().Format(time.RFC3339)}
	message := subject + "\n\nSigned-off-by: " + identity.Name + " <" + identity.Email + ">\n"
	return commitRequest{message, tree, []string{parent}, person, person}
}

// commitSHA is the Git commit object identity of a commit request: the SHA
// the forge assigns to exactly that commit (one parent, UTC timestamps).
func commitSHA(c commitRequest) string {
	author, e1 := time.Parse(time.RFC3339, c.Author.Date)
	committer, e2 := time.Parse(time.RFC3339, c.Committer.Date)
	if e1 != nil || e2 != nil || len(c.Parents) != 1 {
		return ""
	}
	person := func(p commitPerson, date time.Time) string {
		return fmt.Sprintf("%s <%s> %d +0000", p.Name, p.Email, date.Unix())
	}
	raw := fmt.Sprintf("tree %s\nparent %s\nauthor %s\ncommitter %s\n\n%s", c.Tree, c.Parents[0], person(c.Author, author), person(c.Committer, committer), c.Message)
	return hex.EncodeToString(objectID("commit", []byte(raw)))
}

// The request bodies below, like commitBody and commitSHA, are shared with
// the read-only reconciler, which re-derives them to prove which journaled
// write it observes.
type refRequest struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

func branchBody(branch, commit string) refRequest {
	return refRequest{"refs/heads/" + branch, commit}
}

type pullRequestRequest struct {
	Title               string `json:"title"`
	Body                string `json:"body"`
	Head                string `json:"head"`
	Base                string `json:"base"`
	Draft               bool   `json:"draft"`
	MaintainerCanModify bool   `json:"maintainer_can_modify"`
}

// proofMarker is the line of the pull request body that carries the plan digest.
func proofMarker(proofDigest string) string {
	return "Publication proof SHA-256: `" + proofDigest + "`."
}
func pullRequestBody(repository string, issue int, title, branch, proofDigest string) pullRequestRequest {
	return pullRequestRequest{Title: title, Body: IssueReference(repository, issue) + "\n\nAI-SDLC verified candidate.\n" + proofMarker(proofDigest) + "\nIt merges only after the repository's required checks pass.\n", Head: branch, Base: "main"}
}

// CreateCommit uses the plan's recorded identity, not the current
// installation's, and accepts only the commit object it planned.
func (w *Writer) CreateCommit(ctx context.Context, fence WriteFence, p TreePlan, sourceSHA string, date time.Time) (string, error) {
	if _, e := w.validatePlan(p); e != nil || !workspacebundle.ValidSourceSHA(sourceSHA) || date.IsZero() || date.Nanosecond() != 0 || date.Before(time.Now().Add(-time.Hour)) || date.After(time.Now().Add(time.Minute)) {
		return "", ErrPublication
	}
	value := commitBody(p.CommitMessage, p.ExpectedTreeSHA, sourceSHA, p.CommitIdentity, date)
	message, person, planned := value.Message, value.Author, commitSHA(value)
	var result struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Tree    struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Author    commitPerson `json:"author"`
		Committer commitPerson `json:"committer"`
	}
	if w.post(ctx, fence, "/git/commits", value, &result) != nil || !workspacebundle.ValidSourceSHA(result.SHA) || result.SHA != planned || result.Tree.SHA != p.ExpectedTreeSHA || len(result.Parents) != 1 || result.Parents[0].SHA != sourceSHA || strings.TrimSuffix(result.Message, "\n") != strings.TrimSuffix(message, "\n") || result.Author != person || result.Committer != person {
		return "", ErrPublication
	}
	return result.SHA, nil
}
func (w *Writer) CreateBranch(ctx context.Context, fence WriteFence, p TreePlan, commit string) error {
	if _, e := w.validatePlan(p); e != nil || !workspacebundle.ValidSourceSHA(commit) {
		return ErrPublication
	}
	value := branchBody(p.Branch, commit)
	ref := value.Ref
	var result struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if w.post(ctx, fence, "/git/refs", value, &result) != nil || result.Ref != ref || result.Object.Type != "commit" || result.Object.SHA != commit {
		return ErrPublication
	}
	return nil
}

type PublishedPullRequest struct {
	Number               int
	HeadSHA, Branch, URL string
}

func (w *Writer) CreatePullRequest(ctx context.Context, fence WriteFence, p TreePlan, sourceSHA, commit, proofDigest string) (PublishedPullRequest, error) {
	number, e := w.validatePlan(p)
	if e != nil || !workspacebundle.ValidSourceSHA(sourceSHA) || !workspacebundle.ValidSourceSHA(commit) || !workspacebundle.ValidDigest(proofDigest) {
		return PublishedPullRequest{}, ErrPublication
	}
	value := pullRequestBody(w.repository, number, p.CommitMessage, p.Branch, proofDigest)
	type end struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	}
	var result struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		URL    string `json:"html_url"`
		Head   end    `json:"head"`
		Base   end    `json:"base"`
	}
	if w.post(ctx, fence, "/pulls", value, &result) != nil || result.Number < 1 || result.Number > 2147483647 || result.State != "open" || result.Draft || result.Head.Ref != p.Branch || result.Head.SHA != commit || result.Head.Repo.FullName != w.repository || result.Base.Ref != "main" || result.Base.SHA != sourceSHA || result.Base.Repo.FullName != w.repository || result.URL != "https://github.com/"+w.repository+"/pull/"+strconv.Itoa(result.Number) {
		return PublishedPullRequest{}, ErrPublication
	}
	return PublishedPullRequest{result.Number, commit, p.Branch, result.URL}, nil
}

// IssueReference links the pull request to its issue. For a repository
// governed by a project manifest the merge is the final delivery stage, so
// the pull request closes the issue. The built-in references continue to
// build, attest and deploy after the merge, so their issues stay open.
func IssueReference(repository string, issue int) string {
	if _, builtin := project.Builtin(repository); builtin {
		return "Refs #" + strconv.Itoa(issue)
	}
	return "Closes #" + strconv.Itoa(issue)
}

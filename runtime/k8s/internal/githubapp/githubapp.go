// Package githubapp mints short-lived GitHub App installation access
// tokens — the only pipeline identity this runtime (or
// .github/workflows/ai-sdlc-implement.yml) uses for git/gh auth. Fail
// closed: there is deliberately no PAT or GITHUB_TOKEN fallback here
// (root CLAUDE.md "Critical Lessons"; ai-sdlc-implement.yml's "Verify
// ai-sdlc App credentials" step, issue #26). components/worker/cmd/main.go
// treats a missing AppID/PrivateKey as "checkout is not configured", not
// as a reason to substitute a weaker credential.
//
// This replicates what actions/create-github-app-token@v2 does in a GitHub
// Actions run, using only the standard library (RS256 JWT, signed with
// the App's RSA private key):
//
//  1. Sign a short-lived JWT (iss=AppID, exp<=10min) — GitHub App auth,
//     https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app
//  2. GET /repos/{owner}/{repo}/installation to resolve the installation
//     id for the target repository (Bearer: the JWT).
//  3. POST /app/installations/{id}/access_tokens, scoped to just that one
//     repository (Bearer: the JWT) — mirrors create-github-app-token's
//     `repositories:` input, never an org-wide token.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Token is a short-lived (~1h, per the GitHub API) installation access
// token scoped to one repository.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// App mints installation tokens for one GitHub App. AppID and PrivateKey
// are required; everything else has a working default.
type App struct {
	AppID      string
	PrivateKey []byte // PEM-encoded RSA private key (PKCS#1 or PKCS#8)
	BaseURL    string // default https://api.github.com; override in tests
	HTTPClient *http.Client
	Clock      func() time.Time // default time.Now; override in tests
}

func (a App) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "https://api.github.com"
}

func (a App) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

func (a App) now() time.Time {
	if a.Clock != nil {
		return a.Clock()
	}
	return time.Now()
}

// InstallationToken mints a token scoped to exactly one repository
// ("owner/name") — the credential components/worker's checkout boundary
// (internal/checkout) embeds into a short-lived, never-persisted git
// remote URL.
func (a App) InstallationToken(ctx context.Context, repository string) (Token, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return Token{}, err
	}
	jwt, err := a.signedJWT()
	if err != nil {
		return Token{}, fmt.Errorf("githubapp: sign app jwt: %w", err)
	}
	installationID, err := a.installationID(ctx, jwt, owner, name)
	if err != nil {
		return Token{}, err
	}
	return a.mintAccessToken(ctx, jwt, installationID, name)
}

func splitRepository(repository string) (owner, name string, err error) {
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("githubapp: repository %q is not owner/name", repository)
	}
	return parts[0], parts[1], nil
}

// signedJWT builds the RS256 JWT GitHub App auth requires: a 60s
// clock-skew backdated iat, a <=10min exp (we use 9m), iss=AppID.
func (a App) signedJWT() (string, error) {
	if a.AppID == "" || len(a.PrivateKey) == 0 {
		return "", errors.New("githubapp: AppID and PrivateKey are both required")
	}
	key, err := parsePrivateKey(a.PrivateKey)
	if err != nil {
		return "", err
	}
	now := a.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.AppID,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64URL(headerJSON) + "." + base64URL(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: sign jwt: %w", err)
	}
	return signingInput + "." + base64URL(signature), nil
}

func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("githubapp: PrivateKey is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("githubapp: parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("githubapp: private key is not RSA")
	}
	return key, nil
}

func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a App) installationID(ctx context.Context, jwt, owner, name string) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/installation", a.baseURL(), owner, name)
	var out struct {
		ID int64 `json:"id"`
	}
	if err := a.doJSON(ctx, http.MethodGet, url, jwt, nil, &out); err != nil {
		return 0, fmt.Errorf("githubapp: resolve installation for %s/%s: %w", owner, name, err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("githubapp: no installation found for %s/%s — is the ai-sdlc App installed on this repo?", owner, name)
	}
	return out.ID, nil
}

func (a App) mintAccessToken(ctx context.Context, jwt string, installationID int64, repoName string) (Token, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.baseURL(), installationID)
	body, err := json.Marshal(map[string]any{"repositories": []string{repoName}})
	if err != nil {
		return Token{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.doJSON(ctx, http.MethodPost, url, jwt, body, &out); err != nil {
		return Token{}, fmt.Errorf("githubapp: mint installation access token: %w", err)
	}
	if out.Token == "" {
		return Token{}, errors.New("githubapp: access token response had no token field")
	}
	return Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

func (a App) doJSON(ctx context.Context, method, url, bearer string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

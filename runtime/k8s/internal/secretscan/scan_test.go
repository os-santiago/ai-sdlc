package secretscan

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Test values are assembled at run time from split prefixes and a generated
// body, so this file never contains a literal that a repository secret
// scanner (or this package) would report.

// body returns n characters from a fixed, varied alphabet starting at offset.
func body(n, offset int, alphabet string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[(offset+i*7)%len(alphabet)])
	}
	return b.String()
}

const (
	base62 = "aB3dE5fG7hJ9kL2mN4pQ6rS8tU0vW1xYzACDFHKMPRTVXZbcegijnoqsuwy"
	base32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	b64    = base62 + "+/"
)

func awsKeyID() string      { return "AK" + "IA" + body(16, 3, base32) }
func awsSecret() string     { return body(40, 5, b64) }
func githubClassic() string { return "gh" + "p_" + body(36, 1, base62) }
func githubFine() string    { return "github" + "_pat_" + body(22, 2, base62) + "_" + body(59, 4, base62) }
func gitlabToken() string   { return "gl" + "pat-" + body(20, 6, base62) }
func googleKey() string     { return "AI" + "za" + body(35, 8, base62) }
func azureKey() string      { return "Account" + "Key=" + body(86, 9, b64) + "==" }
func keyHeaderLine() string { return "-----BEGIN " + "RSA PRIVATE" + " KEY-----" }
func keyBodyLine() string   { return body(64, 10, b64) }

var sha = strings.Repeat("a", 40)

func bundle(files map[string]string) workspacebundle.Bundle {
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: sha, Files: []workspacebundle.File{}}
	for p, data := range files {
		b.Files = append(b.Files, workspacebundle.File{Path: p, Mode: 0644, Data: []byte(data)})
	}
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	return b
}

// scan builds the candidate delta between two trees and scans it.
func scan(t *testing.T, before, after map[string]string) (Finding, bool) {
	t.Helper()
	source := bundle(before)
	d, err := candidate.Build(source, bundle(after))
	if err != nil {
		t.Fatalf("build delta: %v", err)
	}
	return Scan(source, d)
}

func added(t *testing.T, file, content string) (Finding, bool) {
	t.Helper()
	return scan(t, map[string]string{"README.md": "base\n"}, map[string]string{"README.md": "base\n", file: content})
}

func TestEachRuleFindsItsSecretInAnAddedLine(t *testing.T) {
	cases := []struct {
		rule, file, content string
	}{
		{RulePrivateKey, "deploy/key.pem", keyHeaderLine() + "\n" + keyBodyLine() + "\n" + "-----END RSA PRIVATE KEY-----\n"},
		{RulePrivateKey, "config/sa.json", `{"private_key": "` + "-----BEGIN " + "PRIVATE" + " KEY-----\\n" + keyBodyLine() + `\n"}`},
		{RulePrivateKey, "main.go", "const k = `" + keyHeaderLine() + "\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00\n\n" + keyBodyLine() + "\n`\n"},
		{RulePrivateKey, "Keys.java", "String k = \"" + keyHeaderLine() + "\\n\"\n    + \"" + keyBodyLine() + "\\n\"\n    + \"-----END RSA PRIVATE KEY-----\";"},
		{RulePrivateKey, "keys.go", "k := \"" + keyHeaderLine() + "\\n\" +\n\t\"" + keyBodyLine() + "\\n\" +\n"},
		{RulePrivateKey, "id_ed25519", "-----BEGIN " + "OPENSSH PRIVATE" + " KEY-----\n" + keyBodyLine() + "\n"},
		{RuleAWSAccessKeyID, "main.go", `id := "` + awsKeyID() + `"`},
		{RuleAWSAccessKeyID, "settings.yaml", "key: " + "AS" + "IA" + body(16, 11, base32)},
		{RuleAWSSecretAccessKey, ".env", "AWS_SECRET_ACCESS_KEY=" + awsSecret()},
		{RuleAWSSecretAccessKey, "app.py", `aws_secret_access_key = "` + awsSecret() + `"`},
		{RuleAWSSecretAccessKey, "creds.json", `"awsSecretAccessKey": "` + awsSecret() + `"`},
		{RuleGitHubToken, "client.go", `token := "` + githubClassic() + `"`},
		{RuleGitHubToken, "client.go", "Authorization: Bearer " + "gh" + "s_" + body(36, 12, base62)},
		{RuleGitHubToken, "ci.yml", "token: " + githubFine()},
		{RuleGitLabToken, "notes.txt", "PRIVATE-TOKEN: " + gitlabToken()},
		{RuleGoogleAPIKey, "web/app.js", `const key = "` + googleKey() + `";`},
		{RuleAzureStorageKey, "app.config", "DefaultEndpointsProtocol=https;AccountName=x;" + azureKey() + ";EndpointSuffix=core.windows.net"},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i, "-", c.rule), func(t *testing.T) {
			finding, ok := added(t, c.file, "package x\n"+c.content+"\n")
			if !ok || finding.Rule != c.rule || finding.PathClass != PathClass(c.file) {
				t.Fatalf("finding = %+v, %v; want rule %s", finding, ok, c.rule)
			}
		})
	}
}

func TestPlaceholdersExamplesAndLookalikesAreNotFindings(t *testing.T) {
	cases := []string{
		// Documented vendor examples.
		"AK" + "IAIOSFODNN7" + "EXAMPLE",
		"aws_secret" + "_access_key = " + "wJalrXUtnFEMI" + "/K7MDENG/bPxRfiCY" + "EXAMPLE" + "KEY",
		// Placeholders without randomness.
		"gh" + "p_" + strings.Repeat("x", 36),
		"gh" + "p_" + strings.Repeat("0123", 9),
		"gl" + "pat-" + strings.Repeat("X", 20),
		"AI" + "za" + strings.Repeat("A", 35),
		"Account" + "Key=" + strings.Repeat("A", 86) + "==",
		"aws_secret_access_key = " + strings.Repeat("a", 40),
		"aws_secret_access_key = ${AWS_SECRET_ACCESS_KEY}",
		// A prefix inside a longer identifier, or a value of the wrong length.
		"x" + awsKeyID(),
		awsKeyID() + "Z",
		"AK" + "IA" + body(15, 3, base32),
		"AK" + "IA" + body(16, 3, base62), // lowercase is not an access key ID
		"my" + githubClassic(),
		githubClassic() + "a",
		"gh" + "x_" + body(36, 1, base62),
		"AI" + "za" + body(34, 8, base62),
		"AI" + "za" + body(36, 8, base62),
		"Account" + "Key=" + body(86, 9, b64),
		// A 40-character value without the assignment keyword.
		"checksum: " + awsSecret(),
		// Code that names a private key header without key material.
		`if strings.HasPrefix(s, "` + keyHeaderLine() + `") {`,
		`return errors.New("expected ` + keyHeaderLine() + `")`,
		keyHeaderLine() + "\n" + "}\n",
		keyHeaderLine() + "\n" + strings.Repeat("abcdefghij", 5) + "\n", // a word, not encoded key bytes
		"-----BEGIN " + "PUBLIC" + " KEY-----\n" + keyBodyLine(),
		"-----BEGIN " + "CERTIFICATE-----\n" + keyBodyLine(),
		// Ordinary code.
		"func main() { fmt.Println(\"hello\") }",
		"sum := sha256.Sum256(data) // " + strings.Repeat("f", 64),
	}
	for i, content := range cases {
		if finding, ok := added(t, "main.go", "package x\n"+content+"\n"); ok {
			t.Errorf("case %d: unexpected finding %+v", i, finding)
		}
	}
}

func TestOnlyAddedLinesAreScanned(t *testing.T) {
	token := githubClassic()
	existing := "package x\n// " + token + "\nfunc a() {}\n"
	t.Run("an edit elsewhere in a file that already holds a secret", func(t *testing.T) {
		if f, ok := scan(t, map[string]string{"a.go": existing}, map[string]string{"a.go": existing + "func b() {}\n"}); ok {
			t.Fatalf("unexpected finding %+v", f)
		}
	})
	t.Run("a moved existing line", func(t *testing.T) {
		moved := "package x\nfunc a() {}\n// " + token + "\n"
		if f, ok := scan(t, map[string]string{"a.go": existing}, map[string]string{"a.go": moved}); ok {
			t.Fatalf("unexpected finding %+v", f)
		}
	})
	t.Run("a deleted file", func(t *testing.T) {
		if f, ok := scan(t, map[string]string{"a.go": existing, "b.go": "package x\n"}, map[string]string{"b.go": "package x\n// edit\n"}); ok {
			t.Fatalf("unexpected finding %+v", f)
		}
	})
	t.Run("a removed secret", func(t *testing.T) {
		if f, ok := scan(t, map[string]string{"a.go": existing}, map[string]string{"a.go": "package x\nfunc a() {}\n"}); ok {
			t.Fatalf("unexpected finding %+v", f)
		}
	})
	t.Run("the same secret added to another file", func(t *testing.T) {
		if f, ok := scan(t, map[string]string{"a.go": existing}, map[string]string{"a.go": existing, "b.go": "package x\n// " + token + "\n"}); !ok || f.Rule != RuleGitHubToken {
			t.Fatalf("finding = %+v, %v", f, ok)
		}
	})
	t.Run("a changed line in a modified file", func(t *testing.T) {
		if f, ok := scan(t, map[string]string{"a.go": existing}, map[string]string{"a.go": existing + "var t = \"" + googleKey() + "\"\n"}); !ok || f.Rule != RuleGoogleAPIKey {
			t.Fatalf("finding = %+v, %v", f, ok)
		}
	})
	t.Run("a new key under an existing header line", func(t *testing.T) {
		before := keyHeaderLine() + "\n" + body(64, 20, b64) + "\n"
		after := keyHeaderLine() + "\n" + keyBodyLine() + "\n"
		if f, ok := scan(t, map[string]string{"k.pem": before}, map[string]string{"k.pem": after}); !ok || f.Rule != RulePrivateKey || f.PathClass != ClassKey {
			t.Fatalf("finding = %+v, %v", f, ok)
		}
	})
	t.Run("CRLF line endings", func(t *testing.T) {
		crlf := strings.ReplaceAll(existing, "\n", "\r\n")
		if f, ok := scan(t, map[string]string{"a.go": existing}, map[string]string{"a.go": crlf}); ok {
			t.Fatalf("unexpected finding %+v", f)
		}
	})
}

func TestFirstFindingIsDeterministic(t *testing.T) {
	after := map[string]string{"README.md": "base\n", "b.go": githubClassic() + "\n", "a.yaml": "x: " + awsKeyID() + "\n" + googleKey() + "\n"}
	for i := 0; i < 20; i++ {
		f, ok := scan(t, map[string]string{"README.md": "base\n"}, after)
		if !ok || f != (Finding{RuleAWSAccessKeyID, ClassConfig}) {
			t.Fatalf("finding = %+v, %v", f, ok)
		}
	}
}

func TestFindingNeverCarriesTheSecretOrThePath(t *testing.T) {
	secrets := []string{awsKeyID(), githubClassic(), githubFine(), gitlabToken(), googleKey(), azureKey(), keyBodyLine(), awsSecret()}
	contents := []string{awsKeyID(), githubClassic(), githubFine(), gitlabToken(), googleKey(), azureKey(), keyHeaderLine() + "\n" + keyBodyLine(), "AWS_SECRET_ACCESS_KEY=" + awsSecret()}
	file := "services/payments/very-specific-name.go"
	for i, content := range contents {
		f, ok := added(t, file, content+"\n")
		if !ok {
			t.Fatalf("case %d: no finding", i)
		}
		rendered := fmt.Sprintf("%v %+v %#v", f, f, f)
		for _, s := range secrets {
			if strings.Contains(rendered, s) || strings.Contains(rendered, s[:12]) {
				t.Fatalf("case %d: finding %q carries secret text", i, rendered)
			}
		}
		if strings.Contains(rendered, "payments") || strings.Contains(rendered, "very-specific") {
			t.Fatalf("case %d: finding %q carries the path", i, rendered)
		}
		if !KnownRule(f.Rule) || !KnownClass(f.PathClass) {
			t.Fatalf("case %d: unbounded finding %+v", i, f)
		}
	}
}

func TestRulesAreBoundedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range Rules() {
		if seen[id] || id == "" || len(id) > 32 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz-") != "" {
			t.Fatalf("rule id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 7 || KnownRule("generic-entropy") || KnownClass("secret") {
		t.Fatalf("rules = %v", Rules())
	}
}

func TestPathClass(t *testing.T) {
	for file, want := range map[string]string{
		"cmd/main.go": ClassCode, "web/app.TS": ClassCode, "scripts/run.sh": ClassCode,
		"config/app.yaml": ClassConfig, ".env": ClassConfig, "deploy/.env.production": ClassConfig, "settings.json": ClassConfig,
		"README.md": ClassDocs, "docs/guide.adoc": ClassDocs,
		"certs/server.pem": ClassKey, "home/.ssh/id_rsa": ClassKey, "release.asc": ClassKey,
		"Makefile": ClassOther, "data.bin": ClassOther,
	} {
		if got := PathClass(file); got != want {
			t.Errorf("PathClass(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestLargeDeltaWithinBoundsScansQuickly(t *testing.T) {
	// A single long line and many lines near the delta bound stay linear.
	long := strings.Repeat("A", 600*1024)
	many := strings.Repeat("value := compute(input) // "+strings.Repeat("x", 40)+"\n", 8000)
	if f, ok := scan(t, map[string]string{"README.md": "base\n"}, map[string]string{"README.md": "base\n", "long.txt": long, "many.go": many}); ok {
		t.Fatalf("unexpected finding %+v", f)
	}
}

// The right boundary of one value must not hide the value right after it:
// a rejected placeholder or documented example, or an accepted token,
// followed by a real token after a single separator.
func TestAdjacentValuesAreEachScanned(t *testing.T) {
	cases := []struct{ rule, content string }{
		{RuleGitHubToken, "gh" + "p_" + strings.Repeat("x", 36) + " " + githubClassic()},
		{RuleGitHubToken, "gh" + "p_" + strings.Repeat("x", 36) + "," + githubClassic()},
		{RuleAWSAccessKeyID, "AK" + "IAIOSFODNN7" + "EXAMPLE," + awsKeyID()},
		{RuleAWSAccessKeyID, "AK" + "IAIOSFODNN7" + "EXAMPLE " + awsKeyID()},
		{RuleGitLabToken, "gl" + "pat-" + strings.Repeat("X", 20) + " " + gitlabToken()},
		{RuleGoogleAPIKey, "AI" + "za" + strings.Repeat("A", 35) + "," + googleKey()},
		{RuleGitHubToken, githubClassic() + " " + "gh" + "o_" + body(36, 13, base62)},
		{RuleAWSAccessKeyID, awsKeyID() + "," + "AS" + "IA" + body(16, 11, base32)},
	}
	for i, c := range cases {
		if f, ok := added(t, "main.go", "package x\n"+c.content+"\n"); !ok || f.Rule != c.rule {
			t.Errorf("case %d: finding = %+v, %v; want %s", i, f, ok, c.rule)
		}
	}
	// Two rejected values side by side stay rejected.
	if f, ok := added(t, "main.go", "gh"+"p_"+strings.Repeat("x", 36)+" gh"+"p_"+strings.Repeat("y", 36)+"\n"); ok {
		t.Fatalf("unexpected finding %+v", f)
	}
}

// Each rule matches only the accepted value after resuming, never a value
// that starts inside the previous one.
func TestResumingNeverMatchesInsideAToken(t *testing.T) {
	r := rules[0] // aws-access-key-id
	if r.id != RuleAWSAccessKeyID {
		t.Fatal("rule order changed")
	}
	if r.matches("AK"+"IAIOSFODNN7"+"EXAMPLE") || r.matches("x"+awsKeyID()) || !r.matches(awsKeyID()) {
		t.Fatal("boundaries")
	}
}

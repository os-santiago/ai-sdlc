// The artifact builder is a trusted publisher, never a source-code executor.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/artifact"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildstorage"
)

const registryHost = "registry-service.kind-registry"

var outputPattern = regexp.MustCompile(`^registry-service\.kind-registry/ai-sdlc-fixtures/homedir:[a-z0-9][a-z0-9-]{0,62}$`)
var saferusOutputPattern = regexp.MustCompile(`^registry-service\.kind-registry/ai-sdlc-fixtures/saferus:[a-z0-9][a-z0-9-]{0,62}$`)

func validRecipeOutput(recipe, output string) bool {
	switch recipe {
	case artifact.Recipe:
		return outputPattern.MatchString(output)
	case artifact.SaferusRecipe:
		return saferusOutputPattern.MatchString(output)
	default:
		return false
	}
}

// registryTransport applies to every HTTP exchange, including bearer realms,
// upload Location URLs and redirects resolved by the registry client.
type registryTransport struct {
	base http.RoundTripper
	host string
}

func (t registryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != t.host || r.URL.User != nil {
		return nil, errors.New("registry request escaped its fixed HTTPS authority")
	}
	return t.base.RoundTrip(r)
}

func boundedFile(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file exceeds limit")
	}
	return b, nil
}
func registryAuth(file string) (authn.Authenticator, error) {
	b, e := boundedFile(file, 64<<10)
	if e != nil {
		return nil, e
	}
	var cfg struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if e = json.Unmarshal(b, &cfg); e != nil {
		return nil, errors.New("invalid registry configuration")
	}
	a, ok := cfg.Auths[registryHost]
	if !ok || (a.Auth == "" && (a.Username == "" || a.Password == "")) || a.IdentityToken != "" || a.RegistryToken != "" {
		return nil, errors.New("missing fixed registry basic identity")
	}
	return authn.FromConfig(a), nil
}
func run() error {
	guard := flag.Bool("guard-workspace", false, "verify installed local-kind workspace without publishing")
	storageID := flag.String("storage-identity", "", "installed physical storage identity")
	bootID := flag.String("node-boot-id", "", "installed node boot identity")
	phase := flag.String("workspace-phase", "", "build or publish workspace inspection")
	recipe := flag.String("recipe", artifact.Recipe, "reviewed artifact recipe")
	directory := flag.String("input", "/artifacts/quarkus-app", "read-only package directory")
	output := flag.String("output", "", "isolated internal registry image tag")
	source := flag.String("source-sha", "", "trusted original source revision")
	definition := flag.String("definition-digest", "", "trusted pipeline definition digest")
	runUID := flag.String("run-uid", "", "trusted original PipelineRun UID")
	ca := flag.String("ca", "/etc/pki/ca-trust/ca-bundle.crt", "public CA bundle")
	auth := flag.String("auth", "/registry-auth/.dockerconfigjson", "registry client credential file")
	results := flag.String("results", "/tekton/results", "trusted result directory")
	timeout := flag.Duration("timeout", 10*time.Minute, "total operation deadline")
	flag.Parse()
	if *guard {
		valid := flag.NArg() == 0
		allowed := map[string]bool{"guard-workspace": true, "storage-identity": true, "node-boot-id": true, "workspace-phase": true}
		flag.Visit(func(f *flag.Flag) {
			if !allowed[f.Name] {
				valid = false
			}
		})
		if !valid {
			return buildstorage.ErrCustody
		}
		proof, e := buildstorage.Inspect("/slot", *storageID, *bootID, *phase)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(proof)
	}
	if *storageID != "" || *bootID != "" || *phase != "" {
		return errors.New("workspace arguments require guard mode")
	}
	if flag.NArg() != 0 || !validRecipeOutput(*recipe, *output) || *timeout < time.Second || *timeout > 15*time.Minute {
		return errors.New("invalid publisher arguments")
	}
	id := artifact.Identity{SourceSHA: *source, DefinitionDigest: *definition, RunUID: *runUID}
	if e := id.Validate(); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	roots, e := x509.SystemCertPool()
	if e != nil {
		return e
	}
	pem, e := boundedFile(*ca, 2<<20)
	if e != nil || !roots.AppendCertsFromPEM(pem) {
		return errors.New("invalid public CA bundle")
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 4}
	defer transport.CloseIdleConnections()
	options := []remote.Option{remote.WithContext(ctx), remote.WithTransport(transport), remote.WithJobs(2)}

	var img v1.Image
	var report artifact.Report
	sourceURL := artifact.SourceURL
	if *recipe == artifact.SaferusRecipe {
		sourceURL = artifact.SaferusSourceURL
		img, report, e = artifact.AssembleSaferus(ctx, *directory, id)
	} else {
		baseRef, err := name.NewDigest(artifact.Base, name.StrictValidation)
		if err != nil {
			return err
		}
		base, err := remote.Image(baseRef, append(options, remote.WithAuth(authn.Anonymous), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))...)
		if err != nil {
			return errors.New("cannot fetch pinned base with verified TLS")
		}
		img, report, e = artifact.Assemble(ctx, base, *directory, id)
	}
	if e != nil {
		return e
	}
	credentials, e := registryAuth(*auth)
	if e != nil {
		return e
	}
	target, e := name.NewTag(*output, name.StrictValidation)
	if e != nil {
		return e
	}
	publishing := []remote.Option{remote.WithContext(ctx), remote.WithTransport(registryTransport{base: transport, host: registryHost}), remote.WithJobs(2), remote.WithAuth(credentials)}
	if e = remote.Write(target, img, publishing...); e != nil {
		return errors.New("registry publication failed")
	}
	observed, e := remote.Head(target, publishing...)
	if e != nil || observed.Digest.String() != report.ImageDigest {
		return errors.New("published registry digest mismatch")
	}
	// Results originate here, after registry verification; Maven has no mount here.
	values := map[string]string{"IMAGE_URL": target.Context().Name(), "IMAGE_DIGEST": report.ImageDigest, "CHAINS-GIT_URL": sourceURL, "CHAINS-GIT_COMMIT": id.SourceSHA, "DEFINITION_DIGEST": id.DefinitionDigest, "RUN_UID": id.RunUID}
	for key, value := range values {
		if e = os.WriteFile(filepath.Join(*results, key), []byte(value), 0600); e != nil {
			return errors.New("cannot write trusted publisher result")
		}
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

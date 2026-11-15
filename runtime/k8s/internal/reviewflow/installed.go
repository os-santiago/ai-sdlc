package reviewflow

import (
	"encoding/json"
	"io"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
)

type InstalledConfiguration struct {
	Version      string           `json:"version"`
	Profile      executor.Profile `json:"profile"`
	ADev         string           `json:"aDev"`
	WorkerImage  string           `json:"workerImage"`
	MailboxImage string           `json:"mailboxImage"`
}

func readInstalled(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrReview
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || len(b) == 0 || int64(len(b)) > limit {
		return nil, ErrReview
	}
	return b, nil
}

// LoadInstalled accepts no endpoint or command from the issue/config. The
// review gateway is a separate loopback sidecar with its own client identity.
func LoadInstalled(path, tokenPath string) (Configuration, error) {
	b, e := readInstalled(path, 128*1024)
	if e != nil {
		return Configuration{}, e
	}
	var installed InstalledConfiguration
	if authority.DecodeStrict(b, &installed) != nil || installed.Version != "ai-sdlc.independent-review-runtime/v1" || installed.Profile.Model != "codex-cli" {
		return Configuration{}, ErrReview
	}
	token, e := readInstalled(tokenPath, 512)
	if e != nil {
		return Configuration{}, e
	}
	profile, _ := json.Marshal(installed.Profile)
	c := Configuration{Namespace: "ai-sdlc-reviewers", WorkerImage: installed.WorkerImage, MailboxImage: installed.MailboxImage, Profile: profile, ADev: []byte(installed.ADev), Model: modelbroker.ProviderConfig{Endpoint: "http://127.0.0.1:8787/v1/chat/completions", BearerToken: string(token), AllowLoopbackHTTP: true}}
	if c.Validate() != nil || executor.ValidatePlacement(c.Namespace, c.WorkerImage, c.MailboxImage) != nil {
		return Configuration{}, ErrReview
	}
	return c, nil
}

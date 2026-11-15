package intake

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
)

// Config is reviewed installation policy, never part of an issue request.
type Config struct {
	Version            int     `json:"version"`
	Repository         string  `json:"repository"`
	BaseBranch         string  `json:"baseBranch"`
	TriggerLabel       string  `json:"triggerLabel"`
	AuthorizedActorIDs []int64 `json:"authorizedActorIDs"`
	PollSeconds        int     `json:"pollSeconds"`
	MaxPages           int     `json:"maxPages"`
}

func LoadConfig(path, repository string) (Config, error) {
	var config Config
	f, err := os.Open(path)
	if err != nil {
		return config, errors.New("intake configuration unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(raw) > 16384 || authority.DecodeStrict(raw, &config) != nil {
		return Config{}, errors.New("invalid intake configuration")
	}
	return config, config.Validate(repository)
}
func (c Config) Validate(repository string) error {
	if c.Version != 1 || c.Repository != repository || !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(c.BaseBranch) || len(c.BaseBranch) > 100 || !regexp.MustCompile(`^[a-z][a-z0-9:-]{0,99}$`).MatchString(c.TriggerLabel) || len(c.AuthorizedActorIDs) == 0 || len(c.AuthorizedActorIDs) > 20 || c.PollSeconds < 30 || c.PollSeconds > 3600 || c.MaxPages < 1 || c.MaxPages > 100 {
		return errors.New("invalid intake policy scope or bounds")
	}
	seen := map[int64]bool{}
	for _, id := range c.AuthorizedActorIDs {
		if id <= 0 || seen[id] {
			return errors.New("invalid intake actor allowlist")
		}
		seen[id] = true
	}
	return nil
}
func (c Config) Digest() string { raw, _ := json.Marshal(c); return authority.Digest(raw) }
func (c Config) AllowsActor(actor forge.Actor) bool {
	for _, id := range c.AuthorizedActorIDs {
		if actor.ID == id {
			return true
		}
	}
	return false
}

type Snapshot struct {
	Version         int         `json:"version"`
	Repository      string      `json:"repository"`
	Issue           forge.Issue `json:"issue"`
	Revision        string      `json:"revision"`
	Generation      int64       `json:"generation"`
	ConfigDigest    string      `json:"configDigest"`
	BaseSHA         string      `json:"baseSHA"`
	AuthorizedActor forge.Actor `json:"authorizedActor"`
	LabelEventID    int64       `json:"labelEventID"`
	Eligible        bool        `json:"eligible"`
}

func (s Snapshot) ContentRevision() string {
	labels := make([]string, 0, len(s.Issue.Labels))
	for _, label := range s.Issue.Labels {
		labels = append(labels, label.Name)
	}
	sort.Strings(labels)
	// Comments and display-name changes are not new requests. Bind the actual
	// issue content/state plus the authoritative authorization event instead.
	raw, _ := json.Marshal(struct {
		ID                 int64
		Number             int
		Title, Body, State string
		Author             int64
		Labels             []string
		Actor, Event       int64
	}{s.Issue.ID, s.Issue.Number, s.Issue.Title, s.Issue.Body, s.Issue.State, s.Issue.User.ID, labels, s.AuthorizedActor.ID, s.LabelEventID})
	return authority.Digest(raw)
}
func (s Snapshot) ID() string {
	return "issue-" + authority.Digest([]byte(s.Repository+"\x00"+s.Revision+"\x00"+s.ConfigDigest+"\x00"+strconv.FormatInt(s.Generation, 10)))
}
func (s Snapshot) Validate(c Config) error {
	if s.Generation < 1 {
		return errors.New("issue generation required")
	}
	return s.validateContent(c)
}
func (s Snapshot) validateContent(c Config) error {
	if s.Version != 1 || s.Repository != c.Repository || s.ConfigDigest != c.Digest() || s.Issue.Validate() != nil || s.Issue.IsPR() || s.Revision != s.ContentRevision() {
		return errors.New("issue snapshot binding mismatch")
	}
	if s.Eligible && (s.Issue.State != "open" || !s.Issue.HasLabel(c.TriggerLabel) || !c.AllowsActor(s.AuthorizedActor) || s.LabelEventID <= 0 || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(s.BaseSHA)) {
		return errors.New("issue snapshot admission denied")
	}
	return nil
}

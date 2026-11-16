package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

// The fixture seeds synthetic historical merge evidence in disposable PostgreSQL
// and then executes actual Kubernetes workloads through the production controller.
// It never writes a live runtime journal or claims an autonomous issue E2E.
func TestRealJournalBackedClosedBuild(t *testing.T) {
	path := os.Getenv("AI_SDLC_JOURNAL_BUILD_FIXTURE")
	if path == "" {
		t.Skip("explicit isolated journal/build fixture required")
	}
	var input struct {
		Config                                                     podtransport.Config
		Slot                                                       building.Slot
		Phase, OutputDirectory, PublisherImage, SourceSHA, TreeSHA string
	}
	raw, e := os.ReadFile(path)
	if e != nil || len(raw) > 256*1024 || authority.DecodeStrict(raw, &input) != nil || input.Slot.Validate() != nil {
		t.Fatal("private fixture input")
	}
	var profile buildworkload.Profile
	raw, e = os.ReadFile(filepath.Join(input.OutputDirectory, "profile.json"))
	if e != nil || authority.DecodeStrict(raw, &profile) != nil || profile.Validate() != nil {
		t.Fatal("actual installed profile")
	}
	b := newBuildFixture(t, buildFixtureSource{input.SourceSHA, input.TreeSHA})
	b.cfg.ProfileSHA256 = profile.Digest()
	b.cfg.Slots = []building.Slot{input.Slot}
	b.cfg.IssueMaxAgeSeconds = 300
	b.command.ProfileSHA256 = b.cfg.ProfileDigest()
	b.command.IssuedAt = time.Now().UnixMilli()
	b.command.ExpiresAt = time.Now().Add(19 * time.Minute).UnixMilli()
	b.raw, _ = json.Marshal(b.command)
	signed, e := b.f.Material.Identities["release-manager"].Seal(b.cfg.Outer, b.command.ID(), "build-merged", b.raw)
	if e != nil {
		t.Fatal("fixture authority")
	}
	b.receipt, _ = json.Marshal(signed)
	compiled, e := buildworkload.Compile(*b.cfg, profile, signed, b.raw, input.Slot, time.Now())
	if e != nil {
		t.Fatal("closed compilation")
	}
	write := func(name string, value any) {
		body, _ := json.MarshalIndent(value, "", "  ")
		if os.WriteFile(filepath.Join(input.OutputDirectory, name), append(body, '\n'), 0600) != nil {
			t.Fatal("report write")
		}
	}
	for name, body := range map[string][]byte{"build.json": compiled.Build, "publish.json": compiled.Publish} {
		if os.WriteFile(filepath.Join(input.OutputDirectory, name), body, 0600) != nil {
			t.Fatal("compiled report")
		}
	}
	write("fixture-command.json", b.command)
	write("fixture-slot.json", input.Slot)
	report := map[string]any{"status": "running", "scope": "isolated journal and actual workload fixture", "syntheticAuthority": true, "autonomousE2E": false, "liveWorkflowAdvanced": false, "releaseAuthorized": false, "plan": compiled.Plan, "buildName": "ai-sdlc-build-" + b.command.ID()[6:38], "publishName": "ai-sdlc-publish-" + b.command.ID()[6:38]}
	write("transport-report.json", report)
	client, e := podtransport.NewClient(input.Config)
	if e != nil {
		t.Fatal("private explicit API client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	if e = b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal("fresh synthetic request queue", e)
	}
	selected, e := b.journal.NextBuild(ctx)
	if e != nil || selected != b.command.ID() {
		t.Fatal("current queued selection", e)
	}
	result, e := podtransport.ExecuteQueuedBuild(ctx, client, b.journal, selected, *b.cfg, profile)
	if e != nil {
		report["status"] = "failed"
		write("transport-report.json", report)
		t.Fatal("journal-backed actual execution", e)
	}
	record, e := b.journal.ReadOwnBuiltRecord(ctx, selected)
	if e != nil || record.Result != result || result.ReleaseAuthorized {
		t.Fatal("durable built record", e)
	}
	if _, e = b.journal.ReadQueuedBuild(ctx, selected); e == nil {
		t.Fatal("completed request resumed")
	}
	if _, e = b.journal.UnusedBuildSlot(ctx); e == nil {
		t.Fatal("consumed physical slot reused")
	}
	report["status"] = "passed"
	report["durableBuiltRecordValidated"] = true
	report["buildRunUID"] = result.BuildRunUID
	report["publishRunUID"] = result.PublishRunUID
	report["buildTermination"] = record.Build
	report["publishTermination"] = record.Publish
	report["results"] = map[string]string{"IMAGE_URL": result.ImageURL, "IMAGE_DIGEST": result.ImageDigest, "CHAINS-GIT_URL": "https://github.com/os-santiago/saferus", "CHAINS-GIT_COMMIT": result.SourceSHA, "DEFINITION_DIGEST": "sha256:" + result.DefinitionSHA256, "RUN_UID": result.PublishRunUID}
	write("transport-report.json", report)
}

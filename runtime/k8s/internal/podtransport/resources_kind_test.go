package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRealWorkloadResourcesFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_RESOURCE_FIXTURE")
	if path == "" {
		t.Skip("isolated kind workload resource fixture required")
	}
	raw, err := os.ReadFile(path)
	var input struct {
		Config                                       Config
		Binding                                      Binding
		SourceFile, OutputFile, ReportFile, Scenario string
	}
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &input) != nil {
		t.Fatal("private resource fixture input")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	client, err := NewClient(input.Config)
	if err != nil {
		t.Fatal("fixture client")
	}
	resources, err := PlanResources(client, input.Binding)
	if err != nil {
		t.Fatal("fixed resource compilation")
	}
	if input.Scenario == "create-denied" {
		if _, err = resources.CreateConfig(ctx); !errors.Is(err, ErrPermission) {
			t.Fatal("actual create permission not denied")
		}
		return
	}
	if input.Scenario != "normal" {
		t.Fatal("fixture scenario")
	}
	configUID, podUID, replacementUID := "", "", ""
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		if replacementUID != "" {
			_ = resources.DeletePod(cleanup, replacementUID)
		} else if podUID != "" {
			_ = resources.DeletePod(cleanup, podUID)
		}
		if configUID != "" {
			_ = resources.DeleteConfig(cleanup, configUID)
		}
	}()
	configUID, err = resources.CreateConfig(ctx)
	if err != nil {
		t.Fatal("actual fixed ConfigMap create")
	}
	// Conflict must not adopt an existing immutable resource.
	if duplicate, e := resources.CreateConfig(ctx); e == nil || duplicate != "" {
		t.Fatal("existing config adopted")
	}
	podUID, err = resources.CreatePod(ctx, configUID)
	if err != nil {
		t.Fatal("actual fixed Pod create")
	}
	if duplicate, e := resources.CreatePod(ctx, configUID); e == nil || duplicate != "" {
		t.Fatal("existing Pod adopted")
	}
	session, err := resources.WaitRunning(ctx, podUID)
	if err != nil {
		t.Fatal("actual same-UID readiness")
	}
	binding := input.Binding
	binding.PodUID = podUID
	candidate := exerciseWorkspaceLifecycle(t, ctx, session, binding, input.SourceFile, input.OutputFile)
	if err = resources.DeletePod(ctx, podUID); err != nil {
		t.Fatal("actual original Pod cleanup")
	}
	replacementUID, err = resources.CreatePod(ctx, configUID)
	if err != nil || replacementUID == podUID {
		t.Fatal("actual replacement setup")
	}
	if err = resources.DeletePod(ctx, podUID); err == nil {
		t.Fatal("old UID deleted replacement")
	}
	if _, err = resources.WaitRunning(ctx, replacementUID); err != nil {
		t.Fatal("replacement was not preserved")
	}
	originalUID := podUID
	podUID = ""
	if err = resources.DeletePod(ctx, replacementUID); err != nil {
		t.Fatal("replacement cleanup")
	}
	replacementUID = ""
	originalConfigUID := configUID
	if err = resources.DeleteConfig(ctx, configUID); err != nil {
		t.Fatal("immutable config cleanup")
	}
	configUID = ""
	report := map[string]any{"scope": "isolated-component-fixture", "syntheticModelReplies": true, "liveWorkflowAdvanced": false,
		"fixedResourcesCreated": true, "createConflictRejected": true, "oldUIDDeleteRejected": true, "replacementPreserved": true,
		"confirmedPodUID": originalUID, "confirmedConfigUID": originalConfigUID, "workerExitCode": candidate.WorkerExitCode(),
		"candidateBytes": len(candidate.Bytes()), "modelCalls": 2, "durableModelHandoffs": 0, "resourcesConfirmedRemoved": true}
	body, _ := json.Marshal(report)
	file, err := os.OpenFile(input.ReportFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("private report output")
	}
	_, err = file.Write(body)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal("report write")
	}
}

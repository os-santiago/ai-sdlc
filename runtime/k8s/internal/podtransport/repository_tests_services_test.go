package podtransport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func fixtureServices() []project.Service {
	return []project.Service{{Name: "villains-db", Image: "docker.io/library/postgres@sha256:" + strings.Repeat("5", 64),
		Env:  map[string]string{"POSTGRES_PASSWORD": "superbad", "PGDATA": "/var/lib/postgresql/data/pgdata"},
		User: 999, Port: 5432, ReadySeconds: 60, WritablePaths: []string{"/var/lib/postgresql/data", "/var/run/postgresql"},
		CPU: "500m", Memory: "512Mi", EphemeralStorage: "512Mi"}}
}

// The compiled pod decodes strictly into a Kubernetes Pod: every service
// field is a real API field, the sandbox and token settings hold for every
// container, and no service can reach the workspace or the runner.
func TestServicesPodDecodesStrictlyAndIsIsolated(t *testing.T) {
	p, source := testsFixturePod()
	p.Verification.Services = fixtureServices()
	c := &testsCluster{state: "running", reply: honestReply(p, source), services: p.Verification.Services}
	run := testsServer(t, c)
	pod := run.resources.pod
	s := pod.Spec
	if s.RuntimeClassName == nil || *s.RuntimeClassName != "ai-sdlc-offline" || s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken ||
		s.EnableServiceLinks == nil || *s.EnableServiceLinks || s.HostNetwork || s.ServiceAccountName != "" ||
		s.SecurityContext.SeccompProfile == nil || s.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Fatal("pod isolation", s)
	}
	if len(s.InitContainers) != 3 || s.InitContainers[2].Name != "service-villains-db" {
		t.Fatal("service sidecar", s.InitContainers)
	}
	db := s.InitContainers[2]
	sc := db.SecurityContext
	if db.RestartPolicy == nil || *db.RestartPolicy != core.ContainerRestartPolicyAlways || sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.RunAsUser == nil || *sc.RunAsUser != 999 || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.Privileged != nil ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
		t.Fatal("service security context", sc)
	}
	if db.Resources.Requests.Cpu().String() != "500m" || !db.Resources.Requests.Memory().Equal(*db.Resources.Limits.Memory()) ||
		db.Resources.Limits.StorageEphemeral().String() != "512Mi" {
		t.Fatal("service resources", db.Resources)
	}
	for _, m := range db.VolumeMounts {
		if m.Name != "service-villains-db" || m.SubPath == "" {
			t.Fatal("a service mounts only its own volume", m)
		}
	}
	for _, v := range s.Volumes {
		if v.Name == "service-villains-db" && (v.EmptyDir == nil || v.EmptyDir.SizeLimit.String() != "512Mi") {
			t.Fatal("service volume", v)
		}
		if v.Secret != nil || v.Projected != nil || v.HostPath != nil {
			t.Fatal("unexpected volume source", v)
		}
	}
	raw, _ := json.Marshal(pod)
	if strings.Contains(string(raw), "serviceAccountToken") {
		t.Fatal("a token projection reached the pod")
	}
}

func TestServicesRunImportsAndReadsResultsWhileServicesRun(t *testing.T) {
	p, source := testsFixturePod()
	c := &testsCluster{state: "initializing", reply: honestReply(p, source), log: strings.Repeat(`{"exitCode":0}`+"\n", 2), services: fixtureServices()}
	run := testsServer(t, c)
	// The result wait covers the service readiness, both repetitions and the margin.
	if run.services != 60*time.Second || run.resultWindow() != 60*time.Second+2*30*time.Second+repositorytests.MarginSeconds*time.Second {
		t.Fatal("the result deadline must add the service readiness", run.services, run.resultWindow())
	}
	without := testsServer(t, &testsCluster{state: "running", reply: honestReply(p, source)})
	if without.resultWindow() != run.resultWindow()-60*time.Second {
		t.Fatal("services must only add their readiness to the result deadline", without.resultWindow())
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.mu.Lock()
		c.state = "running"
		c.mu.Unlock()
	}()
	if err := run.Prepare(context.Background(), testsPodUID); err != nil {
		t.Fatal("a running service failed Prepare", err)
	}
	c.mu.Lock()
	c.state = "stopping"
	c.mu.Unlock()
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.mu.Lock()
		c.state = "succeeded"
		c.mu.Unlock()
	}()
	if _, _, err := run.ReadResult(context.Background(), testsPodUID); err != nil {
		t.Fatal("services stopped after the tests failed the run", err)
	}
}

func TestServicesThatRestartCrashOrAreUnknownFailTheRun(t *testing.T) {
	for name, rewrite := range map[string]func(core.ContainerStatus) core.ContainerStatus{
		"restarted": func(s core.ContainerStatus) core.ContainerStatus { s.RestartCount = 1; return s },
		"crashed": func(s core.ContainerStatus) core.ContainerStatus {
			s.State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1, ContainerID: s.ContainerID}}
			return s
		},
		"renamed": func(s core.ContainerStatus) core.ContainerStatus { s.Name = "service-other"; return s },
		"other image": func(s core.ContainerStatus) core.ContainerStatus {
			s.ImageID = "docker.io/library/postgres@sha256:" + strings.Repeat("6", 64)
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, source := testsFixturePod()
			c := &testsCluster{state: "running", reply: honestReply(p, source), services: fixtureServices(), service: rewrite}
			run := testsServer(t, c)
			if _, err := run.Status(context.Background(), testsPodUID); err == nil {
				t.Fatal("accepted a service that", name)
			}
		})
	}
}

// A service that dies before the tests end could have made them fail; in the
// cross run that would read as new tests failing on the old code. Only a
// kubelet stop after the tests finished is accepted, in every run.
func TestServicesThatDieBeforeTheTestsEndFailTheRun(t *testing.T) {
	stop := func(exit int32, reason string, offset time.Duration) func(core.ContainerStatus) core.ContainerStatus {
		return func(s core.ContainerStatus) core.ContainerStatus {
			if s.State.Terminated != nil {
				s.State.Terminated.ExitCode, s.State.Terminated.Reason = exit, reason
				s.State.Terminated.FinishedAt = meta.NewTime(testsFinishedAt.Add(offset))
			}
			return s
		}
	}
	cases := map[string]struct {
		rewrite func(core.ContainerStatus) core.ContainerStatus
		accept  bool
	}{
		"kubelet stop, clean exit": {stop(0, "Completed", time.Second), true},
		"kubelet stop, SIGTERM":    {stop(143, "Error", time.Second), true},
		"kubelet stop, SIGKILL":    {stop(137, "Error", 0), true},
		"died before the tests":    {stop(137, "Error", -time.Second), false},
		"clean exit before":        {stop(0, "Completed", -2*time.Second), false},
		"OOM-killed after":         {stop(137, "OOMKilled", time.Second), false},
		"crashed after":            {stop(1, "Error", time.Second), false},
		"no finish time": {func(s core.ContainerStatus) core.ContainerStatus {
			if s.State.Terminated != nil {
				s.State.Terminated.FinishedAt = meta.Time{}
			}
			return s
		}, false},
	}
	for _, runName := range []string{repositorytests.RunCross, repositorytests.RunCandidate} {
		for name, tc := range cases {
			t.Run(runName+"/"+name, func(t *testing.T) {
				p, source := testsFixturePod()
				p.Run = runName
				lines := repositorytests.Repetitions(runName, p.Verification)
				c := &testsCluster{state: "running", reply: honestReply(p, source), log: strings.Repeat(`{"exitCode":1}`+"\n", lines),
					services: fixtureServices(), service: tc.rewrite, run: runName}
				run := testsServer(t, c)
				if err := run.Prepare(context.Background(), testsPodUID); err != nil {
					t.Fatal(err)
				}
				c.mu.Lock()
				c.state = "succeeded"
				c.mu.Unlock()
				_, _, err := run.ReadResult(context.Background(), testsPodUID)
				if (err == nil) != tc.accept {
					t.Fatal("service termination decision", err)
				}
				if !tc.accept && TestsStep(err) != "observe-service" {
					t.Fatal("the stop must name the service", TestsStep(err))
				}
			})
		}
	}
}

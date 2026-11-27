package repositorytests

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func postgres() project.Service {
	return project.Service{Name: "villains-db", Image: "docker.io/library/postgres@sha256:" + strings.Repeat("f", 64),
		Env:  map[string]string{"POSTGRES_USER": "superbad", "POSTGRES_PASSWORD": "superbad", "PGDATA": "/var/lib/postgresql/data/pgdata"},
		User: 999, Port: 5432, ReadySeconds: 90, WritablePaths: []string{"/var/lib/postgresql/data", "/var/run/postgresql"},
		CPU: "500m", Memory: "512Mi", EphemeralStorage: "512Mi"}
}

func servicesPod() Pod {
	p := testPod()
	broker := project.Service{Name: "kafka", Image: "docker.io/apache/kafka@sha256:" + strings.Repeat("1", 64), Args: []string{"/etc/kafka/docker/run"},
		User: 1000, Port: 9092, ReadySeconds: 120, WritableRootFilesystem: true, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi"}
	p.Verification.Services = []project.Service{postgres(), broker}
	return p
}

func TestServicesAreHardenedNativeSidecarsBeforeTheTests(t *testing.T) {
	w, err := CompilePod(servicesPod())
	if err != nil {
		t.Fatal(err)
	}
	spec := w.Pod["spec"].(map[string]any)
	// The pod-level isolation every sidecar inherits.
	for key, want := range map[string]any{"runtimeClassName": "ai-sdlc-offline", "automountServiceAccountToken": false, "enableServiceLinks": false,
		"hostNetwork": false, "hostPID": false, "hostIPC": false, "shareProcessNamespace": false, "restartPolicy": "Never"} {
		if spec[key] != want {
			t.Fatal(key, spec[key])
		}
	}
	podSecurity := spec["securityContext"].(map[string]any)
	if podSecurity["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" || podSecurity["runAsNonRoot"] != true {
		t.Fatal("pod security context", podSecurity)
	}
	if _, ok := spec["serviceAccountName"]; ok {
		t.Fatal("a service account was named")
	}
	inits := spec["initContainers"].([]any)
	if len(inits) != 4 || len(spec["containers"].([]any)) != 1 {
		t.Fatal("containers", inits)
	}
	names := []string{}
	for _, c := range inits {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "install-runner,transfer,service-villains-db,service-kafka" {
		t.Fatal("services must start, in manifest order, after the transfer sidecar", names)
	}
	db, kafka := inits[2].(map[string]any), inits[3].(map[string]any)
	for _, c := range []map[string]any{db, kafka} {
		if c["restartPolicy"] != "Always" || c["imagePullPolicy"] != "IfNotPresent" {
			t.Fatal("a service must be a native sidecar", c)
		}
		sc := c["securityContext"].(map[string]any)
		if sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false ||
			strings.Join(toStrings(sc["capabilities"].(map[string]any)["drop"]), ",") != "ALL" || sc["privileged"] != nil {
			t.Fatal("service security context", sc)
		}
		if _, ok := c["command"]; ok {
			t.Fatal("a service's entrypoint must be the image's", c)
		}
		for _, m := range mounts(c) {
			if m["name"] != c["name"] || m["readOnly"] != nil {
				t.Fatal("a service may mount only its own volume", m)
			}
		}
		requests, limits := c["resources"].(map[string]any)["requests"], c["resources"].(map[string]any)["limits"]
		raw1, _ := json.Marshal(requests)
		raw2, _ := json.Marshal(limits)
		if string(raw1) != string(raw2) {
			t.Fatal("service requests must equal its limits", requests, limits)
		}
	}
	dbSecurity := db["securityContext"].(map[string]any)
	if dbSecurity["runAsUser"] != float64(999) || dbSecurity["runAsGroup"] != float64(999) || dbSecurity["readOnlyRootFilesystem"] != true {
		t.Fatal("database identity and read-only root", dbSecurity)
	}
	if kafka["securityContext"].(map[string]any)["readOnlyRootFilesystem"] != false || kafka["args"].([]any)[0] != "/etc/kafka/docker/run" {
		t.Fatal("reviewed writable root and arguments", kafka)
	}
	if _, ok := kafka["volumeMounts"]; ok {
		t.Fatal("a service without writable paths has no volume")
	}
	env, _ := json.Marshal(db["env"])
	if string(env) != `[{"name":"PGDATA","value":"/var/lib/postgresql/data/pgdata"},{"name":"POSTGRES_PASSWORD","value":"superbad"},{"name":"POSTGRES_USER","value":"superbad"}]` {
		t.Fatal("service env must be literal and sorted", string(env))
	}
	dbMounts := mounts(db)
	if len(dbMounts) != 2 || dbMounts[0]["mountPath"] != "/var/lib/postgresql/data" || dbMounts[0]["subPath"] != "0" ||
		dbMounts[1]["mountPath"] != "/var/run/postgresql" || dbMounts[1]["subPath"] != "1" {
		t.Fatal("writable paths", dbMounts)
	}
	volumes := spec["volumes"].([]any)
	last := volumes[len(volumes)-1].(map[string]any)
	if len(volumes) != 6 || last["name"] != "service-villains-db" || last["emptyDir"].(map[string]any)["sizeLimit"] != "512Mi" {
		t.Fatal("each service with writable paths gets one fresh, bounded emptyDir", volumes)
	}
	// The tests never see the services' environment, and the services never
	// see the workspace, runner, settings or pod identity.
	tests := spec["containers"].([]any)[0].(map[string]any)
	if _, ok := tests["env"]; ok {
		t.Fatal("service env reached the tests container")
	}
	if spec["activeDeadlineSeconds"] != float64(PullSeconds+ReadySeconds+120+300+MarginSeconds) {
		t.Fatal("the deadline must add the longest service readiness", spec["activeDeadlineSeconds"])
	}
	settings := w.ConfigMap["data"].(map[string]any)["verification.json"].(string)
	if !strings.Contains(settings, `"services":[{"name":"villains-db"`) {
		t.Fatal("the runner reads the services from the settings", settings)
	}
}

func TestEveryRunGetsItsOwnFreshServices(t *testing.T) {
	p := servicesPod()
	p.Verification.Repeat = 3
	for _, run := range RunNames() {
		p.Run = run
		w, err := CompilePod(p)
		if err != nil {
			t.Fatal(err)
		}
		spec := w.Pod["spec"].(map[string]any)
		deadline := PullSeconds + ReadySeconds + 120 + Repetitions(run, p.Verification)*300 + MarginSeconds
		if spec["activeDeadlineSeconds"] != float64(deadline) {
			t.Fatal(run, spec["activeDeadlineSeconds"])
		}
		for _, v := range spec["volumes"].([]any) {
			volume := v.(map[string]any)
			if strings.HasPrefix(volume["name"].(string), ServiceContainerPrefix) && volume["emptyDir"] == nil {
				t.Fatal("service state must be a per-pod emptyDir", volume)
			}
		}
	}
}

func TestCompilePodRefusesInvalidServices(t *testing.T) {
	for name, change := range map[string]func(*project.Service){
		"root":    func(s *project.Service) { s.User = 0 },
		"mutable": func(s *project.Service) { s.Image = "postgres:17" },
		"expand":  func(s *project.Service) { s.Env = map[string]string{"POSTGRES_PASSWORD": "$(POD_UID)"} },
		"port":    func(s *project.Service) { s.Port = 443 },
	} {
		p := testPod()
		s := postgres()
		change(&s)
		p.Verification.Services = []project.Service{s}
		if _, err := CompilePod(p); err == nil {
			t.Fatal("accepted", name)
		}
	}
}

func TestWaitServicesWaitsForEveryLoopbackPort(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := WaitServices(context.Background(), []project.Service{{Port: port, ReadySeconds: 2}}); err != nil {
		t.Fatal("a listening service was not ready", err)
	}
	// A port that only starts listening later is waited for.
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	late := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	go func() {
		time.Sleep(300 * time.Millisecond)
		if l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", itoa(late))); err == nil {
			time.Sleep(2 * time.Second)
			l.Close()
		}
	}()
	if err := WaitServices(context.Background(), []project.Service{{Port: port, ReadySeconds: 2}, {Port: late, ReadySeconds: 2}}); err != nil {
		t.Fatal("a late service was not waited for", err)
	}
	never, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	silent := never.Addr().(*net.TCPAddr).Port
	never.Close()
	start := time.Now()
	if err := WaitServices(context.Background(), []project.Service{{Port: silent, ReadySeconds: 1}}); err != ErrServices || time.Since(start) > 3*time.Second {
		t.Fatal("a service that never listens must fail within its bound", err, time.Since(start))
	}
	if WaitServices(context.Background(), nil) != nil {
		t.Fatal("no services, nothing to wait for")
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func toStrings(v any) []string {
	out := []string{}
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

func mounts(c map[string]any) []map[string]any {
	out := []map[string]any{}
	list, _ := c["volumeMounts"].([]any)
	for _, m := range list {
		out = append(out, m.(map[string]any))
	}
	return out
}

// Each service has its own bound from the same start: a service that is
// slow within a long bound must not use up the short bound of a service that
// listens from the start.
func TestWaitServicesChecksEveryServiceInParallel(t *testing.T) {
	ready, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	reserved, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	slow := reserved.Addr().(*net.TCPAddr).Port
	reserved.Close()
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", itoa(slow))); err == nil {
			time.Sleep(3 * time.Second)
			l.Close()
		}
	}()
	// The slow service comes first and listens after 1.5 s, past the fast
	// service's 1 s bound; checked in order, the fast one would expire.
	services := []project.Service{{Port: slow, ReadySeconds: 4}, {Port: ready.Addr().(*net.TCPAddr).Port, ReadySeconds: 1}}
	start := time.Now()
	if err := WaitServices(context.Background(), services); err != nil {
		t.Fatal("a ready service failed because an earlier one was slow", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second || elapsed > 4*time.Second {
		t.Fatal("the wait must last until the slowest service is ready", elapsed)
	}
}

// The manifest's budget check mirrors this package's run deadline.
func TestManifestBudgetMatchesTheRunDeadline(t *testing.T) {
	if PullSeconds+ReadySeconds+MarginSeconds != project.RunOverheadSeconds {
		t.Fatal("project.RunOverheadSeconds is out of date")
	}
	p := servicesPod()
	p.Verification.Repeat = 3
	p.Verification.Analysis = &project.Analysis{Commands: [][]string{{"go", "vet", "./..."}}, TimeoutSeconds: 30}
	if DeadlineSeconds(RunCandidate, p.Verification) != p.Verification.LongestRunSeconds() {
		t.Fatal("the longest run must be the candidate's deadline", DeadlineSeconds(RunCandidate, p.Verification), p.Verification.LongestRunSeconds())
	}
	for _, run := range RunNames() {
		if DeadlineSeconds(run, p.Verification) > p.Verification.LongestRunSeconds() {
			t.Fatal(run, "exceeds the longest run")
		}
	}
}

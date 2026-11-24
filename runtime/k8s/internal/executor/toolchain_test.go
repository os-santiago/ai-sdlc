package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"k8s.io/apimachinery/pkg/api/resource"
)

const toolchainImage = "docker.io/library/golang@sha256:ea30e25680a5470bac02b08bb0e0a735fb75bd0ab7d431b9b95ee0abef362586"

func toolchainFixture() *Toolchain {
	return &Toolchain{Image: toolchainImage, Root: "/usr/local/go", Path: []string{"bin"}, Command: []string{"go", "test", "./..."},
		Env: map[string]string{"GOTOOLCHAIN": "local", "GOFLAGS": "-mod=mod"}}
}

type toolchainCompiled struct {
	assignment, profile, adev []byte
	workload                  Workload
}

func compileFixture(t *testing.T, now time.Time, edit func(*Assignment, *Profile)) (toolchainCompiled, error) {
	t.Helper()
	adev := []byte("reviewed profile fixture")
	p := profileFixture()
	a := assignmentFixture(now)
	if edit != nil {
		edit(&a, &p)
	}
	p.ADevSHA256 = workspacebundle.Digest(adev)
	profile, _ := json.Marshal(p)
	a.ProfileSHA256 = workspacebundle.Digest(profile)
	assignment, _ := json.Marshal(a)
	image := "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
	w, err := Compile("fixture", image, image, DefaultRuntimeClass, assignment, profile, adev, now)
	return toolchainCompiled{assignment, profile, adev, w}, err
}

// The digests were computed by the same fixtures on main before the toolchain
// existed: an assignment without a toolchain keeps its bytes and workload.
func TestWorkloadWithoutToolchainIsByteIdenticalToMain(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	golden := map[string][2]string{
		"generation": {"e71327394b141c496120a76836a4f3de9c4f7c412538eece1414b7e8e9c5c510", "3ea74d2d20f7bda435576bbbe5cbb6228c4a8152f2357f580cbc5403cecdcf16"},
		"offline":    {"7f85f3aaf7a746265242c132c7273362f1a2b64fb94cd585835d8c0cffbe2f2f", "58e34b29dde54cb732a478e2e8f12b3495e78da5a9723a16c995054b5b3e8f73"},
		"review":     {"4600618a9c1908e1d235d35155c03ec69000ffc06079f68a316b17987fae04f5", "e25c6bca55c76491eaddd086a6141c06ecdc38dec7a96b4e6cd2900503a7393d"},
	}
	for name, want := range golden {
		c, err := compileFixture(t, now, func(a *Assignment, p *Profile) {
			if name == "offline" {
				a.Workspace = WorkspaceOfflineTests
			}
			if name == "review" {
				p.Version = ReviewProfileVersion
			}
		})
		if err != nil {
			t.Fatal(name, err)
		}
		raw, _ := json.Marshal(c.workload)
		if workspacebundle.Digest(c.assignment) != want[0] || workspacebundle.Digest(raw) != want[1] {
			t.Fatal("workload without a toolchain changed", name)
		}
		if strings.Contains(string(c.assignment), "toolchain") || strings.Contains(string(raw), "initContainers") {
			t.Fatal("toolchain fields without a toolchain", name)
		}
	}
}

func TestToolchainWorkloadCopiesTheRootBeforeTheWorker(t *testing.T) {
	now := time.Now()
	c, err := compileFixture(t, now, func(a *Assignment, _ *Profile) {
		a.Workspace = WorkspaceOfflineTests
		a.Toolchain = toolchainFixture()
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err := compileFixture(t, now, func(a *Assignment, _ *Profile) { a.Workspace = WorkspaceOfflineTests })
	if err != nil {
		t.Fatal(err)
	}
	spec := c.workload.Pod["spec"].(map[string]any)
	baseSpec := base.workload.Pod["spec"].(map[string]any)
	if spec["activeDeadlineSeconds"] != baseSpec["activeDeadlineSeconds"].(float64)+ToolchainStartSeconds {
		t.Fatal("deadline does not cover the toolchain start", spec["activeDeadlineSeconds"])
	}
	inits := spec["initContainers"].([]any)
	if len(inits) != 2 {
		t.Fatal("init sequence", inits)
	}
	install, copier := inits[0].(map[string]any), inits[1].(map[string]any)
	if install["name"] != InitInstallRunner || install["image"] != "quay.io/os-santiago/ai-sdlc@sha256:"+strings.Repeat("a", 64) ||
		!reflect.DeepEqual(install["command"], []any{"/repository-tests", "install", "/runner/repository-tests"}) {
		t.Fatal("install container", install)
	}
	if copier["name"] != InitCopyToolchain || copier["image"] != toolchainImage ||
		!reflect.DeepEqual(copier["command"], []any{"/runner/repository-tests", "copy-toolchain", "/usr/local/go", "/workspaces/root"}) {
		t.Fatal("copy container", copier)
	}
	for _, container := range inits {
		value := container.(map[string]any)
		if !reflect.DeepEqual(value["securityContext"], baseSpec["containers"].([]any)[0].(map[string]any)["securityContext"]) || value["restartPolicy"] != nil || value["env"] != nil {
			t.Fatal("init container isolation", value["name"])
		}
		limits := value["resources"].(map[string]any)["limits"].(map[string]any)
		if limits["ephemeral-storage"] == nil || limits["memory"] == nil || limits["cpu"] == nil {
			t.Fatal("unbounded init container", value["name"])
		}
	}
	if !reflect.DeepEqual(copier["volumeMounts"], []any{map[string]any{"name": "runner", "mountPath": "/runner", "readOnly": true}, map[string]any{"name": "toolchain", "mountPath": "/workspaces"}}) {
		t.Fatal("copy mounts", copier["volumeMounts"])
	}
	worker := spec["containers"].([]any)[0].(map[string]any)
	mounts := worker["volumeMounts"].([]any)
	if !reflect.DeepEqual(mounts[len(mounts)-2:], []any{map[string]any{"name": "toolchain", "mountPath": "/toolchain", "readOnly": true}, map[string]any{"name": "scratch", "mountPath": "/scratch"}}) {
		t.Fatal("worker mounts", mounts)
	}
	limits := worker["resources"].(map[string]any)["limits"].(map[string]any)
	if limits["memory"] != "1280Mi" || limits["ephemeral-storage"] != "2304Mi" || limits["cpu"] != "1" {
		t.Fatal("worker limits", limits)
	}
	volumes := spec["volumes"].([]any)
	want := []any{
		map[string]any{"name": "toolchain", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
		map[string]any{"name": "scratch", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
		map[string]any{"name": "runner", "emptyDir": map[string]any{"sizeLimit": "128Mi"}},
	}
	if !reflect.DeepEqual(volumes[len(volumes)-3:], want) {
		t.Fatal("toolchain volumes", volumes)
	}
	// Only the toolchain additions differ from the workload without it.
	delete(spec, "initContainers")
	spec["volumes"] = volumes[:len(volumes)-3]
	spec["activeDeadlineSeconds"] = baseSpec["activeDeadlineSeconds"]
	worker["volumeMounts"] = mounts[:len(mounts)-2]
	worker["resources"] = baseSpec["containers"].([]any)[0].(map[string]any)["resources"]
	// The assignment digest names the ConfigMap and annotates the pod.
	c.workload.Pod["metadata"] = base.workload.Pod["metadata"]
	worker["command"] = baseSpec["containers"].([]any)[0].(map[string]any)["command"]
	spec["volumes"].([]any)[2] = baseSpec["volumes"].([]any)[2]
	got, _ := json.Marshal(c.workload.Pod)
	expected, _ := json.Marshal(base.workload.Pod)
	if string(got) != string(expected) {
		t.Fatal("the toolchain changed other pod fields", string(got), string(expected))
	}
	var scc map[string]any
	_ = json.Unmarshal([]byte(c.workload.ConfigMap["data"].(map[string]any)["scc-config.json"].(string)), &scc)
	if !reflect.DeepEqual(scc["run_shell"], map[string]any{"allowedEnvVars": []any{"PYTHONDONTWRITEBYTECODE", "GOFLAGS", "GOTOOLCHAIN"}}) {
		t.Fatal("toolchain variables not allowlisted", scc["run_shell"])
	}
	// Without a toolchain the pod and SCC configuration keep their original form.
	if string(SCCConfiguration(profileFixture(), nil)) != base.workload.ConfigMap["data"].(map[string]any)["scc-config.json"] {
		t.Fatal("SCC configuration without a toolchain changed")
	}
}

func TestToolchainAssignmentIsBoundedAndReviewedOnly(t *testing.T) {
	now := time.Now()
	parse := func(edit func(*Toolchain), workspace string) error {
		a := assignmentFixture(now)
		a.Workspace = workspace
		a.Toolchain = toolchainFixture()
		if edit != nil {
			edit(a.Toolchain)
		}
		raw, _ := json.Marshal(a)
		_, err := ParseAssignment(raw, workspacebundle.Digest(raw), now)
		return err
	}
	if err := parse(nil, WorkspaceOfflineTests); err != nil {
		t.Fatal(err)
	}
	if err := parse(func(tc *Toolchain) { tc.Env = nil; tc.WorkingDir = "service" }, WorkspaceOfflineTests); err != nil {
		t.Fatal(err)
	}
	if parse(nil, "") == nil {
		t.Fatal("toolchain without the offline-tests workspace")
	}
	for name, edit := range map[string]func(*Toolchain){
		"tag image":        func(tc *Toolchain) { tc.Image = "docker.io/library/golang:1.27" },
		"no root":          func(tc *Toolchain) { tc.Root = "" },
		"relative root":    func(tc *Toolchain) { tc.Root = "usr/local/go" },
		"excluded root":    func(tc *Toolchain) { tc.Root = "/var/run/secrets" },
		"workspace root":   func(tc *Toolchain) { tc.Root = "/workspaces/go" },
		"no path":          func(tc *Toolchain) { tc.Path = nil },
		"escaping path":    func(tc *Toolchain) { tc.Path = []string{"../bin"} },
		"absolute path":    func(tc *Toolchain) { tc.Path = []string{"/bin"} },
		"no command":       func(tc *Toolchain) { tc.Command = nil },
		"empty argument":   func(tc *Toolchain) { tc.Command = []string{"go", ""} },
		"control argument": func(tc *Toolchain) { tc.Command = []string{"go\ntest"} },
		"bad directory":    func(tc *Toolchain) { tc.WorkingDir = "../x" },
		"PATH":             func(tc *Toolchain) { tc.Env["PATH"] = "/usr/local/go/bin" },
		"NODE_OPTIONS":     func(tc *Toolchain) { tc.Env["NODE_OPTIONS"] = "--require=/x" },
		"SC_MODEL":         func(tc *Toolchain) { tc.Env["SC_MODEL"] = "other" },
		"GIT_DIR":          func(tc *Toolchain) { tc.Env["GIT_DIR"] = "/x" },
		"XDG_CACHE_HOME":   func(tc *Toolchain) { tc.Env["XDG_CACHE_HOME"] = "/x" },
		"NO_COLOR":         func(tc *Toolchain) { tc.Env["NO_COLOR"] = "0" },
		"LD_PRELOAD":       func(tc *Toolchain) { tc.Env["LD_PRELOAD"] = "/x.so" },
		"HOME":             func(tc *Toolchain) { tc.Env["HOME"] = "/x" },
	} {
		if parse(edit, WorkspaceOfflineTests) == nil {
			t.Fatal("invalid toolchain accepted:", name)
		}
	}
	// Unknown nested fields and explicit nulls are refused as everywhere else.
	a := assignmentFixture(now)
	a.Workspace = WorkspaceOfflineTests
	a.Toolchain = toolchainFixture()
	raw, _ := json.Marshal(a)
	for _, bad := range []string{
		strings.Replace(string(raw), `"root":`, `"extra":1,"root":`, 1),
		strings.Replace(string(raw), `"toolchain":{`, `"toolchain":null,"x":{`, 1),
		strings.Replace(string(raw), `"env":{"GOFLAGS":"-mod=mod","GOTOOLCHAIN":"local"}`, `"env":{}`, 1),
	} {
		if _, err := ParseAssignment([]byte(bad), workspacebundle.Digest([]byte(bad)), now); err == nil {
			t.Fatal("ambiguous toolchain accepted")
		}
	}
	// A reviewer never receives a toolchain.
	if _, err := compileFixture(t, now, func(a *Assignment, p *Profile) {
		a.Workspace = WorkspaceOfflineTests
		a.Toolchain = toolchainFixture()
		p.Version = ReviewProfileVersion
	}); err == nil {
		t.Fatal("reviewer workload with a toolchain")
	}
}

func TestToolchainPromptNamesTheReviewedTestCommand(t *testing.T) {
	a := assignmentFixture(time.Now())
	a.Workspace = WorkspaceOfflineTests
	a.Toolchain = toolchainFixture()
	prompt := BuildPrompt(a, "policy")
	command := strings.Index(prompt, "\ngo test ./...\n")
	untrusted := strings.Index(prompt, "<untrusted-issue-data>")
	if command < 0 || untrusted < 0 || command > untrusted || strings.Contains(prompt, "no build toolchain") || !strings.Contains(prompt, "from the repository root:") {
		t.Fatal("toolchain guidance", prompt)
	}
	a.Toolchain.Command = []string{"sh", "-c", "go test ./... && echo 'ok'"}
	a.Toolchain.WorkingDir = "service"
	prompt = BuildPrompt(a, "policy")
	if !strings.Contains(prompt, "\nsh -c 'go test ./... && echo '\\''ok'\\'''\n") || !strings.Contains(prompt, "from the repository directory service:") {
		t.Fatal("quoted command", prompt)
	}
	a.Toolchain = nil
	if !strings.Contains(BuildPrompt(a, "policy"), "no build toolchain") {
		t.Fatal("offline guidance without a toolchain")
	}
}

func TestLaunchEnvironmentPrependsTheToolchain(t *testing.T) {
	p := profileFixture()
	base := launchEnvironment(p, nil, "/workspaces")
	want := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/workspaces/home", "TMPDIR=/workspaces/tmp", "LC_ALL=C.UTF-8", "NO_COLOR=1", "CI=1",
		"SC_CONFIG_PATH=" + InputDirectory + "/scc-config.json", "SC_BASE_URL=http://127.0.0.1:8091/v1", "SC_MODEL=fixture-model",
		"NODE_OPTIONS=--max-old-space-size=256", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "PYTHONDONTWRITEBYTECODE=1"}
	if !reflect.DeepEqual(base, want) {
		t.Fatal("environment without a toolchain changed", base)
	}
	tc := toolchainFixture()
	tc.Path = []string{"bin", "pkg/tool"}
	env := launchEnvironment(p, tc, "/workspaces")
	if env[0] != "PATH=/toolchain/root/bin:/toolchain/root/pkg/tool:/usr/local/bin:/usr/bin:/bin" || env[2] != "TMPDIR=/scratch/tmp" {
		t.Fatal("toolchain search path", env)
	}
	if !reflect.DeepEqual(env[len(env)-3:], []string{"XDG_CACHE_HOME=/scratch/cache", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"}) {
		t.Fatal("toolchain variables", env)
	}
}

func TestPrepareToolchainRefusesShadowedWorkerTools(t *testing.T) {
	dir := t.TempDir()
	root, scratch := filepath.Join(dir, "root"), filepath.Join(dir, "scratch")
	if os.MkdirAll(filepath.Join(root, "bin"), 0o755) != nil || os.Mkdir(scratch, 0o755) != nil || os.WriteFile(filepath.Join(root, "bin", "go"), nil, 0o555) != nil {
		t.Fatal("fixture")
	}
	tc := toolchainFixture()
	if err := prepareToolchain(root, scratch, tc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tmp", "cache"} {
		if info, err := os.Stat(filepath.Join(scratch, name)); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatal("scratch directory", name)
		}
	}
	// A second run in the same scratch volume is refused.
	if prepareToolchain(root, scratch, tc) == nil {
		t.Fatal("existing scratch accepted")
	}
	if prepareToolchain(filepath.Join(dir, "missing"), t.TempDir(), tc) == nil {
		t.Fatal("missing toolchain accepted")
	}
	for _, tool := range shadowedTools {
		path := filepath.Join(root, "bin", tool)
		if os.Symlink("go", path) != nil {
			t.Fatal("fixture")
		}
		if prepareToolchain(root, t.TempDir(), tc) == nil {
			t.Fatal("shadowed worker tool accepted:", tool)
		}
		_ = os.Remove(path)
	}
}

// podQuota is what a ResourceQuota charges for a pod: per resource, the
// larger of the containers' sum and the largest init container.
func podQuota(t *testing.T, pod map[string]any) map[string]resource.Quantity {
	t.Helper()
	charge := map[string]resource.Quantity{}
	spec := pod["spec"].(map[string]any)
	add := func(containers []any, initContainers bool) {
		for _, c := range containers {
			resources := c.(map[string]any)["resources"].(map[string]any)
			for _, kind := range []string{"requests", "limits"} {
				for name, value := range resources[kind].(map[string]any) {
					if name == "ephemeral-storage" {
						continue
					}
					key, q := kind+"."+name, resource.MustParse(value.(string))
					current := charge[key]
					if initContainers {
						if q.Cmp(current) > 0 {
							charge["init."+key] = q
						}
						continue
					}
					current.Add(q)
					charge[key] = current
				}
			}
		}
	}
	add(spec["containers"].([]any), false)
	if inits, ok := spec["initContainers"].([]any); ok {
		add(inits, true)
	}
	for key, q := range charge {
		if after, ok := strings.CutPrefix(key, "init."); ok {
			if q.Cmp(charge[after]) > 0 {
				charge[after] = q
			}
			delete(charge, key)
		}
	}
	return charge
}

func TestToolchainExecutorQuotaGrowsByTheToolchainAdditions(t *testing.T) {
	now := time.Now()
	base, err := compileFixture(t, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := compileFixture(t, now, func(a *Assignment, _ *Profile) { a.Workspace = WorkspaceOfflineTests; a.Toolchain = toolchainFixture() })
	if err != nil {
		t.Fatal(err)
	}
	// The chart's original executor quota.
	original := map[string]string{"requests.cpu": "1", "requests.memory": "1Gi", "limits.cpu": "6", "limits.memory": "4Gi"}
	baseCharge, toolchainCharge := podQuota(t, base.workload.Pod), podQuota(t, tc.workload.Pod)
	for key, value := range ToolchainExecutorQuota() {
		quota, before := resource.MustParse(value), resource.MustParse(original[key])
		perPod := toolchainCharge[key]
		delta := perPod.DeepCopy()
		delta.Sub(baseCharge[key])
		total := before.DeepCopy()
		for i := 0; i < ExecutorPods; i++ {
			total.Add(delta)
		}
		if quota.Cmp(total) != 0 {
			t.Fatal("quota does not grow by exactly the toolchain additions", key, quota.String(), total.String())
		}
		need := perPod.DeepCopy()
		for i := 1; i < ExecutorPods; i++ {
			need.Add(perPod)
		}
		if quota.Cmp(need) < 0 {
			t.Fatal("quota does not fit four toolchain pods", key)
		}
	}
}

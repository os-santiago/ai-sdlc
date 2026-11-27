package repositorytests

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// ServiceContainerPrefix names the native sidecar of each manifest service:
// service-<name>. No other container of a repository-tests pod uses it.
const ServiceContainerPrefix = "service-"

// ServiceContainer is the sidecar name of a manifest service.
func ServiceContainer(s project.Service) string { return ServiceContainerPrefix + s.Name }

// serviceSidecars renders each manifest service as a native sidecar (an init
// container with restartPolicy Always): the kubelet starts them, in order,
// after the transfer sidecar and before the tests container, keeps them
// running while the tests run and stops them once the tests container exits,
// so the pod still completes with the tests. A service that exits is
// restarted, which the controller's observation refuses (no restart is ever
// accepted), so a crashed service fails the run instead of being reused.
//
// A sidecar mounts only its own empty volume: never the workspace, the
// runner, the settings or the pod identity. The pod gives it no service
// account token and no service links, and the sandbox's network=none leaves
// it only the loopback interface it shares with the tests.
func serviceSidecars(v project.Verification) (containers, volumes []any) {
	for _, s := range v.Services {
		name := ServiceContainer(s)
		quota := map[string]any{"cpu": s.CPU, "memory": s.Memory, "ephemeral-storage": s.EphemeralStorage}
		container := map[string]any{"name": name, "image": s.Image, "imagePullPolicy": "IfNotPresent", "restartPolicy": "Always",
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": s.User, "runAsGroup": s.GroupID(),
				"readOnlyRootFilesystem": !s.WritableRootFilesystem, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			"resources": map[string]any{"requests": quota, "limits": quota}}
		if len(s.Args) > 0 {
			container["args"] = append([]string(nil), s.Args...)
		}
		if len(s.Env) > 0 {
			names := make([]string, 0, len(s.Env))
			for n := range s.Env {
				names = append(names, n)
			}
			sort.Strings(names)
			env := make([]any, 0, len(names))
			for _, n := range names {
				env = append(env, map[string]any{"name": n, "value": s.Env[n]})
			}
			container["env"] = env
		}
		if len(s.WritablePaths) > 0 {
			// One volume per service, fresh for every pod, so each run starts
			// from an empty service state; each path is its own subdirectory.
			mounts := make([]any, 0, len(s.WritablePaths))
			for i, p := range s.WritablePaths {
				mounts = append(mounts, map[string]any{"name": name, "mountPath": p, "subPath": strconv.Itoa(i)})
			}
			container["volumeMounts"] = mounts
			volumes = append(volumes, map[string]any{"name": name, "emptyDir": map[string]any{"sizeLimit": s.EphemeralStorage}})
		}
		containers = append(containers, container)
	}
	return containers, volumes
}

// ErrServices reports a service that did not accept a connection in time.
var ErrServices = errors.New("verification service not ready")

// WaitServices waits until every service accepts a TCP connection on
// 127.0.0.1 at its port, each within its own ReadySeconds measured from the
// same start (the services start together, before the tests container). The
// services are checked in parallel, so a slow service never uses up the
// bound of another. It only connects and closes: nothing is sent. The runner
// calls it before any repository code runs, so the repository cannot have
// opened a port first.
func WaitServices(ctx context.Context, services []project.Service) error {
	start := time.Now()
	results := make(chan error, len(services))
	for _, s := range services {
		go func(port int, deadline time.Time) { results <- waitPort(ctx, port, deadline) }(s.Port, start.Add(time.Duration(s.ReadySeconds)*time.Second))
	}
	var failed error
	for range services {
		if err := <-results; err != nil {
			failed = err
		}
	}
	return failed
}

func waitPort(ctx context.Context, port int, deadline time.Time) error {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		attempt, stop := context.WithTimeout(ctx, time.Second)
		conn, err := (&net.Dialer{}).DialContext(attempt, "tcp4", address)
		stop()
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ErrServices
		case <-time.After(100 * time.Millisecond):
		}
	}
}

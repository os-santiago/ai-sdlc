package project

import (
	"regexp"
	"strings"
)

// Service is a backing service (a database, a broker, a registry) that the
// repository's tests reach over the pod's loopback, in place of Quarkus Dev
// Services or Testcontainers, which need a container engine the offline
// verifier does not have. It runs as a native sidecar of every
// repository-tests pod, in the same gVisor sandbox (no network beyond
// loopback), and the test command starts only once its TCP port accepts a
// connection.
//
// Every field is reviewed installation configuration: the image, its
// arguments and its environment come only from the manifest, never from the
// repository, an issue or model output, and a candidate cannot change them.
// Values are not secrets: they are visible in the pod spec and in the
// verifier's settings, so a service must use throwaway test credentials.
//
// Service is a renderer-neutral definition: it holds no Kubernetes types and
// this package does not build pods. The repository-tests pod compiler renders
// it as a native sidecar (repositorytests.serviceSidecars); a Tekton task
// renderer can map the same value to a Task's sidecars, with the same
// identity, security, volume and readiness rules.
type Service struct {
	// Name is the sidecar's name suffix (container service-<name>).
	Name string `json:"name"`
	// Image is the service image pinned by digest.
	Image string `json:"image"`
	// Args replace the image's CMD; the image's ENTRYPOINT is kept.
	Args []string `json:"args,omitempty"`
	// Env is the service's own reviewed environment. The tests container
	// never receives it; verification.env configures the tests.
	Env map[string]string `json:"env,omitempty"`
	// User and Group are the numeric identity the service runs as. Root is
	// refused for both: the verifier namespace enforces the restricted Pod
	// Security profile. Group defaults to User.
	User  int64  `json:"user"`
	Group *int64 `json:"group,omitempty"`
	// Port is the loopback TCP port the tests use; it is also the
	// readiness check.
	Port int `json:"port"`
	// ReadySeconds bounds the wait for Port to accept a connection.
	ReadySeconds int `json:"readySeconds"`
	// WritablePaths are the directories the service writes (data, sockets,
	// temporary files). Each is mounted from the service's own empty,
	// size-limited volume, fresh for every pod.
	WritablePaths []string `json:"writablePaths,omitempty"`
	// WritableRootFilesystem opts out of the read-only root filesystem for
	// an image that cannot run without writing outside WritablePaths.
	WritableRootFilesystem bool `json:"writableRootFilesystem,omitempty"`
	// CPU, Memory and EphemeralStorage are the service container's requests
	// and limits (equal); EphemeralStorage also bounds its volume.
	CPU              string `json:"cpu"`
	Memory           string `json:"memory"`
	EphemeralStorage string `json:"ephemeralStorage"`
}

// Bounds of Verification.Services.
const (
	MaxServices             = 6
	MaxServiceArgs          = 32
	MaxServiceEnv           = 16
	MaxServiceWritablePaths = 8
	MinServicePort          = 1024
	MaxServicePort          = 65535
	MinServiceReadySeconds  = 1
	// MaxServiceReadySeconds keeps the readiness wait small next to the test
	// timeout: the services start while the tree is imported, and the whole
	// verification must fit the signed command's lifetime.
	MaxServiceReadySeconds = 180
	maxServiceID           = 1<<31 - 1
)

var (
	serviceName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,13}[a-z0-9])?$`)
	// serviceExcluded are kernel, device and runtime locations a service
	// volume may not cover.
	serviceExcluded = []string{"/proc", "/sys", "/dev", "/run/secrets", "/var/run/secrets"}
)

// GroupID is the service's numeric group.
func (s Service) GroupID() int64 {
	if s.Group == nil {
		return s.User
	}
	return *s.Group
}

// ServiceReadySeconds is the longest service readiness bound, 0 without
// services. The services start together, so the runner waits at most this
// long for all of them.
func (v Verification) ServiceReadySeconds() int {
	longest := 0
	for _, s := range v.Services {
		longest = max(longest, s.ReadySeconds)
	}
	return longest
}

func validServices(services []Service) bool {
	if len(services) > MaxServices {
		return false
	}
	names, ports := map[string]bool{}, map[int]bool{}
	for _, s := range services {
		if s.validate() != nil || names[s.Name] || ports[s.Port] {
			return false
		}
		names[s.Name], ports[s.Port] = true, true
	}
	return true
}

// expandable reports a value that Kubernetes would rewrite: $(NAME) refers to
// another variable and $$ collapses to $. A reviewed value must run exactly
// as written, so '$' is refused.
func expandable(value string) bool { return strings.ContainsRune(value, '$') }

func (s Service) validate() error {
	if !serviceName.MatchString(s.Name) || !pinnedImage.MatchString(s.Image) || len(s.Image) > 512 ||
		s.User < 1 || s.User > maxServiceID || (s.Group != nil && (*s.Group < 1 || *s.Group > maxServiceID)) ||
		s.Port < MinServicePort || s.Port > MaxServicePort ||
		s.ReadySeconds < MinServiceReadySeconds || s.ReadySeconds > MaxServiceReadySeconds ||
		!cpuQuantity.MatchString(s.CPU) || !byteQuantity.MatchString(s.Memory) || !byteQuantity.MatchString(s.EphemeralStorage) ||
		len(s.Args) > MaxServiceArgs || len(s.Env) > MaxServiceEnv || len(s.WritablePaths) > MaxServiceWritablePaths {
		return ErrManifest
	}
	for _, arg := range s.Args {
		if arg == "" || len(arg) > 1024 || strings.IndexFunc(arg, hiddenRune) >= 0 || expandable(arg) {
			return ErrManifest
		}
	}
	for name, value := range s.Env {
		if !envName.MatchString(name) || strings.HasPrefix(name, "LD_") || value == "" || len(value) > MaxEnvValueBytes ||
			strings.IndexFunc(value, hiddenRune) >= 0 || expandable(value) {
			return ErrManifest
		}
	}
	for i, p := range s.WritablePaths {
		if len(p) < 2 || len(p) > MaxToolchainRootBytes || p[0] != '/' || cleanPath(p[1:]) != nil {
			return ErrManifest
		}
		for _, excluded := range serviceExcluded {
			if underRoot(excluded, p) {
				return ErrManifest
			}
		}
		// Distinct and not nested, so every mount has its own directory.
		for _, other := range s.WritablePaths[:i] {
			if underRoot(other, p) || underRoot(p, other) {
				return ErrManifest
			}
		}
	}
	return nil
}

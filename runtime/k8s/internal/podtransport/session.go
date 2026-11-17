package podtransport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

const maxPodBytes = 1024 * 1024

var containerIDPattern = regexp.MustCompile(`^[a-z0-9-]+://[a-f0-9]{32,128}$`)
var imageIDPattern = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9./:_-]*@)?sha256:[a-f0-9]{64}$`)

// Binding is supplied by the trusted controller, never by an executor. Current
// signed execution authority and durable lifecycle ownership remain caller duties.
type Binding struct {
	Namespace, WorkerImage, MailboxImage, PodUID string
	Assignment, Profile, ADev                    []byte
}

type instance struct{ ID, ImageID, Image string }

// Session captures the exact initially observed container instances. A new
// session is not a retry or a replacement for durable assignment authority.
type Session struct {
	client                               *Client
	expected                             core.Pod
	identity                             modelmailbox.Identity
	expires                              time.Time
	instances                            map[string]instance
	sourceSHA, assignmentDigest, treeSHA string
	reviewProfile                        *executor.Profile
}

func Bind(ctx context.Context, c *Client, b Binding) (*Session, error) {
	if c == nil || c.http == nil || c.config == nil {
		return nil, ErrTransport
	}
	if len(b.Assignment) == 0 || len(b.Assignment) > executor.MaxAssignmentBytes || len(b.Profile) == 0 || len(b.Profile) > executor.MaxProfileBytes || len(b.ADev) == 0 || len(b.ADev) > 64*1024 {
		return nil, ErrTransport
	}
	b.Assignment = append([]byte(nil), b.Assignment...)
	b.Profile = append([]byte(nil), b.Profile...)
	b.ADev = append([]byte(nil), b.ADev...)
	w, err := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.Assignment, b.Profile, b.ADev, time.Now())
	if err != nil {
		return nil, ErrTransport
	}
	a, err := executor.ParseAssignment(b.Assignment, modelmailbox.Digest(b.Assignment), time.Now())
	if err != nil {
		return nil, ErrTransport
	}
	id := modelmailbox.Identity{Attempt: fmt.Sprintf("%s-%d", a.WorkItemID, a.Attempt), PodUID: b.PodUID}
	if id.Validate() != nil {
		return nil, ErrTransport
	}
	raw, err := json.Marshal(w.Pod)
	if err != nil {
		return nil, ErrTransport
	}
	s := &Session{client: c, identity: id, expires: time.UnixMilli(a.ExpiresAt), sourceSHA: a.SourceSHA, assignmentDigest: modelmailbox.Digest(b.Assignment), treeSHA: a.TreeSHA256}
	if profile, e := executor.ParseReviewProfile(b.Profile, a.ProfileSHA256); e == nil {
		s.reviewProfile = &profile
	}
	if authority.DecodeStrict(raw, &s.expected) != nil {
		return nil, ErrTransport
	}
	applyDefaults(&s.expected.Spec)
	observed, err := s.observe(ctx, false)
	if err != nil {
		return nil, err
	}
	s.instances = observed
	return s, nil
}

// These are exact API defaults observed on the supported Kubernetes 1.35
// profile, not fields ignored wholesale. Additional admission changes fail.
func applyDefaults(s *core.PodSpec) {
	s.DNSPolicy = core.DNSClusterFirst
	s.SchedulerName = "default-scheduler"
	s.ServiceAccountName = "default"
	s.DeprecatedServiceAccount = "default"
	priority := int32(0)
	s.Priority = &priority
	preemption := core.PreemptLowerPriority
	s.PreemptionPolicy = &preemption
	seconds := int64(300)
	s.Tolerations = []core.Toleration{
		{Key: "node.kubernetes.io/not-ready", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute, TolerationSeconds: &seconds},
		{Key: "node.kubernetes.io/unreachable", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute, TolerationSeconds: &seconds},
	}
	for i := range s.Containers {
		c := &s.Containers[i]
		c.TerminationMessagePath = "/dev/termination-log"
		c.TerminationMessagePolicy = core.TerminationMessageReadFile
		c.Resources.Requests[core.ResourceEphemeralStorage] = c.Resources.Limits[core.ResourceEphemeralStorage].DeepCopy()
		for j := range c.Env {
			if c.Env[j].ValueFrom != nil && c.Env[j].ValueFrom.FieldRef != nil {
				c.Env[j].ValueFrom.FieldRef.APIVersion = "v1"
			}
		}
	}
	for i := range s.Volumes {
		if d := s.Volumes[i].DownwardAPI; d != nil {
			mode := int32(0644)
			d.DefaultMode = &mode
			for j := range d.Items {
				if d.Items[j].FieldRef != nil {
					d.Items[j].FieldRef.APIVersion = "v1"
				}
			}
		}
	}
}

func (s *Session) podURL() string {
	return s.client.base + "/api/v1/namespaces/" + s.expected.Namespace + "/pods/" + s.expected.Name
}

type observation struct {
	instances        map[string]instance
	workerExitCode   *int32
	workerTerminalID string
}

func (s *Session) observe(ctx context.Context, allowWorkerExit bool) (map[string]instance, error) {
	observed, err := s.inspect(ctx, allowWorkerExit)
	return observed.instances, err
}

func (s *Session) inspect(ctx context.Context, allowWorkerExit bool) (observation, error) {
	if !time.Now().Before(s.expires) {
		return observation{}, ErrTransport
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(5*time.Second), s.expires))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.podURL(), nil)
	if err != nil {
		return observation{}, ErrTransport
	}
	req.Header.Set("Accept", "application/json")
	if s.client.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.client.config.BearerToken)
	}
	response, err := s.client.http.Do(req)
	if err != nil {
		return observation{}, ErrTransport
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		return observation{}, ErrPermission
	}
	if response.StatusCode != 200 {
		return observation{}, ErrTransport
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPodBytes+1))
	var pod core.Pod
	if err != nil || len(raw) > maxPodBytes || authority.DecodeStrict(raw, &pod) != nil {
		return observation{}, ErrTransport
	}
	if string(pod.UID) != s.identity.PodUID || pod.Name != s.expected.Name || pod.Namespace != s.expected.Namespace || pod.DeletionTimestamp != nil || pod.Status.Phase != core.PodRunning || pod.Spec.NodeName == "" || !reflect.DeepEqual(pod.Labels, s.expected.Labels) || !reflect.DeepEqual(pod.Annotations, s.expected.Annotations) {
		return observation{}, ErrTransport
	}
	// Scheduling selects nodeName. Every other specification field must match
	// the compiled template plus the exact supported API defaults above.
	pod.Spec.NodeName = ""
	if !equality.Semantic.DeepEqual(pod.Spec, s.expected.Spec) || len(pod.Status.ContainerStatuses) != 2 || len(pod.Status.InitContainerStatuses) != 0 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return observation{}, ErrTransport
	}
	instances := map[string]instance{}
	var workerExitCode *int32
	var workerTerminalID string
	expectedImages := map[string]string{}
	for _, container := range s.expected.Spec.Containers {
		expectedImages[container.Name] = container.Image
	}
	for _, status := range pod.Status.ContainerStatuses {
		stateOK := status.State.Running != nil && status.State.Terminated == nil && status.State.Waiting == nil
		if allowWorkerExit && status.Name == "worker" && status.State.Terminated != nil && status.State.Running == nil && status.State.Waiting == nil {
			// The worker may consume the final response and exit before the
			// post-publication GET. This is not candidate acceptance evidence.
			stateOK = true
			code := status.State.Terminated.ExitCode
			workerExitCode = &code
			workerTerminalID = status.State.Terminated.ContainerID
		}
		if (status.Name != "worker" && status.Name != "mailbox") || status.RestartCount != 0 || !stateOK || !containerIDPattern.MatchString(status.ContainerID) || len(status.ImageID) > 1024 || !imageIDPattern.MatchString(status.ImageID) || len(status.Image) == 0 || len(status.Image) > 1024 {
			return observation{}, ErrTransport
		}
		// The supported kind/containerd profile reports its actual manifest
		// digest in ImageID, while Image may be an imported tag. Require that
		// content digest, not the tag, to match the compiler's immutable pin.
		// Multi-platform index pins need explicit platform resolution first.
		image := expectedImages[status.Name]
		if status.ImageID[strings.LastIndex(status.ImageID, "@")+1:] != image[strings.LastIndex(image, "@")+1:] {
			return observation{}, ErrTransport
		}
		if _, duplicate := instances[status.Name]; duplicate {
			return observation{}, ErrTransport
		}
		instances[status.Name] = instance{status.ContainerID, status.ImageID, status.Image}
	}
	if s.instances != nil && !reflect.DeepEqual(instances, s.instances) {
		return observation{}, ErrTransport
	}
	if ctx.Err() != nil || !time.Now().Before(s.expires) {
		return observation{}, ErrTransport
	}
	return observation{instances: instances, workerExitCode: workerExitCode, workerTerminalID: workerTerminalID}, nil
}

func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

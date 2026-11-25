package podtransport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
)

// VerificationResources compiles only the fixed offline recipe. Its caller
// fences each effect with current signed ownership. It has no arbitrary exec,
// model, Secret, publishing or namespace capability. Every role has its own Pod.
type VerificationResources struct{ resources map[string]*Resources }
type VerificationResults struct{ Baseline, Candidate, Reference []byte }

var verificationRoles = []string{"baseline", "candidate", "reference", "tests"}

func PlanVerification(client *Client, c verification.Command, in verification.Inputs, image, runtimeClass string, admission RuntimeClassAdmission) (*VerificationResources, error) {
	if client == nil || client.http == nil || client.config == nil {
		return nil, ErrTransport
	}
	w, err := verification.Compile(c, in, image, runtimeClass, time.Now())
	if err != nil {
		return nil, ErrTransport
	}
	v := &VerificationResources{resources: map[string]*Resources{}}
	for _, kind := range verificationRoles {
		r := &Resources{client: client, binding: Binding{Namespace: "ai-sdlc-verifiers", RuntimeClass: runtimeClass, Admission: admission}, expires: time.UnixMilli(c.ExpiresAt)}
		raw, _ := json.Marshal(w.ConfigMap)
		if authority.DecodeStrict(raw, &r.config) != nil {
			return nil, ErrTransport
		}
		raw, _ = json.Marshal(w.Pods[kind])
		if authority.DecodeStrict(raw, &r.pod) != nil {
			return nil, ErrTransport
		}
		v.resources[kind] = r
	}
	return v, nil
}
func (v *VerificationResources) resource(kind string) (*Resources, error) {
	if v == nil || v.resources == nil || v.resources[kind] == nil {
		return nil, ErrTransport
	}
	return v.resources[kind], nil
}
func (v *VerificationResources) fetch(ctx context.Context, endpoint, accept string, limit int64) ([]byte, error) {
	r, err := v.resource("reference")
	if err != nil {
		return nil, err
	}
	return r.fetch(ctx, endpoint, accept, limit)
}

// fetch is one bounded GET with the client's identity and no redirects.
func (r *Resources) fetch(ctx context.Context, endpoint, accept string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrTransport
	}
	req.Header.Set("Accept", accept)
	if r.client.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.client.config.BearerToken)
	}
	response, err := r.client.http.Do(req)
	if err != nil {
		return nil, ErrTransport
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || response.StatusCode != 200 || len(raw) == 0 || int64(len(raw)) > limit {
		return nil, ErrTransport
	}
	return raw, nil
}

// Additional NetworkPolicies are additive, so observing one deny policy alone
// is insufficient. Reject additional rules or pagination instead of assuming
// denial. Installation separately probes the network-none runtime enforcement.
func (v *VerificationResources) isolation(ctx context.Context) error {
	r, err := v.resource("reference")
	if err != nil {
		return err
	}
	return r.isolation(ctx)
}

// isolation requires the verifier namespace's single deny-all policy.
func (r *Resources) isolation(ctx context.Context) error {
	raw, err := r.fetch(ctx, r.client.base+"/apis/networking.k8s.io/v1/namespaces/ai-sdlc-verifiers/networkpolicies?limit=16", "application/json", 65536)
	if err != nil {
		return err
	}
	var policies networking.NetworkPolicyList
	if authority.DecodeStrict(raw, &policies) != nil || policies.Continue != "" || len(policies.Items) != 1 {
		return ErrTransport
	}
	p := policies.Items[0]
	if p.Name != "deny-all" || p.Namespace != "ai-sdlc-verifiers" || p.DeletionTimestamp != nil || len(p.Spec.PodSelector.MatchLabels) != 0 || len(p.Spec.PodSelector.MatchExpressions) != 0 || len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 || len(p.Spec.PolicyTypes) != 2 {
		return ErrTransport
	}
	types := map[networking.PolicyType]bool{}
	for _, kind := range p.Spec.PolicyTypes {
		types[kind] = true
	}
	if !types[networking.PolicyTypeIngress] || !types[networking.PolicyTypeEgress] {
		return ErrTransport
	}
	return nil
}
func (v *VerificationResources) CreateConfig(ctx context.Context) (string, error) {
	r, e := v.resource("reference")
	if e != nil || v.isolation(ctx) != nil {
		return "", ErrTransport
	}
	return r.CreateConfig(ctx)
}
func (v *VerificationResources) CreatePod(ctx context.Context, kind, configUID string) (string, error) {
	r, e := v.resource(kind)
	if e != nil || v.isolation(ctx) != nil {
		return "", ErrTransport
	}
	return r.CreatePodObserved(ctx, configUID)
}
func (v *VerificationResources) DeletePod(ctx context.Context, kind, uid string) error {
	r, e := v.resource(kind)
	if e != nil {
		return e
	}
	return r.DeletePod(ctx, uid)
}
func (v *VerificationResources) DeleteConfig(ctx context.Context, uid string) error {
	r, e := v.resource("reference")
	if e != nil {
		return e
	}
	return r.DeleteConfig(ctx, uid)
}
func (v *VerificationResources) observe(ctx context.Context, kind, uid string) (bool, instance, error) {
	r, e := v.resource(kind)
	if e != nil || !resourceUID.MatchString(uid) {
		return false, instance{}, ErrTransport
	}
	raw, status, err := r.request(ctx, http.MethodGet, "pods", r.pod.Name, nil, false)
	if err != nil || status != 200 {
		return false, instance{}, ErrTransport
	}
	var pod core.Pod
	if authority.DecodeStrict(raw, &pod) != nil || !exactPodTemplate(pod, r.pod, uid, r.binding.Admission) {
		return false, instance{}, ErrTransport
	}
	if pod.Status.Phase == core.PodPending || pod.Status.Phase == core.PodRunning {
		return false, instance{}, nil
	}
	if pod.Status.Phase != core.PodSucceeded || len(pod.Status.ContainerStatuses) != 1 {
		return false, instance{}, ErrTransport
	}
	s := pod.Status.ContainerStatuses[0]
	if s.Name != kind || s.RestartCount != 0 || s.State.Running != nil || s.State.Waiting != nil || s.State.Terminated == nil || s.State.Terminated.ExitCode != 0 || s.State.Terminated.ContainerID != s.ContainerID || !containerIDPattern.MatchString(s.ContainerID) || len(s.ImageID) > 1024 || !imageIDPattern.MatchString(s.ImageID) {
		return false, instance{}, ErrTransport
	}
	image := r.pod.Spec.Containers[0].Image
	if !imageMatchesPin(s.Image, s.ImageID, image) {
		return false, instance{}, ErrTransport
	}
	return true, instance{s.ContainerID, s.ImageID, s.Image}, nil
}
func (v *VerificationResources) log(ctx context.Context, kind string) ([]byte, error) {
	if kind != "baseline" && kind != "candidate" && kind != "reference" {
		return nil, ErrTransport
	}
	r, e := v.resource(kind)
	if e != nil {
		return nil, e
	}
	// Kubernetes negotiates the API response before proxying the plain log stream.
	// Accept text/plain returns 406 on supported kind versions; bounds and
	// the recipe parser still reject anything other than the expected result.
	return v.fetch(ctx, r.url("pods", r.pod.Name)+"/log?container="+url.QueryEscape(kind)+"&limitBytes=32769", "*/*", 32768)
}
func (v *VerificationResources) ReadResults(ctx context.Context, uids map[string]string) (VerificationResults, error) {
	r, e := v.resource("reference")
	if e != nil || len(uids) != 4 || v.isolation(ctx) != nil {
		return VerificationResults{}, ErrTransport
	}
	for _, kind := range verificationRoles {
		if !resourceUID.MatchString(uids[kind]) {
			return VerificationResults{}, ErrTransport
		}
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(75*time.Second), r.expires))
	defer cancel()
	original := map[string]instance{}
	for {
		complete := true
		for _, kind := range verificationRoles {
			done, observed, err := v.observe(ctx, kind, uids[kind])
			if err != nil {
				return VerificationResults{}, err
			}
			if !done {
				complete = false
			} else {
				original[kind] = observed
			}
		}
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			return VerificationResults{}, ErrTransport
		case <-time.After(250 * time.Millisecond):
		}
	}
	var result VerificationResults
	for kind, target := range map[string]*[]byte{"baseline": &result.Baseline, "candidate": &result.Candidate, "reference": &result.Reference} {
		raw, err := v.log(ctx, kind)
		if err != nil {
			return VerificationResults{}, err
		}
		*target = raw
		complete, current, err := v.observe(ctx, kind, uids[kind])
		if err != nil || !complete || current != original[kind] {
			return VerificationResults{}, ErrTransport
		}
	}
	if v.isolation(ctx) != nil {
		return VerificationResults{}, ErrTransport
	}
	return result, nil
}

// CleanupVerification deletes only a name/UID pair acknowledged in the durable
// verifier effect ledger. No lookup/adoption by a missing UID is permitted.
func (client *Client) CleanupVerification(ctx context.Context, kind, name, uid string) error {
	if client == nil || client.http == nil || client.config == nil || !resourceUID.MatchString(uid) {
		return ErrTransport
	}
	resource := "pods"
	suffix := kind
	switch kind {
	case "config":
		resource = "configmaps"
		suffix = "inputs"
	case "baseline", "candidate", "reference", "tests":
	default:
		return ErrTransport
	}
	pattern := regexp.MustCompile(`^verify-[a-f0-9]{32}-` + suffix + `$`)
	if !pattern.MatchString(name) {
		return ErrTransport
	}
	r := &Resources{client: client, binding: Binding{Namespace: "ai-sdlc-verifiers"}}
	return r.remove(ctx, resource, name, uid)
}

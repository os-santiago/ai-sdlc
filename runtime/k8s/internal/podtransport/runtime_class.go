package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	node "k8s.io/api/node/v1"
)

// ErrRuntimeClassUnavailable means the installation's sandbox RuntimeClass
// could not be observed. Callers stay not ready and claim no work; there is
// no fallback to a pod without a sandbox runtime.
var ErrRuntimeClassUnavailable = errors.New("sandbox runtime class unavailable")

const maxRuntimeClassBytes = 64 * 1024

// CheckRuntimeClass reads one cluster-scoped RuntimeClass by name. It is a
// single bounded GET with the client's identity: it never creates, changes or
// lists runtime classes. The class must exist, carry a handler and not be
// terminating. The handler itself belongs to the platform, so any standard
// sandbox (gVisor, Kata, GKE Sandbox, AKS Pod Sandboxing) is accepted.
func (c *Client) CheckRuntimeClass(ctx context.Context, name string) error {
	if c == nil || c.http == nil || c.config == nil || !executor.ValidRuntimeClass(name) {
		return ErrRuntimeClassUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/apis/node.k8s.io/v1/runtimeclasses/"+name, nil)
	if err != nil {
		return ErrRuntimeClassUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if c.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.BearerToken)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return ErrRuntimeClassUnavailable
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxRuntimeClassBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || len(raw) > maxRuntimeClassBytes {
		return ErrRuntimeClassUnavailable
	}
	// Server-populated fields vary by platform, so this decode is not strict;
	// only the identity, handler and lifecycle are relied on.
	var class node.RuntimeClass
	if json.Unmarshal(raw, &class) != nil || class.Kind != "RuntimeClass" || class.Name != name || class.Handler == "" || class.DeletionTimestamp != nil {
		return ErrRuntimeClassUnavailable
	}
	return nil
}

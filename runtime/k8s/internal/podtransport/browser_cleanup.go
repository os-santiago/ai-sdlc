package podtransport

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"time"
)

// BrowserCleanupClient exposes no CREATE, application patch or health method.
// Only the current controller's resource-containment gate authorizes DELETE.
type BrowserCleanupClient struct{ client *Client }

func NewBrowserCleanupClient(client *Client) (*BrowserCleanupClient, error) {
	if client == nil || client.http == nil || client.config == nil {
		return nil, ErrTransport
	}
	return &BrowserCleanupClient{client: client}, nil
}
func (c *BrowserCleanupClient) CleanupBrowser(ctx context.Context, profile browsercheck.Profile, request browsercheck.Request, route ConsoleRollout, uid string, gate browsercheck.CleanupGate) error {
	if c == nil || gate == nil || !resourceUID.MatchString(uid) || gate.CheckCleanup(ctx) != nil {
		return ErrTransport
	}
	pod, e := CompileBrowserPod(profile, request, route, time.UnixMilli(request.IssuedAt))
	if e != nil {
		return e
	}
	b := &BrowserResources{profile: profile, request: request, pod: pod, console: &ConsoleDeployment{client: c.client}, resources: &Resources{client: c.client, binding: Binding{Namespace: browsercheck.Namespace}, pod: pod, expires: time.UnixMilli(request.ExpiresAt)}}
	if gate.CheckCleanup(ctx) != nil || b.cleanup(ctx, uid, gate.CheckCleanup) != nil || gate.CheckCleanup(ctx) != nil {
		return ErrTransport
	}
	return gate.RecordCleanup(ctx)
}

package podtransport

import (
	"context"
	"regexp"
	"time"
)

var cleanupPodName = regexp.MustCompile(`^executor-[a-f0-9-]{36}-[0-9]{1,2}$`)
var cleanupConfigName = regexp.MustCompile(`^assignment-[a-f0-9]{32}$`)

// CleanupAcknowledged has no execution capability. Inputs are immutable names
// and acknowledged UIDs from the controller ledger, never an issue or model.
// Namespace is fixed to the installation's isolated executor namespace.
func (c *Client) CleanupAcknowledged(ctx context.Context, namespace, podName, configName, podUID, configUID string) error {
	if c == nil || c.http == nil || namespace != "ai-sdlc-executors" || !cleanupPodName.MatchString(podName) || !cleanupConfigName.MatchString(configName) || (podUID != "" && !resourceUID.MatchString(podUID)) || (configUID != "" && !resourceUID.MatchString(configUID)) {
		return ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	r := &Resources{client: c, binding: Binding{Namespace: namespace}}
	if podUID != "" {
		if err := r.remove(ctx, "pods", podName, podUID); err != nil {
			return err
		}
	}
	if configUID != "" {
		if err := r.remove(ctx, "configmaps", configName, configUID); err != nil {
			return err
		}
	}
	return nil
}

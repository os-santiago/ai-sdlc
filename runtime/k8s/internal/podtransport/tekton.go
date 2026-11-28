package podtransport

import (
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/tektontasks"
)

// TektonCluster returns the generator tools' cluster client: the same
// verified API server identity, reaching only what the execution controller's
// ai-sdlc-ci Role grants (PipelineRuns and candidate ConfigMaps there).
func (c *Client) TektonCluster() (tektontasks.DynamicCluster, error) {
	if c == nil || c.config == nil {
		return tektontasks.DynamicCluster{}, ErrTransport
	}
	client, err := dynamic.NewForConfig(rest.CopyConfig(c.config))
	if err != nil {
		return tektontasks.DynamicCluster{}, ErrTransport
	}
	return tektontasks.DynamicCluster{Client: client}, nil
}

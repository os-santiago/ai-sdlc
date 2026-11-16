package releasing

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// Snapshots and receipts are observations. Only authenticated journal readers
// establish their origin; constructing them grants no effect authority.
type Snapshot struct {
	DeploymentUID   string `json:"deploymentUID"`
	ResourceVersion string `json:"resourceVersion"`
	Generation      int64  `json:"generation"`
	Image           string `json:"image"`
	TargetSHA256    string `json:"targetSHA256"`
	ObservedAt      int64  `json:"observedAt"`
}

type Receipt struct {
	Before        Snapshot `json:"before"`
	After         Snapshot `json:"after"`
	RequestSHA256 string   `json:"requestSHA256"`
	Rollback      bool     `json:"rollback"`
}

func (r Receipt) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }

type Rollout struct {
	DeploymentUID    string `json:"deploymentUID"`
	Generation       int64  `json:"generation"`
	ReplicaSetUID    string `json:"replicaSetUID"`
	PodUID           string `json:"podUID"`
	PodIP            string `json:"podIP"`
	ImageID          string `json:"imageID"`
	Node             string `json:"node"`
	ServiceUID       string `json:"serviceUID"`
	ServiceIP        string `json:"serviceIP"`
	EndpointSliceUID string `json:"endpointSliceUID"`
	TargetSHA256     string `json:"targetSHA256"`
	Image            string `json:"image"`
	ObservedAt       int64  `json:"observedAt"`
}

func (r Rollout) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }

// Health contains only bounded response hashes and exact routing/workload
// identities. Its origin must be authenticated by the deployment journal.
type Health struct {
	Version        string   `json:"version"`
	Rollout        Rollout  `json:"rollout"`
	ResponseSHA256 []string `json:"responseSHA256"`
	ObservedAt     int64    `json:"observedAt"`
}

func (h Health) Digest() string { raw, _ := json.Marshal(h); return authority.Digest(raw) }

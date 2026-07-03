package deployer

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type SandboxRequest struct {
	ScanID                    string           `json:"scan_id"`
	TargetImage               string           `json:"target_image"`
	Topology                  *SandboxTopology `json:"topology,omitempty"`
	TopologyJSON              string           `json:"topology_json,omitempty"`
	PreferredEndpointWorkload string           `json:"preferred_endpoint_workload,omitempty"`
}

type SandboxResponse struct {
	Namespace        string                   `json:"namespace"`
	Endpoint         string                   `json:"endpoint"`
	EndpointWorkload string                   `json:"endpoint_workload,omitempty"`
	Workloads        []SandboxWorkloadStatus  `json:"workloads,omitempty"`
	Summary          SandboxDeploymentSummary `json:"summary,omitempty"`
}

type SandboxWorkloadStatus struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Service  string `json:"service,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type SandboxDeploymentSummary struct {
	Requested        int  `json:"requested"`
	Deployed         int  `json:"deployed"`
	Ready            int  `json:"ready"`
	NotReady         int  `json:"not_ready"`
	Skipped          int  `json:"skipped"`
	EndpointSelected bool `json:"endpoint_selected"`
}

type SandboxTopology struct {
	Services    []TopologyWorkload `json:"services,omitempty"`
	Deployments []TopologyWorkload `json:"deployments,omitempty"`
	Containers  []TopologyWorkload `json:"containers,omitempty"`
}

type TopologyWorkload struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name"`
	Image    string            `json:"image"`
	Ports    []TopologyPort    `json:"ports,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Replicas *int32            `json:"replicas,omitempty"`
	Liveness *corev1.Probe     `json:"liveness_probe,omitempty"`
}

type TopologyPort struct {
	Name          string `json:"name,omitempty"`
	Number        int32  `json:"number,omitempty"`
	Port          int32  `json:"port,omitempty"`
	TargetPort    int32  `json:"target_port,omitempty"`
	ContainerPort int32  `json:"container_port,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
}

type topologyWorkloadAlias struct {
	ID            string          `json:"id,omitempty"`
	Name          string          `json:"name"`
	Image         string          `json:"image"`
	Ports         []TopologyPort  `json:"ports,omitempty"`
	Env           json.RawMessage `json:"env,omitempty"`
	EnvVars       json.RawMessage `json:"env_vars,omitempty"`
	Replicas      *int32          `json:"replicas,omitempty"`
	Liveness      json.RawMessage `json:"liveness_probe,omitempty"`
	LivenessCamel json.RawMessage `json:"livenessProbe,omitempty"`
}

type Activities struct {
	k8s kubernetes.Interface
}

func NewActivities(k8s kubernetes.Interface) *Activities {
	return &Activities{k8s: k8s}
}

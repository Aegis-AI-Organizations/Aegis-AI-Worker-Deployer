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
	Services        []TopologyWorkload     `json:"services,omitempty"`
	Deployments     []TopologyWorkload     `json:"deployments,omitempty"`
	Containers      []TopologyWorkload     `json:"containers,omitempty"`
	Connections     []TopologyConnection   `json:"connections,omitempty"`
	Routes          []TopologyConnection   `json:"routes,omitempty"`
	ExternalMocks   []ExternalMockScenario `json:"external_mocks,omitempty"`
	DatabaseSchemas []DatabaseSchema       `json:"database_schemas,omitempty"`
}

type TopologyConnection struct {
	Source          string `json:"source,omitempty"`
	SourceName      string `json:"source_name,omitempty"`
	SourceNameCamel string `json:"sourceName,omitempty"`
	Target          string `json:"target,omitempty"`
	TargetName      string `json:"target_name,omitempty"`
	TargetNameCamel string `json:"targetName,omitempty"`
}

type ExternalMockScenario struct {
	Host    string              `json:"host,omitempty"`
	Routes  []ExternalMockRoute `json:"routes,omitempty"`
	Capture bool                `json:"capture,omitempty"`
}

type ExternalMockRoute struct {
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Latency string            `json:"latency,omitempty"`
}

type DatabaseSchema struct {
	Engine              string          `json:"engine,omitempty"`
	Host                string          `json:"host,omitempty"`
	Port                int32           `json:"port,omitempty"`
	DatabaseName        string          `json:"database_name,omitempty"`
	Username            string          `json:"username,omitempty"`
	Password            string          `json:"password,omitempty"`
	SourceContainerID   string          `json:"source_container_id,omitempty"`
	SourceContainerName string          `json:"source_container_name,omitempty"`
	Tables              []DatabaseTable `json:"tables,omitempty"`
}

type DatabaseTable struct {
	Name        string               `json:"name,omitempty"`
	Columns     []DatabaseColumn     `json:"columns,omitempty"`
	Indexes     []DatabaseIndex      `json:"indexes,omitempty"`
	ForeignKeys []DatabaseForeignKey `json:"foreign_keys,omitempty"`
}

type DatabaseColumn struct {
	Name         string `json:"name,omitempty"`
	DataType     string `json:"data_type,omitempty"`
	Nullable     bool   `json:"nullable,omitempty"`
	PrimaryKey   bool   `json:"primary_key,omitempty"`
	DefaultValue string `json:"default_value,omitempty"`
}

type DatabaseIndex struct {
	Name    string   `json:"name,omitempty"`
	Columns []string `json:"columns,omitempty"`
	Unique  bool     `json:"unique,omitempty"`
}

type DatabaseForeignKey struct {
	Name              string   `json:"name,omitempty"`
	Columns           []string `json:"columns,omitempty"`
	ReferencedTable   string   `json:"referenced_table,omitempty"`
	ReferencedColumns []string `json:"referenced_columns,omitempty"`
}

type SeedDatabaseRequest struct {
	ScanID          string           `json:"scan_id"`
	DatabaseSchemas []DatabaseSchema `json:"database_schemas,omitempty"`
	RestoreSQL      string           `json:"restore_sql,omitempty"`
	SeedFlag        string           `json:"seed_flag,omitempty"`
}

type SeedDatabaseResponse struct {
	Namespace     string   `json:"namespace"`
	Seeded        []string `json:"seeded"`
	SeededCount   int      `json:"seeded_count"`
	SeedFlag      string   `json:"seed_flag"`
	Anonymized    bool     `json:"anonymized"`
	DebugBundle   string   `json:"debug_bundle,omitempty"`
	TrafficBundle string   `json:"traffic_bundle,omitempty"`
}

type TopologyWorkload struct {
	ID                 string                      `json:"id,omitempty"`
	Name               string                      `json:"name"`
	Image              string                      `json:"image"`
	ImageArchiveRef    string                      `json:"image_archive_ref,omitempty"`
	ImageArchiveObject string                      `json:"image_archive_object,omitempty"`
	Ports              []TopologyPort              `json:"ports,omitempty"`
	Env                map[string]string           `json:"env,omitempty"`
	Replicas           *int32                      `json:"replicas,omitempty"`
	Liveness           *corev1.Probe               `json:"liveness_probe,omitempty"`
	DependsOn          []string                    `json:"depends_on,omitempty"`
	WaitFor            []string                    `json:"wait_for,omitempty"`
	Required           *bool                       `json:"required,omitempty"`
	Command            []string                    `json:"command,omitempty"`
	Args               []string                    `json:"args,omitempty"`
	WorkingDir         string                      `json:"working_dir,omitempty"`
	InitContainers     []TopologyInitContainer     `json:"init_containers,omitempty"`
	ConfigFiles        []TopologyFile              `json:"config_files,omitempty"`
	SecretFiles        []TopologyFile              `json:"secret_files,omitempty"`
	EmptyDirs          []TopologyEmptyDir          `json:"empty_dirs,omitempty"`
	Resources          corev1.ResourceRequirements `json:"resources,omitempty"`
	SecurityContext    *corev1.SecurityContext     `json:"security_context,omitempty"`
	PodSecurityContext *corev1.PodSecurityContext  `json:"pod_security_context,omitempty"`
	Stateful           bool                        `json:"stateful,omitempty"`
	Service            TopologyService             `json:"service,omitempty"`
}

type TopologyInitContainer struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Command    []string          `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

type TopologyFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    *int32 `json:"mode,omitempty"`
}

type TopologyEmptyDir struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
}

type TopologyService struct {
	Type     string   `json:"type,omitempty"`
	Headless bool     `json:"headless,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
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
	ID                      string                      `json:"id,omitempty"`
	Name                    string                      `json:"name"`
	Image                   string                      `json:"image"`
	ImageArchiveRef         string                      `json:"image_archive_ref,omitempty"`
	ImageArchiveRefCamel    string                      `json:"imageArchiveRef,omitempty"`
	ImageArchiveObject      string                      `json:"image_archive_object,omitempty"`
	ImageArchiveObjectCamel string                      `json:"imageArchiveObject,omitempty"`
	Ports                   []TopologyPort              `json:"ports,omitempty"`
	Env                     json.RawMessage             `json:"env,omitempty"`
	EnvVars                 json.RawMessage             `json:"env_vars,omitempty"`
	Replicas                *int32                      `json:"replicas,omitempty"`
	Liveness                json.RawMessage             `json:"liveness_probe,omitempty"`
	LivenessCamel           json.RawMessage             `json:"livenessProbe,omitempty"`
	DependsOn               []string                    `json:"depends_on,omitempty"`
	WaitFor                 []string                    `json:"wait_for,omitempty"`
	Required                *bool                       `json:"required,omitempty"`
	Command                 []string                    `json:"command,omitempty"`
	Args                    []string                    `json:"args,omitempty"`
	WorkingDir              string                      `json:"working_dir,omitempty"`
	InitContainers          []TopologyInitContainer     `json:"init_containers,omitempty"`
	ConfigFiles             []TopologyFile              `json:"config_files,omitempty"`
	SecretFiles             []TopologyFile              `json:"secret_files,omitempty"`
	EmptyDirs               []TopologyEmptyDir          `json:"empty_dirs,omitempty"`
	Resources               corev1.ResourceRequirements `json:"resources,omitempty"`
	SecurityContext         *corev1.SecurityContext     `json:"security_context,omitempty"`
	PodSecurityContext      *corev1.PodSecurityContext  `json:"pod_security_context,omitempty"`
	Stateful                bool                        `json:"stateful,omitempty"`
	Service                 TopologyService             `json:"service,omitempty"`
}

type sandboxTopologyAlias struct {
	Services             []TopologyWorkload     `json:"services,omitempty"`
	Deployments          []TopologyWorkload     `json:"deployments,omitempty"`
	Containers           []TopologyWorkload     `json:"containers,omitempty"`
	Connections          []TopologyConnection   `json:"connections,omitempty"`
	Routes               []TopologyConnection   `json:"routes,omitempty"`
	ExternalMocks        []ExternalMockScenario `json:"external_mocks,omitempty"`
	ExternalMocksCamel   []ExternalMockScenario `json:"externalMocks,omitempty"`
	DatabaseSchemas      []DatabaseSchema       `json:"database_schemas,omitempty"`
	DatabaseSchemasCamel []DatabaseSchema       `json:"databaseSchemas,omitempty"`
}

type databaseSchemaAlias struct {
	Engine                   string          `json:"engine,omitempty"`
	Host                     string          `json:"host,omitempty"`
	Port                     int32           `json:"port,omitempty"`
	DatabaseName             string          `json:"database_name,omitempty"`
	DatabaseNameCamel        string          `json:"databaseName,omitempty"`
	Username                 string          `json:"username,omitempty"`
	Password                 string          `json:"password,omitempty"`
	SourceContainerID        string          `json:"source_container_id,omitempty"`
	SourceContainerIDCamel   string          `json:"sourceContainerId,omitempty"`
	SourceContainerName      string          `json:"source_container_name,omitempty"`
	SourceContainerNameCamel string          `json:"sourceContainerName,omitempty"`
	Tables                   []DatabaseTable `json:"tables,omitempty"`
}

type Activities struct {
	k8s kubernetes.Interface
}

func NewActivities(k8s kubernetes.Interface) *Activities {
	return &Activities{k8s: k8s}
}

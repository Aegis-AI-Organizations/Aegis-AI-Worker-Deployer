package deployer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

const (
	defaultTemporalHost      = "localhost:7233"
	defaultTemporalNamespace = "default"
	defaultTaskQueue         = "DEPLOYER_TASK_QUEUE"
	sandboxNamespacePrefix   = "aegis-war-room-"
	topologyMockSecretPrefix = "aegis-mock-secret"
	sandboxRuntimeClassName  = "gvisor"
	externalMockName         = "external-api-mock"
	externalMockHTTPPort     = int32(8080)
	externalMockDNSPort      = int32(53)
	fallbackKubeDNSIP        = "10.43.0.10"
	fallbackExternalMockIP   = "10.43.0.200"
)

var (
	newK8sClient                   = newKubernetesClient
	temporalDial                   = client.Dial
	newWorker                      = worker.New
	temporalConnectMaxAttempts     = 0
	temporalConnectRetryDelay      = 2 * time.Second
	topologyDeploymentReadyTimeout = 15 * time.Second
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

func Start() {
	if err := Run(context.Background()); err != nil {
		log.Fatalf("deployer worker stopped: %v", err)
	}
}

func Run(ctx context.Context) error {
	_ = ctx

	k8s, err := newK8sClient()
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	var temporalClient client.Client
	temporalHost := getenv("TEMPORAL_HOST", defaultTemporalHost)
	temporalNamespace := getenv("TEMPORAL_NAMESPACE", defaultTemporalNamespace)
	log.Printf("Connecting Deployer worker to Temporal at %s (namespace=%s)...", temporalHost, temporalNamespace)
	temporalOptions, err := temporalClientOptions(temporalHost, temporalNamespace)
	if err != nil {
		return err
	}

	for attempt := 1; ; attempt++ {
		if err = ctx.Err(); err != nil {
			return fmt.Errorf("connect temporal cancelled: %w", err)
		}

		temporalClient, err = temporalDial(temporalOptions)
		if err == nil {
			break
		}

		if temporalConnectMaxAttempts > 0 && attempt >= temporalConnectMaxAttempts {
			return fmt.Errorf("connect temporal: %w", err)
		}

		log.Printf("Failed to connect to Temporal at %s (attempt %d): %v", temporalHost, attempt, err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("connect temporal cancelled: %w", ctx.Err())
		case <-time.After(temporalConnectRetryDelay):
		}
	}
	defer temporalClient.Close()

	stopTimeout := envDurationSeconds("TEMPORAL_WORKER_STOP_TIMEOUT_SECONDS", 14*time.Minute)
	w := newWorker(temporalClient, getenv("DEPLOYER_TASK_QUEUE", defaultTaskQueue), worker.Options{
		WorkerStopTimeout: stopTimeout,
	})
	activities := NewActivities(k8s)
	w.RegisterActivityWithOptions(activities.CreateSandbox, activity.RegisterOptions{Name: "CreateSandbox"})
	w.RegisterActivityWithOptions(activities.DestroySandbox, activity.RegisterOptions{Name: "DestroySandbox"})

	log.Printf("Aegis AI Worker Deployer listening on queue %s", getenv("DEPLOYER_TASK_QUEUE", defaultTaskQueue))
	return w.Run(worker.InterruptCh())
}

func temporalClientOptions(host, namespace string) (client.Options, error) {
	options := client.Options{
		HostPort:  host,
		Namespace: namespace,
	}
	if !envBool("TEMPORAL_TLS_ENABLE") {
		return options, nil
	}

	tlsConfig, err := temporalTLSConfig()
	if err != nil {
		return client.Options{}, fmt.Errorf("configure temporal tls: %w", err)
	}
	options.ConnectionOptions.TLS = tlsConfig
	return options, nil
}

func temporalTLSConfig() (*tls.Config, error) {
	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: getenv("TEMPORAL_TLS_SERVER_NAME", ""),
	}

	if caPath := getenv("TEMPORAL_TLS_CA_PATH", ""); caPath != "" {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read ca certificate: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("parse ca certificate")
		}
		config.RootCAs = roots
	}

	certPath := getenv("TEMPORAL_TLS_CERT_PATH", "")
	keyPath := getenv("TEMPORAL_TLS_KEY_PATH", "")
	if certPath == "" && keyPath == "" {
		return config, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("TEMPORAL_TLS_CERT_PATH and TEMPORAL_TLS_KEY_PATH must be configured together")
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	config.Certificates = []tls.Certificate{certificate}
	return config, nil
}

func (a *Activities) CreateSandbox(ctx context.Context, req SandboxRequest) (SandboxResponse, error) {
	req.ScanID = strings.TrimSpace(req.ScanID)
	req.TargetImage = strings.TrimSpace(req.TargetImage)
	if req.ScanID == "" {
		return SandboxResponse{}, errors.New("scan_id is required")
	}
	topology, err := req.parseTopology()
	if err != nil {
		return SandboxResponse{}, err
	}
	if req.TargetImage == "" && topology == nil {
		return SandboxResponse{}, errors.New("target_image or topology_json is required")
	}

	namespace := sandboxNamespace(req.ScanID)
	if err := validateSandboxNamespace(namespace); err != nil {
		return SandboxResponse{}, err
	}

	podName := "target-" + req.ScanID
	serviceName := "svc-" + req.ScanID

	log.Printf(
		"[CreateSandbox] scan=%s image=%s namespace=%s pod=%s service=%s",
		req.ScanID,
		req.TargetImage,
		namespace,
		podName,
		serviceName,
	)

	log.Printf("[CreateSandbox] scan=%s creating namespace %s", req.ScanID, namespace)
	if err := a.createNamespace(ctx, namespace, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}
	log.Printf("[CreateSandbox] scan=%s namespace %s ready", req.ScanID, namespace)
	if err := a.createSandboxNetworkPolicy(ctx, namespace, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}

	if topology != nil {
		return a.createTopologySandbox(ctx, req.ScanID, namespace, topology, req.PreferredEndpointWorkload)
	}

	log.Printf("[CreateSandbox] scan=%s creating pod %s/%s", req.ScanID, namespace, podName)
	if err := a.createPod(ctx, namespace, podName, req.ScanID, req.TargetImage); err != nil {
		return SandboxResponse{}, err
	}
	log.Printf("[CreateSandbox] scan=%s pod %s/%s created", req.ScanID, namespace, podName)

	log.Printf("[CreateSandbox] scan=%s creating service %s/%s", req.ScanID, namespace, serviceName)
	if err := a.createService(ctx, namespace, serviceName, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}
	log.Printf("[CreateSandbox] scan=%s service %s/%s created", req.ScanID, namespace, serviceName)

	log.Printf("[CreateSandbox] scan=%s waiting for pod %s/%s to become Ready", req.ScanID, namespace, podName)
	if err := a.waitForPodReady(ctx, namespace, podName, time.Minute); err != nil {
		return SandboxResponse{}, err
	}

	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:80", serviceName, namespace)
	log.Printf("[CreateSandbox] scan=%s sandbox ready endpoint=%s", req.ScanID, endpoint)
	return SandboxResponse{
		Namespace: namespace,
		Endpoint:  endpoint,
	}, nil
}

func (a *Activities) DestroySandbox(ctx context.Context, scanID string) (string, error) {
	scanID = strings.TrimSpace(scanID)
	if scanID == "" {
		return "", errors.New("scan_id is required")
	}

	namespace := sandboxNamespace(scanID)
	if err := validateSandboxNamespace(namespace); err != nil {
		return "", err
	}

	log.Printf("[DestroySandbox] scan=%s deleting namespace %s", scanID, namespace)
	err := a.k8s.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		log.Printf("[DestroySandbox] scan=%s namespace %s already absent", scanID, namespace)
		return "CLEANED", nil
	}
	if err != nil {
		return "", fmt.Errorf("delete namespace %s: %w", namespace, err)
	}
	log.Printf("[DestroySandbox] scan=%s namespace %s deleted", scanID, namespace)
	return "CLEANED", nil
}

func (req SandboxRequest) parseTopology() (*SandboxTopology, error) {
	if req.Topology != nil {
		if err := req.Topology.validate(); err != nil {
			return nil, err
		}
		return req.Topology, nil
	}
	if strings.TrimSpace(req.TopologyJSON) == "" {
		return nil, nil
	}

	var topology SandboxTopology
	if err := json.Unmarshal([]byte(req.TopologyJSON), &topology); err != nil {
		return nil, fmt.Errorf("parse topology_json: %w", err)
	}
	if err := topology.validate(); err != nil {
		return nil, err
	}
	return &topology, nil
}

func (t *SandboxTopology) validate() error {
	workloads := t.workloads()
	if len(workloads) == 0 {
		return errors.New("topology must contain at least one service, deployment, or container")
	}
	seen := map[string]struct{}{}
	for i, workload := range workloads {
		name := strings.TrimSpace(workload.Name)
		if name == "" {
			return fmt.Errorf("topology workload %d name is required", i)
		}
		if strings.TrimSpace(workload.Image) == "" {
			return fmt.Errorf("topology workload %q image is required", name)
		}
		k8sName := kubernetesName(name)
		if _, ok := seen[k8sName]; ok {
			return fmt.Errorf("topology workload name %q collides after normalization", name)
		}
		seen[k8sName] = struct{}{}
		for _, port := range workload.Ports {
			if port.servicePort() <= 0 || port.containerPort() <= 0 {
				return fmt.Errorf("topology workload %q contains invalid port", name)
			}
		}
	}
	return nil
}

func (t *SandboxTopology) workloads() []TopologyWorkload {
	workloads := make([]TopologyWorkload, 0, len(t.Services)+len(t.Deployments)+len(t.Containers))
	workloads = append(workloads, t.Services...)
	workloads = append(workloads, t.Deployments...)
	workloads = append(workloads, t.Containers...)
	return workloads
}

func (w *TopologyWorkload) UnmarshalJSON(data []byte) error {
	var alias topologyWorkloadAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}

	env, err := parseTopologyEnv(alias.Env)
	if err != nil {
		return fmt.Errorf("parse env for workload %q: %w", alias.Name, err)
	}
	envVars, err := parseTopologyEnv(alias.EnvVars)
	if err != nil {
		return fmt.Errorf("parse env_vars for workload %q: %w", alias.Name, err)
	}
	for key, value := range envVars {
		env[key] = value
	}
	liveness, err := parseTopologyProbe(alias.Liveness, alias.LivenessCamel)
	if err != nil {
		return fmt.Errorf("parse liveness_probe for workload %q: %w", alias.Name, err)
	}

	*w = TopologyWorkload{
		ID:       strings.TrimSpace(alias.ID),
		Name:     strings.TrimSpace(alias.Name),
		Image:    strings.TrimSpace(alias.Image),
		Ports:    alias.Ports,
		Env:      env,
		Replicas: alias.Replicas,
		Liveness: liveness,
	}
	return nil
}

func parseTopologyEnv(raw json.RawMessage) (map[string]string, error) {
	env := map[string]string{}
	if len(raw) == 0 || string(raw) == "null" {
		return env, nil
	}

	var envMap map[string]string
	if err := json.Unmarshal(raw, &envMap); err == nil {
		for key, value := range envMap {
			env[strings.TrimSpace(key)] = value
		}
		return env, nil
	}

	var envList []struct {
		Name  string `json:"name"`
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &envList); err != nil {
		return nil, errors.New("env must be an object or a list of name/value pairs")
	}
	for _, item := range envList {
		key := strings.TrimSpace(item.Name)
		if key == "" {
			key = strings.TrimSpace(item.Key)
		}
		if key == "" {
			return nil, errors.New("env entry name is required")
		}
		env[key] = item.Value
	}
	return env, nil
}

func parseTopologyProbe(rawValues ...json.RawMessage) (*corev1.Probe, error) {
	for _, raw := range rawValues {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var probe corev1.Probe
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, errors.New("probe must be a Kubernetes probe object")
		}
		return &probe, nil
	}
	return nil, nil
}

func (a *Activities) createNamespace(ctx context.Context, namespace, scanID string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aegis-worker-deployer",
				"aegis-scan":                   scanID,
			},
		},
	}

	_, err := a.k8s.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] namespace %s already exists", namespace)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create namespace %s: %w", namespace, err)
	}
	log.Printf("[CreateSandbox] namespace %s created", namespace)
	return nil
}

func (a *Activities) createSandboxNetworkPolicy(ctx context.Context, namespace, scanID string) error {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-deny-egress",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aegis-worker-deployer",
				"aegis-scan":                   scanID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{},
						},
					},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name": "kube-system",
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: ptrProtocol(corev1.ProtocolUDP),
							Port:     ptrIntOrString(intstr.FromInt32(53)),
						},
						{
							Protocol: ptrProtocol(corev1.ProtocolTCP),
							Port:     ptrIntOrString(intstr.FromInt32(53)),
						},
					},
				},
			},
		},
	}

	_, err := a.k8s.NetworkingV1().NetworkPolicies(namespace).Create(ctx, policy, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] networkpolicy %s/%s already exists", namespace, policy.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create sandbox egress policy %s/%s: %w", namespace, policy.Name, err)
	}
	log.Printf("[CreateSandbox] networkpolicy %s/%s created", namespace, policy.Name)
	return nil
}

func (a *Activities) createTopologySandbox(ctx context.Context, scanID, namespace string, topology *SandboxTopology, preferredEndpointWorkload string) (SandboxResponse, error) {
	workloads := topology.workloads()
	log.Printf("[CreateSandbox] scan=%s deploying topology with %d workload(s)", scanID, len(workloads))
	if len(workloads) == 0 {
		return SandboxResponse{}, errors.New("topology does not contain any workload")
	}
	mockDNSIP, err := a.createExternalDependencyMock(ctx, namespace, scanID)
	if err != nil {
		return SandboxResponse{}, err
	}

	preferredEndpointWorkload = strings.TrimSpace(strings.ToLower(preferredEndpointWorkload))
	if preferredEndpointWorkload != "" {
		preferredEndpointWorkload = kubernetesName(preferredEndpointWorkload)
	}
	mockSecret := mockTopologySecret(scanID)
	firstServiceName := ""
	firstReadyServiceName := ""
	firstReadyServicePort := int32(0)
	createdWorkloads := make([]TopologyWorkload, 0, len(workloads))
	statuses := make([]SandboxWorkloadStatus, 0, len(workloads))
	for index, workload := range workloads {
		workload = sanitizeTopologySecrets(workload, mockSecret)
		name := kubernetesName(workload.Name)
		if firstServiceName == "" {
			firstServiceName = name
		}

		log.Printf(
			"[CreateSandbox] scan=%s topology workload %d/%d name=%s image=%s ports=%d env=%d",
			scanID,
			index+1,
			len(workloads),
			name,
			workload.Image,
			len(workload.normalizedPorts()),
			len(workload.Env),
		)
		if err := a.createDeployment(ctx, namespace, scanID, name, workload, mockDNSIP); err != nil {
			log.Printf("[CreateSandbox] deployment %s/%s failed; continuing topology deployment: %v", namespace, name, err)
			statuses = append(statuses, SandboxWorkloadStatus{
				Name:   name,
				Image:  workload.Image,
				Status: "skipped",
				Error:  err.Error(),
			})
			continue
		}
		if len(workload.normalizedPorts()) > 0 {
			if err := a.createTopologyService(ctx, namespace, scanID, name, workload); err != nil {
				log.Printf("[CreateSandbox] service %s/%s failed; workload remains deployed without service: %v", namespace, name, err)
				statuses = append(statuses, SandboxWorkloadStatus{
					Name:   name,
					Image:  workload.Image,
					Status: "deployed",
					Error:  err.Error(),
				})
				createdWorkloads = append(createdWorkloads, workload)
				continue
			}
		}
		statuses = append(statuses, SandboxWorkloadStatus{
			Name:    name,
			Image:   workload.Image,
			Service: serviceNameForWorkload(name, workload),
			Status:  "deployed",
		})
		createdWorkloads = append(createdWorkloads, workload)
	}
	if len(createdWorkloads) == 0 {
		return SandboxResponse{
			Namespace: namespace,
			Workloads: statuses,
			Summary: summarizeSandboxWorkloads(
				len(workloads),
				statuses,
				false,
			),
		}, errors.New("topology deployment failed for every workload")
	}

	for _, workload := range createdWorkloads {
		name := kubernetesName(workload.Name)
		ports := workload.normalizedPorts()
		if err := a.waitForDeploymentReady(ctx, namespace, name, topologyDeploymentReadyTimeout); err != nil {
			log.Printf("[CreateSandbox] deployment %s/%s is not ready; continuing topology deployment: %v", namespace, name, err)
			updateWorkloadStatus(statuses, name, "not_ready", "", err.Error())
			continue
		}
		endpoint := workloadEndpoint(namespace, name, ports)
		updateWorkloadStatus(statuses, name, "ready", endpoint, "")
		if firstReadyServiceName == "" && len(ports) > 0 {
			firstReadyServiceName = name
			firstReadyServicePort = ports[0].servicePort()
		}
	}

	endpointServiceName := firstReadyServiceName
	endpointPort := firstReadyServicePort
	if preferredEndpointWorkload != "" {
		preferredStatus, ok := findWorkloadStatus(statuses, preferredEndpointWorkload)
		if !ok {
			return SandboxResponse{
				Namespace: namespace,
				Workloads: statuses,
				Summary:   summarizeSandboxWorkloads(len(workloads), statuses, false),
			}, fmt.Errorf("preferred endpoint workload %q was not deployed", preferredEndpointWorkload)
		}
		if preferredStatus.Status != "ready" {
			return SandboxResponse{
					Namespace: namespace,
					Workloads: statuses,
					Summary:   summarizeSandboxWorkloads(len(workloads), statuses, false),
				}, fmt.Errorf(
					"preferred endpoint workload %q is %s: %s",
					preferredEndpointWorkload,
					preferredStatus.Status,
					strings.TrimSpace(preferredStatus.Error),
				)
		}
		preferredPorts := topologyPortsForWorkload(createdWorkloads, preferredEndpointWorkload)
		if len(preferredPorts) == 0 {
			return SandboxResponse{
				Namespace: namespace,
				Workloads: statuses,
				Summary:   summarizeSandboxWorkloads(len(workloads), statuses, false),
			}, fmt.Errorf("preferred endpoint workload %q does not expose any port", preferredEndpointWorkload)
		}
		endpointServiceName = preferredEndpointWorkload
		endpointPort = preferredPorts[0].servicePort()
	}
	if endpointServiceName == "" {
		endpointServiceName = firstServiceName
		firstWorkloadPorts := workloads[0].normalizedPorts()
		if len(firstWorkloadPorts) > 0 {
			endpointPort = firstWorkloadPorts[0].servicePort()
		}
		log.Printf("[CreateSandbox] scan=%s topology has no ready service; falling back to first declared endpoint", scanID)
	}

	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local", endpointServiceName, namespace)
	if endpointPort > 0 {
		endpoint = fmt.Sprintf("%s:%d", endpoint, endpointPort)
	}
	log.Printf("[CreateSandbox] scan=%s topology sandbox ready endpoint=%s", scanID, endpoint)
	return SandboxResponse{
		Namespace:        namespace,
		Endpoint:         endpoint,
		EndpointWorkload: endpointServiceName,
		Workloads:        statuses,
		Summary:          summarizeSandboxWorkloads(len(workloads), statuses, endpointServiceName != ""),
	}, nil
}

func mockTopologySecret(scanID string) string {
	scanID = strings.TrimSpace(scanID)
	if scanID == "" {
		return topologyMockSecretPrefix
	}
	return fmt.Sprintf("%s-%s", topologyMockSecretPrefix, scanID)
}

func sanitizeTopologySecrets(workload TopologyWorkload, secret string) TopologyWorkload {
	if len(workload.Env) == 0 {
		return workload
	}

	sanitized := make(map[string]string, len(workload.Env))
	for key, value := range workload.Env {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		sanitized[key] = replaceRedactedSecret(key, value, secret)
	}
	workload.Env = sanitized
	return workload
}

func replaceRedactedSecret(key, value, secret string) string {
	trimmedValue := strings.TrimSpace(value)
	if !strings.EqualFold(trimmedValue, "REDACTED") && !strings.Contains(trimmedValue, "<REDACTED") {
		return value
	}
	return mockValueForEnvKey(key, secret)
}

func mockValueForEnvKey(key, secret string) string {
	if secret == "" {
		secret = topologyMockSecretPrefix
	}
	key = strings.ToUpper(strings.TrimSpace(key))
	switch {
	case strings.Contains(key, "AWS_ACCESS_KEY_ID") || key == "AWS_KEY":
		return "AKIA0000000000000000"
	case strings.Contains(key, "AWS_SECRET_ACCESS_KEY"):
		return "aegis-mock-aws-secret"
	case strings.Contains(key, "JWT_SECRET"):
		return "aegis-mock-jwt-secret"
	case strings.Contains(key, "API_KEY") || strings.HasSuffix(key, "_KEY"):
		return "aegis-mock-api-key"
	case strings.Contains(key, "TOKEN"):
		return "aegis-mock-token"
	case strings.Contains(key, "PASS") ||
		strings.Contains(key, "PASSWORD") ||
		strings.Contains(key, "PWD") ||
		strings.Contains(key, "SECRET") ||
		strings.Contains(key, "PRIVATE_KEY"):
		return secret
	default:
		return "aegis-mock-value"
	}
}

func (a *Activities) createExternalDependencyMock(ctx context.Context, namespace, scanID string) (string, error) {
	mockIP, err := a.createExternalMockService(ctx, namespace, scanID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(mockIP) == "" {
		mockIP = fallbackExternalMockIP
	}
	kubeDNSIP := a.kubeDNSIP(ctx)
	if err := a.createExternalMockConfigMap(ctx, namespace, scanID, mockIP, kubeDNSIP); err != nil {
		return "", err
	}
	if err := a.createExternalMockDeployment(ctx, namespace, scanID); err != nil {
		return "", err
	}
	return mockIP, nil
}

func (a *Activities) createExternalMockService(ctx context.Context, namespace, scanID string) (string, error) {
	labels := externalMockLabels(scanID)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   externalMockName,
			Labels: labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt32(externalMockHTTPPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "dns-tcp",
					Port:       externalMockDNSPort,
					TargetPort: intstr.FromInt32(externalMockDNSPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "dns-udp",
					Port:       externalMockDNSPort,
					TargetPort: intstr.FromInt32(externalMockDNSPort),
					Protocol:   corev1.ProtocolUDP,
				},
			},
		},
	}

	created, err := a.k8s.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := a.k8s.CoreV1().Services(namespace).Get(ctx, externalMockName, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("read external mock service %s/%s: %w", namespace, externalMockName, getErr)
		}
		log.Printf("[CreateSandbox] service %s/%s already exists", namespace, externalMockName)
		return existing.Spec.ClusterIP, nil
	}
	if err != nil {
		return "", fmt.Errorf("create external mock service %s/%s: %w", namespace, externalMockName, err)
	}
	log.Printf("[CreateSandbox] service %s/%s created for external dependency mocking", namespace, externalMockName)
	return created.Spec.ClusterIP, nil
}

func (a *Activities) createExternalMockConfigMap(ctx context.Context, namespace, scanID, mockIP, kubeDNSIP string) error {
	if strings.TrimSpace(kubeDNSIP) == "" {
		kubeDNSIP = fallbackKubeDNSIP
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   externalMockName,
			Labels: externalMockLabels(scanID),
		},
		Data: map[string]string{
			"Corefile": externalMockCorefile(mockIP, kubeDNSIP),
			"default.conf": fmt.Sprintf(`server {
    listen %d default_server;
    access_log off;
    location / {
        add_header Content-Type text/plain;
        return 200 '';
    }
}
`, externalMockHTTPPort),
		},
	}
	_, err := a.k8s.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] configmap %s/%s already exists", namespace, externalMockName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create external mock configmap %s/%s: %w", namespace, externalMockName, err)
	}
	log.Printf("[CreateSandbox] configmap %s/%s created for external dependency mocking", namespace, externalMockName)
	return nil
}

func (a *Activities) createExternalMockDeployment(ctx context.Context, namespace, scanID string) error {
	replicas := int32(1)
	labels := externalMockLabels(scanID)
	mode := int32(420)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   externalMockName,
			Labels: labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RuntimeClassName: ptrString(sandboxRuntimeClassName),
					Containers: []corev1.Container{
						{
							Name:            "http",
							Image:           "nginx:1.27-alpine",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{{
								Name:          "http",
								ContainerPort: externalMockHTTPPort,
								Protocol:      corev1.ProtocolTCP,
							}},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "external-mock-config",
								MountPath: "/etc/nginx/conf.d/default.conf",
								SubPath:   "default.conf",
								ReadOnly:  true,
							}},
						},
						{
							Name:            "dns",
							Image:           "coredns/coredns:1.11.3",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            []string{"-conf", "/etc/coredns/Corefile"},
							Ports: []corev1.ContainerPort{
								{Name: "dns-tcp", ContainerPort: externalMockDNSPort, Protocol: corev1.ProtocolTCP},
								{Name: "dns-udp", ContainerPort: externalMockDNSPort, Protocol: corev1.ProtocolUDP},
							},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "external-mock-config",
								MountPath: "/etc/coredns/Corefile",
								SubPath:   "Corefile",
								ReadOnly:  true,
							}},
						},
					},
					Volumes: []corev1.Volume{{
						Name: "external-mock-config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: externalMockName},
								DefaultMode:          &mode,
							},
						},
					}},
				},
			},
		},
	}
	_, err := a.k8s.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] deployment %s/%s already exists", namespace, externalMockName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create external mock deployment %s/%s: %w", namespace, externalMockName, err)
	}
	log.Printf("[CreateSandbox] deployment %s/%s created for external dependency mocking", namespace, externalMockName)
	return nil
}

func (a *Activities) kubeDNSIP(ctx context.Context) string {
	for _, serviceName := range []string{"kube-dns", "coredns"} {
		service, err := a.k8s.CoreV1().Services("kube-system").Get(ctx, serviceName, metav1.GetOptions{})
		if err == nil && strings.TrimSpace(service.Spec.ClusterIP) != "" {
			return service.Spec.ClusterIP
		}
	}
	return fallbackKubeDNSIP
}

func externalMockCorefile(mockIP, kubeDNSIP string) string {
	return fmt.Sprintf(`cluster.local:53 {
    errors
    cache 30
    forward . %s
}
.:53 {
    errors
    template IN A . {
        match .*
        answer "{{ .Name }} 60 IN A %s"
    }
}
`, kubeDNSIP, mockIP)
}

func externalMockLabels(scanID string) map[string]string {
	return map[string]string{
		"app":                          externalMockName,
		"app.kubernetes.io/name":       externalMockName,
		"app.kubernetes.io/managed-by": "aegis-worker-deployer",
		"aegis-scan":                   scanID,
	}
}

func (a *Activities) createDeployment(ctx context.Context, namespace, scanID, name string, workload TopologyWorkload, mockDNSIP string) error {
	replicas := int32(1)
	if workload.Replicas != nil && *workload.Replicas > 0 {
		replicas = *workload.Replicas
	}
	containerPorts := workload.containerPorts()
	readinessProbe := localTCPProbe(containerPorts)
	var livenessProbe *corev1.Probe
	if workload.Liveness != nil {
		livenessProbe = localTCPProbe(containerPorts)
	}
	dnsPolicy := corev1.DNSClusterFirst
	var dnsConfig *corev1.PodDNSConfig
	if strings.TrimSpace(mockDNSIP) != "" {
		dnsPolicy = corev1.DNSNone
		dnsConfig = sandboxDNSConfig(namespace, mockDNSIP)
	}
	labels := topologyLabels(scanID, name)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RuntimeClassName: ptrString(sandboxRuntimeClassName),
					DNSPolicy:        dnsPolicy,
					DNSConfig:        dnsConfig,
					Containers: []corev1.Container{{
						Name:            name,
						Image:           strings.TrimSpace(workload.Image),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports:           containerPorts,
						Env:             workload.envVars(),
						ReadinessProbe:  readinessProbe,
						LivenessProbe:   livenessProbe,
					}},
				},
			},
		},
	}

	_, err := a.k8s.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] deployment %s/%s already exists", namespace, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create deployment %s/%s: %w", namespace, name, err)
	}
	log.Printf("[CreateSandbox] deployment %s/%s created replicas=%d image=%s", namespace, name, replicas, workload.Image)
	return nil
}

func localTCPProbe(containerPorts []corev1.ContainerPort) *corev1.Probe {
	if len(containerPorts) > 0 {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(containerPorts[0].ContainerPort),
				},
			},
			InitialDelaySeconds: 3,
			PeriodSeconds:       3,
			TimeoutSeconds:      1,
			FailureThreshold:    10,
		}
	}
	return nil
}

func sandboxDNSConfig(namespace, nameserver string) *corev1.PodDNSConfig {
	return &corev1.PodDNSConfig{
		Nameservers: []string{nameserver},
		Searches: []string{
			namespace + ".svc.cluster.local",
			"svc.cluster.local",
			"cluster.local",
		},
		Options: []corev1.PodDNSConfigOption{{
			Name:  "ndots",
			Value: ptrString("5"),
		}},
	}
}

func (a *Activities) createTopologyService(ctx context.Context, namespace, scanID, name string, workload TopologyWorkload) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: topologyLabels(scanID, name),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: topologyLabels(scanID, name),
			Ports:    workload.servicePorts(),
		},
	}

	_, err := a.k8s.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] service %s/%s already exists", namespace, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create service %s/%s: %w", namespace, name, err)
	}
	log.Printf("[CreateSandbox] service %s/%s created with %d port(s)", namespace, name, len(service.Spec.Ports))
	return nil
}

func serviceNameForWorkload(name string, workload TopologyWorkload) string {
	if len(workload.normalizedPorts()) == 0 {
		return ""
	}
	return name
}

func workloadEndpoint(namespace, name string, ports []TopologyPort) string {
	if len(ports) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"http://%s.%s.svc.cluster.local:%d",
		name,
		namespace,
		ports[0].servicePort(),
	)
}

func updateWorkloadStatus(statuses []SandboxWorkloadStatus, name, status, endpoint, errText string) {
	for i := range statuses {
		if statuses[i].Name != name {
			continue
		}
		statuses[i].Status = status
		if endpoint != "" {
			statuses[i].Endpoint = endpoint
		}
		if errText != "" {
			statuses[i].Error = errText
		}
		return
	}
}

func findWorkloadStatus(statuses []SandboxWorkloadStatus, name string) (SandboxWorkloadStatus, bool) {
	for _, status := range statuses {
		if status.Name == name {
			return status, true
		}
	}
	return SandboxWorkloadStatus{}, false
}

func topologyPortsForWorkload(workloads []TopologyWorkload, name string) []TopologyPort {
	for _, workload := range workloads {
		if kubernetesName(workload.Name) == name {
			return workload.normalizedPorts()
		}
	}
	return nil
}

func summarizeSandboxWorkloads(requested int, statuses []SandboxWorkloadStatus, endpointSelected bool) SandboxDeploymentSummary {
	summary := SandboxDeploymentSummary{
		Requested:        requested,
		EndpointSelected: endpointSelected,
	}
	for _, status := range statuses {
		switch status.Status {
		case "ready":
			summary.Deployed++
			summary.Ready++
		case "not_ready":
			summary.Deployed++
			summary.NotReady++
		case "deployed":
			summary.Deployed++
		case "skipped":
			summary.Skipped++
		}
	}
	return summary
}

func (a *Activities) createPod(ctx context.Context, namespace, podName, scanID, image string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
			Labels: map[string]string{
				"app":  "vulnerable-target",
				"scan": scanID,
			},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptrString(sandboxRuntimeClassName),
			Containers: []corev1.Container{{
				Name:            "target-container",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports: []corev1.ContainerPort{{
					ContainerPort: 80,
				}},
			}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	_, err := a.k8s.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] pod %s/%s already exists", namespace, podName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create pod %s/%s: %w", namespace, podName, err)
	}
	log.Printf("[CreateSandbox] pod %s/%s created with image %s", namespace, podName, image)
	return nil
}

func (a *Activities) createService(ctx context.Context, namespace, serviceName, scanID string) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":  "vulnerable-target",
				"scan": scanID,
			},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(80),
			}},
		},
	}

	_, err := a.k8s.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] service %s/%s already exists", namespace, serviceName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create service %s/%s: %w", namespace, serviceName, err)
	}
	log.Printf("[CreateSandbox] service %s/%s created", namespace, serviceName)
	return nil
}

func (a *Activities) waitForDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	log.Printf("[CreateSandbox] waiting for deployment %s/%s readiness (timeout=%s)", namespace, name, timeout)
	lastSummary := ""
	for {
		deployment, err := a.k8s.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read deployment %s/%s: %w", namespace, name, err)
		}

		expected := int32(1)
		if deployment.Spec.Replicas != nil {
			expected = *deployment.Spec.Replicas
		}
		summary := fmt.Sprintf(
			"replicas=%d updated=%d available=%d observed_generation=%d generation=%d",
			expected,
			deployment.Status.UpdatedReplicas,
			deployment.Status.AvailableReplicas,
			deployment.Status.ObservedGeneration,
			deployment.Generation,
		)
		if summary != lastSummary {
			log.Printf("[CreateSandbox] deployment %s/%s status=%s", namespace, name, summary)
			lastSummary = summary
		}
		if deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.UpdatedReplicas >= expected &&
			deployment.Status.AvailableReplicas >= expected {
			log.Printf("[CreateSandbox] deployment %s/%s is Ready", namespace, name)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for deployment %s/%s to become ready", namespace, name)
		case <-ticker.C:
		}
	}
}

func (a *Activities) waitForPodReady(ctx context.Context, namespace, podName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	log.Printf("[CreateSandbox] waiting for pod %s/%s readiness (timeout=%s)", namespace, podName, timeout)
	lastSummary := ""
	for {
		pod, err := a.k8s.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read pod %s/%s: %w", namespace, podName, err)
		}
		if err := checkImageErrors(pod); err != nil {
			log.Printf("[CreateSandbox] pod %s/%s image error: %v", namespace, podName, err)
			return err
		}
		summary := podStatusSummary(pod)
		if summary != lastSummary {
			log.Printf("[CreateSandbox] pod %s/%s status=%s", namespace, podName, summary)
			lastSummary = summary
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				log.Printf("[CreateSandbox] pod %s/%s is Ready", namespace, podName)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for pod %s/%s to become ready", namespace, podName)
		case <-ticker.C:
		}
	}
}

func podStatusSummary(pod *corev1.Pod) string {
	containerSummaries := make([]string, 0, len(pod.Status.ContainerStatuses))
	readyCount := 0
	for _, status := range pod.Status.ContainerStatuses {
		state := "unknown"
		switch {
		case status.State.Waiting != nil:
			state = "waiting:" + status.State.Waiting.Reason
		case status.State.Running != nil:
			state = "running"
		case status.State.Terminated != nil:
			state = "terminated:" + status.State.Terminated.Reason
		}
		if status.Ready {
			readyCount++
		}
		containerSummaries = append(containerSummaries, fmt.Sprintf("%s=%s", status.Name, state))
	}
	return fmt.Sprintf(
		"phase=%s ready=%d/%d containers=[%s]",
		pod.Status.Phase,
		readyCount,
		len(pod.Status.ContainerStatuses),
		strings.Join(containerSummaries, ","),
	)
}

func (w TopologyWorkload) normalizedPorts() []TopologyPort {
	if len(w.Ports) > 0 {
		return w.Ports
	}
	return nil
}

func (w TopologyWorkload) containerPorts() []corev1.ContainerPort {
	ports := w.normalizedPorts()
	containerPorts := make([]corev1.ContainerPort, 0, len(ports))
	for i, port := range ports {
		protocol := corev1.ProtocolTCP
		if strings.EqualFold(port.Protocol, "UDP") {
			protocol = corev1.ProtocolUDP
		}
		containerPorts = append(containerPorts, corev1.ContainerPort{
			Name:          portName(port, i),
			ContainerPort: port.containerPort(),
			Protocol:      protocol,
		})
	}
	return containerPorts
}

func (w TopologyWorkload) servicePorts() []corev1.ServicePort {
	ports := w.normalizedPorts()
	servicePorts := make([]corev1.ServicePort, 0, len(ports))
	for i, port := range ports {
		protocol := corev1.ProtocolTCP
		if strings.EqualFold(port.Protocol, "UDP") {
			protocol = corev1.ProtocolUDP
		}
		servicePorts = append(servicePorts, corev1.ServicePort{
			Name:       portName(port, i),
			Port:       port.servicePort(),
			TargetPort: intstr.FromInt32(port.containerPort()),
			Protocol:   protocol,
		})
	}
	return servicePorts
}

func (w TopologyWorkload) envVars() []corev1.EnvVar {
	if len(w.Env) == 0 {
		return nil
	}
	envVars := make([]corev1.EnvVar, 0, len(w.Env))
	for key, value := range w.Env {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		envVars = append(envVars, corev1.EnvVar{Name: key, Value: value})
	}
	return envVars
}

func (p TopologyPort) servicePort() int32 {
	if p.Port > 0 {
		return p.Port
	}
	if p.Number > 0 {
		return p.Number
	}
	if p.TargetPort > 0 {
		return p.TargetPort
	}
	return p.ContainerPort
}

func (p TopologyPort) containerPort() int32 {
	if p.ContainerPort > 0 {
		return p.ContainerPort
	}
	if p.TargetPort > 0 {
		return p.TargetPort
	}
	if p.Number > 0 {
		return p.Number
	}
	return p.Port
}

func portName(port TopologyPort, index int) string {
	if strings.TrimSpace(port.Name) != "" {
		return kubernetesName(port.Name)
	}
	return fmt.Sprintf("p-%d", index+1)
}

func topologyLabels(scanID, name string) map[string]string {
	return map[string]string{
		"app":                       name,
		"scan":                      scanID,
		"app.kubernetes.io/part-of": "aegis-sandbox",
	}
}

func kubernetesName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var builder strings.Builder
	lastHyphen := false
	for _, r := range value {
		valid := unicode.IsLetter(r) || unicode.IsDigit(r)
		switch {
		case valid:
			builder.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			builder.WriteRune('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		name = "workload"
	}
	if len(name) <= 50 {
		return name
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(name))
	return fmt.Sprintf("%s-%08x", strings.Trim(name[:41], "-"), hash.Sum32())
}

func checkImageErrors(pod *corev1.Pod) error {
	for _, status := range pod.Status.ContainerStatuses {
		waiting := status.State.Waiting
		if waiting == nil {
			continue
		}
		switch waiting.Reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
			return temporalNonRetryableError(fmt.Sprintf("failed to deploy target: %s - %s", waiting.Reason, waiting.Message))
		}
	}
	return nil
}

func temporalNonRetryableError(message string) error {
	return temporal.NewNonRetryableApplicationError(message, "SandboxDeploymentError", nil)
}

func ptrString(value string) *string {
	return &value
}

func ptrProtocol(value corev1.Protocol) *corev1.Protocol {
	return &value
}

func ptrIntOrString(value intstr.IntOrString) *intstr.IntOrString {
	return &value
}

func sandboxNamespace(scanID string) string {
	return sandboxNamespacePrefix + scanID
}

func validateSandboxNamespace(namespace string) error {
	if !strings.HasPrefix(namespace, sandboxNamespacePrefix) || len(namespace) == len(sandboxNamespacePrefix) {
		return fmt.Errorf("refusing to manage non-sandbox namespace %q", namespace)
	}
	return nil
}

func newKubernetesClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			return nil, err
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(config)
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envBool(key string) bool {
	switch strings.ToLower(getenv(key, "")) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envDurationSeconds(key string, fallback time.Duration) time.Duration {
	value := getenv(key, "")
	if value == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		log.Printf("Invalid %s=%q, using %s", key, value, fallback)
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

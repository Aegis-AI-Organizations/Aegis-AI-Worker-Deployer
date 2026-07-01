package deployer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const (
	defaultTemporalHost      = "localhost:7233"
	defaultTemporalNamespace = "default"
	defaultTaskQueue         = "DEPLOYER_TASK_QUEUE"
	sandboxNamespacePrefix   = "aegis-war-room-"
	topologyMockSecretPrefix = "aegis-mock-secret"
	sandboxRuntimeClassName  = "gvisor"
	sandboxRuntimeClassEnv   = "SANDBOX_RUNTIME_CLASS"
	retainSandboxEnv         = "RETAIN_WAR_ROOM_NAMESPACES"
	externalMockName         = "external-api-mock"
	externalMockHTTPPort     = int32(8080)
	externalMockHTTPSPort    = int32(8443)
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

	mockDNSIP, err := a.createExternalDependencyMock(ctx, namespace, req.ScanID)
	if err != nil {
		return SandboxResponse{}, err
	}

	log.Printf("[CreateSandbox] scan=%s creating pod %s/%s", req.ScanID, namespace, podName)
	if err := a.createPod(ctx, namespace, podName, req.ScanID, req.TargetImage, mockDNSIP); err != nil {
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
	if keepSandboxNamespaces() {
		log.Printf("[DestroySandbox] scan=%s retaining namespace %s because %s=true", scanID, namespace, retainSandboxEnv)
		return "RETAINED", nil
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

package deployer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
)

var (
	newK8sClient               = newKubernetesClient
	temporalDial               = client.Dial
	newWorker                  = worker.New
	temporalConnectMaxAttempts = 0
	temporalConnectRetryDelay  = 2 * time.Second
)

type SandboxRequest struct {
	ScanID      string `json:"scan_id"`
	TargetImage string `json:"target_image"`
}

type SandboxResponse struct {
	Namespace string `json:"namespace"`
	Endpoint  string `json:"endpoint"`
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

	w := newWorker(temporalClient, getenv("DEPLOYER_TASK_QUEUE", defaultTaskQueue), worker.Options{})
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
	if req.TargetImage == "" {
		return SandboxResponse{}, errors.New("target_image is required")
	}

	namespace := sandboxNamespace(req.ScanID)
	if err := validateSandboxNamespace(namespace); err != nil {
		return SandboxResponse{}, err
	}

	podName := "target-" + req.ScanID
	serviceName := "svc-" + req.ScanID

	if err := a.createNamespace(ctx, namespace, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}
	if err := a.createPod(ctx, namespace, podName, req.ScanID, req.TargetImage); err != nil {
		return SandboxResponse{}, err
	}
	if err := a.createService(ctx, namespace, serviceName, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}
	if err := a.waitForPodReady(ctx, namespace, podName, time.Minute); err != nil {
		return SandboxResponse{}, err
	}

	return SandboxResponse{
		Namespace: namespace,
		Endpoint:  fmt.Sprintf("http://%s.%s.svc.cluster.local:80", serviceName, namespace),
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

	err := a.k8s.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return "CLEANED", nil
	}
	if err != nil {
		return "", fmt.Errorf("delete namespace %s: %w", namespace, err)
	}
	return "CLEANED", nil
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
		return nil
	}
	if err != nil {
		return fmt.Errorf("create namespace %s: %w", namespace, err)
	}
	return nil
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
		return nil
	}
	if err != nil {
		return fmt.Errorf("create pod %s/%s: %w", namespace, podName, err)
	}
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
		return nil
	}
	if err != nil {
		return fmt.Errorf("create service %s/%s: %w", namespace, serviceName, err)
	}
	return nil
}

func (a *Activities) waitForPodReady(ctx context.Context, namespace, podName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		pod, err := a.k8s.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read pod %s/%s: %w", namespace, podName, err)
		}
		if err := checkImageErrors(pod); err != nil {
			return err
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
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

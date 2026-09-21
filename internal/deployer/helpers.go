package deployer

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"go.temporal.io/sdk/temporal"
)

func keepSandboxNamespaces() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(retainSandboxEnv)))
	return err == nil && enabled
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

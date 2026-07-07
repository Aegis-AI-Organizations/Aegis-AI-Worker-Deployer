package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (a *Activities) createSandboxDebugBundle(ctx context.Context, namespace, scanID string, response SandboxResponse, topology *SandboxTopology) error {
	topologyJSON := "{}"
	if topology != nil {
		if data, err := json.MarshalIndent(topology, "", "  "); err == nil {
			topologyJSON = string(data)
		}
	}
	responseJSON := "{}"
	if data, err := json.MarshalIndent(response, "", "  "); err == nil {
		responseJSON = string(data)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   sandboxDebugBundleName(scanID),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "aegis-worker-deployer", "aegis-scan": scanID},
		},
		Data: map[string]string{
			"created_at":     time.Now().UTC().Format(time.RFC3339),
			"namespace":      namespace,
			"sandbox.json":   responseJSON,
			"topology.json":  topologyJSON,
			"traffic_bundle": fmt.Sprintf("configmap/%s", externalMockTrafficConfigMapName(scanID)),
		},
	}
	_, err := a.k8s.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

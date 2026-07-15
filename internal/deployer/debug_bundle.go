package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
			"created_at":              time.Now().UTC().Format(time.RFC3339),
			"namespace":               namespace,
			"sandbox.json":            responseJSON,
			"topology.json":           topologyJSON,
			"mock_data_contract.json": sandboxMockDataContract(namespace, scanID, topology),
			"traffic_bundle":          fmt.Sprintf("configmap/%s", externalMockTrafficConfigMapName(scanID)),
		},
	}
	_, err := a.k8s.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func sandboxMockDataContract(namespace, scanID string, topology *SandboxTopology) string {
	contract := map[string]any{
		"version":   "2026-07-15",
		"scan_id":   scanID,
		"namespace": namespace,
		"principles": []string{
			"Use synthetic but business-realistic records.",
			"Never copy production secrets; use aegis-mock-secret and sk_test_aegis_mock.",
			"Keep generated data internally consistent across users, tokens, projects, and audit trails.",
		},
		"workloads":         []map[string]any{},
		"external_services": []map[string]any{},
		"database_schemas":  []map[string]any{},
	}
	if topology == nil {
		return marshalContract(contract)
	}
	workloads := topology.workloads()
	sort.SliceStable(workloads, func(i, j int) bool { return workloads[i].Name < workloads[j].Name })
	for _, workload := range workloads {
		contract["workloads"] = append(contract["workloads"].([]map[string]any), map[string]any{
			"name":         workload.Name,
			"image":        workload.Image,
			"ports":        workload.Ports,
			"stateful":     workload.Stateful,
			"depends_on":   workload.DependsOn,
			"config_files": len(workload.ConfigFiles),
			"secret_files": len(workload.SecretFiles),
			"empty_dirs":   len(workload.EmptyDirs),
		})
	}
	for _, mock := range topology.ExternalMocks {
		contract["external_services"] = append(contract["external_services"].([]map[string]any), map[string]any{
			"host":           mock.Host,
			"route_count":    len(mock.Routes),
			"capture":        mock.Capture,
			"traffic_bundle": fmt.Sprintf("configmap/%s", externalMockTrafficConfigMapName(scanID)),
		})
	}
	contract["database_schemas"] = sanitizeDatabaseSchemas(topology.DatabaseSchemas)
	return marshalContract(contract)
}

func marshalContract(contract map[string]any) string {
	data, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

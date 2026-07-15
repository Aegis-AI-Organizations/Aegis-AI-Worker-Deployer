package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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
			"kubernetes_state.json":   a.sandboxKubernetesState(ctx, namespace),
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

func (a *Activities) sandboxKubernetesState(ctx context.Context, namespace string) string {
	state := map[string]any{"namespace": namespace, "collected_at": time.Now().UTC().Format(time.RFC3339)}
	errors := map[string]string{}
	if services, err := a.k8s.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		state["services"] = summarizeServices(services.Items)
	} else {
		errors["services"] = err.Error()
	}
	if pods, err := a.k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		state["pods"] = summarizePods(pods.Items)
	} else {
		errors["pods"] = err.Error()
	}
	if deployments, err := a.k8s.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		state["deployments"] = summarizeDeployments(deployments.Items)
	} else {
		errors["deployments"] = err.Error()
	}
	if jobs, err := a.k8s.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		state["jobs"] = summarizeJobs(jobs.Items)
	} else {
		errors["jobs"] = err.Error()
	}
	if events, err := a.k8s.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		state["events"] = summarizeEvents(events.Items)
	} else {
		errors["events"] = err.Error()
	}
	if configMaps, err := a.k8s.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		state["configmaps"] = summarizeConfigMapNames(configMaps.Items)
	} else {
		errors["configmaps"] = err.Error()
	}
	if secrets, err := a.k8s.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		state["secrets"] = summarizeSecretNames(secrets.Items)
	} else {
		errors["secrets"] = err.Error()
	}
	if len(errors) > 0 {
		state["collection_errors"] = errors
	}
	return marshalContract(state)
}

func summarizeServices(services []corev1.Service) []map[string]any {
	items := make([]map[string]any, 0, len(services))
	for _, service := range services {
		ports := make([]map[string]any, 0, len(service.Spec.Ports))
		for _, port := range service.Spec.Ports {
			ports = append(ports, map[string]any{"name": port.Name, "port": port.Port, "target_port": port.TargetPort.String(), "protocol": port.Protocol})
		}
		items = append(items, map[string]any{"name": service.Name, "type": service.Spec.Type, "cluster_ip": service.Spec.ClusterIP, "ports": ports})
	}
	return items
}

func summarizePods(pods []corev1.Pod) []map[string]any {
	items := make([]map[string]any, 0, len(pods))
	for _, pod := range pods {
		containers := make([]map[string]any, 0, len(pod.Status.ContainerStatuses))
		for _, container := range pod.Status.ContainerStatuses {
			containers = append(containers, map[string]any{"name": container.Name, "ready": container.Ready, "restart_count": container.RestartCount, "image": container.Image})
		}
		items = append(items, map[string]any{"name": pod.Name, "phase": pod.Status.Phase, "pod_ip": pod.Status.PodIP, "node": pod.Spec.NodeName, "containers": containers})
	}
	return items
}

func summarizeDeployments(deployments []appsv1.Deployment) []map[string]any {
	items := make([]map[string]any, 0, len(deployments))
	for _, deployment := range deployments {
		items = append(items, map[string]any{"name": deployment.Name, "replicas": deployment.Status.Replicas, "ready_replicas": deployment.Status.ReadyReplicas, "available_replicas": deployment.Status.AvailableReplicas})
	}
	return items
}

func summarizeJobs(jobs []batchv1.Job) []map[string]any {
	items := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		conditions := make([]map[string]any, 0, len(job.Status.Conditions))
		for _, condition := range job.Status.Conditions {
			conditions = append(conditions, map[string]any{"type": condition.Type, "status": condition.Status, "reason": condition.Reason, "message": condition.Message})
		}
		items = append(items, map[string]any{"name": job.Name, "succeeded": job.Status.Succeeded, "failed": job.Status.Failed, "active": job.Status.Active, "conditions": conditions})
	}
	return items
}

func summarizeEvents(events []corev1.Event) []map[string]any {
	items := make([]map[string]any, 0, len(events))
	for _, event := range events {
		items = append(items, map[string]any{"type": event.Type, "reason": event.Reason, "object": strings.TrimSpace(event.InvolvedObject.Kind + "/" + event.InvolvedObject.Name), "message": event.Message})
	}
	return items
}

func summarizeConfigMapNames(configMaps []corev1.ConfigMap) []string {
	names := make([]string, 0, len(configMaps))
	for _, configMap := range configMaps {
		names = append(names, configMap.Name)
	}
	sort.Strings(names)
	return names
}

func summarizeSecretNames(secrets []corev1.Secret) []string {
	names := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		names = append(names, secret.Name)
	}
	sort.Strings(names)
	return names
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

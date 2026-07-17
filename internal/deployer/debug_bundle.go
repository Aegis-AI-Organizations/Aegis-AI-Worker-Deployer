package deployer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	readSandboxPodLog        = defaultReadSandboxPodLog
	uploadSandboxDebugBundle = uploadSandboxDebugBundleArchive
)

func (a *Activities) createSandboxDebugBundle(ctx context.Context, namespace, scanID string, response SandboxResponse, topology *SandboxTopology) (string, error) {
	files := a.collectSandboxDebugFiles(ctx, namespace, scanID, response, topology)
	archive, err := buildDebugBundleArchive(files)
	if err != nil {
		return "", err
	}
	debugRef := fmt.Sprintf("configmap/%s", sandboxDebugBundleName(scanID))
	if uploadedRef, err := uploadSandboxDebugBundle(ctx, scanID, archive); err == nil && strings.TrimSpace(uploadedRef) != "" {
		debugRef = uploadedRef
	} else if err != nil {
		log.Printf("[DebugBundle] scan=%s upload skipped: %v", scanID, err)
	}
	if err := a.createSandboxDebugConfigMap(ctx, namespace, scanID, debugRef, files); err != nil {
		return debugRef, err
	}
	return debugRef, nil
}

func (a *Activities) collectSandboxDebugFiles(ctx context.Context, namespace, scanID string, response SandboxResponse, topology *SandboxTopology) map[string][]byte {
	files := map[string][]byte{}
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
	files["metadata/created_at.txt"] = []byte(time.Now().UTC().Format(time.RFC3339) + "\n")
	files["metadata/namespace.txt"] = []byte(namespace + "\n")
	files["sandbox.json"] = []byte(responseJSON)
	files["topology.json"] = []byte(topologyJSON)
	files["kubernetes/state.json"] = []byte(a.sandboxKubernetesState(ctx, namespace))
	files["contracts/mock_data_contract.json"] = []byte(sandboxMockDataContract(namespace, scanID, topology))
	files["contracts/traffic_bundle.txt"] = []byte(fmt.Sprintf("configmap/%s\n", externalMockTrafficConfigMapName(scanID)))
	a.collectExistingDebugContracts(ctx, namespace, scanID, files)
	a.collectSandboxEvents(ctx, namespace, files)
	a.collectSandboxPodLogs(ctx, namespace, files)
	a.collectSandboxSeedErrors(ctx, namespace, files)
	return files
}

func (a *Activities) collectExistingDebugContracts(ctx context.Context, namespace, scanID string, files map[string][]byte) {
	configMap, err := a.k8s.CoreV1().ConfigMaps(namespace).Get(ctx, sandboxDebugBundleName(scanID), metav1.GetOptions{})
	if err != nil || configMap.Data == nil {
		return
	}
	for key, value := range configMap.Data {
		switch key {
		case "seed_contract.json":
			files["contracts/seed_contract.json"] = []byte(value)
		}
	}
}

func (a *Activities) createSandboxDebugConfigMap(ctx context.Context, namespace, scanID, debugRef string, files map[string][]byte) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   sandboxDebugBundleName(scanID),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "aegis-worker-deployer", "aegis-scan": scanID},
		},
		Data: map[string]string{
			"created_at":              strings.TrimSpace(string(files["metadata/created_at.txt"])),
			"namespace":               namespace,
			"debug_bundle":            debugRef,
			"sandbox.json":            string(files["sandbox.json"]),
			"topology.json":           string(files["topology.json"]),
			"kubernetes_state.json":   string(files["kubernetes/state.json"]),
			"kubernetes_events.json":  string(files["kubernetes/events.json"]),
			"mock_data_contract.json": string(files["contracts/mock_data_contract.json"]),
			"traffic_bundle":          fmt.Sprintf("configmap/%s", externalMockTrafficConfigMapName(scanID)),
		},
	}
	if seedContract, ok := files["contracts/seed_contract.json"]; ok {
		configMap.Data["seed_contract.json"] = string(seedContract)
	}
	_, err := a.k8s.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := a.k8s.CoreV1().ConfigMaps(namespace).Get(ctx, configMap.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if current.Data == nil {
			current.Data = map[string]string{}
		}
		for key, value := range configMap.Data {
			current.Data[key] = value
		}
		_, updateErr := a.k8s.CoreV1().ConfigMaps(namespace).Update(ctx, current, metav1.UpdateOptions{})
		return updateErr
	}
	return err
}

func (a *Activities) collectSandboxEvents(ctx context.Context, namespace string, files map[string][]byte) {
	events, err := a.k8s.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		files["kubernetes/events.error.txt"] = []byte(err.Error() + "\n")
		return
	}
	sort.SliceStable(events.Items, func(i, j int) bool {
		return events.Items[i].LastTimestamp.Time.Before(events.Items[j].LastTimestamp.Time)
	})
	data, err := json.MarshalIndent(events.Items, "", "  ")
	if err != nil {
		files["kubernetes/events.error.txt"] = []byte(err.Error() + "\n")
		return
	}
	files["kubernetes/events.json"] = data
}

func (a *Activities) collectSandboxPodLogs(ctx context.Context, namespace string, files map[string][]byte) {
	pods, err := a.k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		files["logs/pods.error.txt"] = []byte(err.Error() + "\n")
		return
	}
	sort.SliceStable(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for _, pod := range pods.Items {
		for _, container := range allPodContainerNames(pod) {
			path := filepath.ToSlash(filepath.Join("logs", "pods", pod.Name, container+".log"))
			data, err := safeReadSandboxPodLog(ctx, a.k8s, namespace, pod.Name, container, false)
			if err != nil {
				files[path+".error.txt"] = []byte(err.Error() + "\n")
			} else {
				files[path] = data
			}
			previousPath := filepath.ToSlash(filepath.Join("logs", "pods", pod.Name, container+".previous.log"))
			if data, err := safeReadSandboxPodLog(ctx, a.k8s, namespace, pod.Name, container, true); err == nil && len(data) > 0 {
				files[previousPath] = data
			}
		}
	}
}

func safeReadSandboxPodLog(ctx context.Context, k8s kubernetes.Interface, namespace, pod, container string, previous bool) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("read pod logs panic: %v", recovered)
		}
	}()
	return readSandboxPodLog(ctx, k8s, namespace, pod, container, previous)
}

func allPodContainerNames(pod corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers)+len(pod.Spec.EphemeralContainers))
	for _, container := range pod.Spec.InitContainers {
		names = append(names, container.Name)
	}
	for _, container := range pod.Spec.Containers {
		names = append(names, container.Name)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		names = append(names, container.Name)
	}
	sort.Strings(names)
	return names
}

func defaultReadSandboxPodLog(ctx context.Context, k8s kubernetes.Interface, namespace, pod, container string, previous bool) ([]byte, error) {
	return k8s.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous}).DoRaw(ctx)
}

func (a *Activities) collectSandboxSeedErrors(ctx context.Context, namespace string, files map[string][]byte) {
	jobs, err := a.k8s.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		files["seeding/errors.error.txt"] = []byte(err.Error() + "\n")
		return
	}
	seedErrors := make([]map[string]string, 0)
	for _, job := range jobs.Items {
		if !strings.HasPrefix(job.Name, "db-seed-") {
			continue
		}
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				seedErrors = append(seedErrors, map[string]string{
					"job":     job.Name,
					"reason":  condition.Reason,
					"message": condition.Message,
				})
			}
		}
	}
	data, err := json.MarshalIndent(seedErrors, "", "  ")
	if err != nil {
		files["seeding/errors.error.txt"] = []byte(err.Error() + "\n")
		return
	}
	files["seeding/errors.json"] = data
}

func buildDebugBundleArchive(files map[string][]byte) ([]byte, error) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, filepath.ToSlash(filepath.Clean(name)))
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			return nil, fmt.Errorf("invalid debug bundle path %q", name)
		}
		data := files[name]
		header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now().UTC()}
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := tarWriter.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func uploadSandboxDebugBundleArchive(ctx context.Context, scanID string, archive []byte) (string, error) {
	endpoint := strings.TrimSpace(os.Getenv("MINIO_ENDPOINT"))
	if endpoint == "" {
		return "", errors.New("MINIO_ENDPOINT is not configured")
	}
	bucket := strings.TrimSpace(os.Getenv("MINIO_DEBUG_BUCKET"))
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv("MINIO_BUCKET"))
	}
	if bucket == "" {
		bucket = "aegis-debug"
	}
	secure := strings.EqualFold(os.Getenv("MINIO_SECURE"), "true")
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("MINIO_ACCESS_KEY"), os.Getenv("MINIO_SECRET_KEY"), ""),
		Secure: secure,
	})
	if err != nil {
		return "", fmt.Errorf("create MinIO client: %w", err)
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return "", fmt.Errorf("check debug bucket %s: %w", bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return "", fmt.Errorf("create debug bucket %s: %w", bucket, err)
		}
	}
	object := fmt.Sprintf("debug-bundles/%s/%s.tar.gz", kubernetesName(scanID), sandboxDebugBundleName(scanID))
	_, err = client.PutObject(ctx, bucket, object, bytes.NewReader(archive), int64(len(archive)), minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return "", fmt.Errorf("upload debug bundle %s/%s: %w", bucket, object, err)
	}
	return fmt.Sprintf("s3://%s/%s", bucket, object), nil
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

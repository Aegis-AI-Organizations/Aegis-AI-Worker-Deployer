package deployer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

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
		if !ok || preferredStatus.Status == "skipped" {
			return SandboxResponse{
				Namespace: namespace,
				Workloads: statuses,
				Summary:   summarizeSandboxWorkloads(len(workloads), statuses, false),
			}, fmt.Errorf("preferred endpoint workload %q was not deployed", preferredEndpointWorkload)
		}
		preferredPorts := topologyPortsForWorkload(createdWorkloads, preferredEndpointWorkload)
		if len(preferredPorts) == 0 {
			return SandboxResponse{
				Namespace: namespace,
				Workloads: statuses,
				Summary:   summarizeSandboxWorkloads(len(workloads), statuses, false),
			}, fmt.Errorf("preferred endpoint workload %q does not expose any port", preferredEndpointWorkload)
		}
		if preferredStatus.Status != "ready" {
			log.Printf(
				"[CreateSandbox] scan=%s preferred endpoint workload %q is %s; exposing endpoint anyway: %s",
				scanID,
				preferredEndpointWorkload,
				preferredStatus.Status,
				strings.TrimSpace(preferredStatus.Error),
			)
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
		if shouldDropTopologyEnv(key) {
			continue
		}
		sanitizedValue, keep := replaceRedactedSecret(key, value, secret)
		if !keep {
			log.Printf("[CreateSandbox] dropping non-secret redacted env %q from workload %q", key, workload.Name)
			continue
		}
		sanitized[key] = sanitizedValue
	}
	workload.Env = sanitized
	return workload
}

func replaceRedactedSecret(key, value, secret string) (string, bool) {
	trimmedValue := strings.TrimSpace(value)
	if !strings.EqualFold(trimmedValue, "REDACTED") && !strings.Contains(trimmedValue, "<REDACTED") {
		return value, true
	}
	if !isSecretLikeEnvKey(key) {
		return "", false
	}
	return mockValueForEnvKey(key, secret), true
}

func isSecretLikeEnvKey(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	return strings.Contains(key, "AWS_ACCESS_KEY_ID") ||
		strings.Contains(key, "AWS_SECRET_ACCESS_KEY") ||
		strings.Contains(key, "API_KEY") ||
		strings.HasSuffix(key, "_KEY") ||
		strings.Contains(key, "TOKEN") ||
		strings.Contains(key, "PASS") ||
		strings.Contains(key, "PASSWORD") ||
		strings.Contains(key, "PWD") ||
		strings.Contains(key, "SECRET") ||
		strings.Contains(key, "PRIVATE_KEY")
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

func shouldDropTopologyEnv(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" || !isValidEnvName(key) {
		return true
	}
	upper := strings.ToUpper(key)
	switch upper {
	case "PATH", "HOME", "HOSTNAME":
		return true
	default:
		return false
	}
}

func isValidEnvName(key string) bool {
	for i, r := range key {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return key != ""
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
	runtimeClassName := a.sandboxRuntimeClassName(ctx)
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
					RuntimeClassName: runtimeClassName,
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
		if shouldDropTopologyEnv(key) {
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

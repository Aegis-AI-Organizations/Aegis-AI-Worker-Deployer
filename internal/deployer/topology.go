package deployer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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
	for i, mock := range t.ExternalMocks {
		if strings.TrimSpace(mock.Host) == "" {
			return fmt.Errorf("topology external_mock %d host is required", i)
		}
		for routeIndex, route := range mock.Routes {
			if route.Status < 0 || route.Status > 599 {
				return fmt.Errorf("topology external_mock %q route %d status is invalid", mock.Host, routeIndex)
			}
		}
	}
	for i, schema := range t.DatabaseSchemas {
		if strings.TrimSpace(schema.Engine) == "" {
			return fmt.Errorf("topology database_schema %d engine is required", i)
		}
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
		for _, file := range append(workload.ConfigFiles, workload.SecretFiles...) {
			if strings.TrimSpace(file.Path) == "" {
				return fmt.Errorf("topology workload %q contains file without path", name)
			}
		}
		for _, volume := range workload.EmptyDirs {
			if kubernetesName(volume.Name) == "" || strings.TrimSpace(volume.MountPath) == "" {
				return fmt.Errorf("topology workload %q contains invalid empty_dir", name)
			}
		}
	}
	if _, err := orderedTopologyWorkloads(workloads); err != nil {
		return err
	}
	return nil
}

func (t *SandboxTopology) UnmarshalJSON(data []byte) error {
	var alias sandboxTopologyAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*t = SandboxTopology{
		Services:        alias.Services,
		Deployments:     alias.Deployments,
		Containers:      alias.Containers,
		Connections:     alias.Connections,
		Routes:          alias.Routes,
		ExternalMocks:   append(alias.ExternalMocks, alias.ExternalMocksCamel...),
		DatabaseSchemas: append(alias.DatabaseSchemas, alias.DatabaseSchemasCamel...),
	}
	return nil
}

func (s *DatabaseSchema) UnmarshalJSON(data []byte) error {
	var alias databaseSchemaAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*s = DatabaseSchema{
		Engine:              strings.TrimSpace(alias.Engine),
		Host:                strings.TrimSpace(alias.Host),
		Port:                alias.Port,
		DatabaseName:        firstNonEmpty(alias.DatabaseName, alias.DatabaseNameCamel),
		Username:            strings.TrimSpace(alias.Username),
		Password:            alias.Password,
		SourceContainerID:   firstNonEmpty(alias.SourceContainerID, alias.SourceContainerIDCamel),
		SourceContainerName: firstNonEmpty(alias.SourceContainerName, alias.SourceContainerNameCamel),
		Tables:              alias.Tables,
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

func orderedTopologyWorkloads(workloads []TopologyWorkload) ([]TopologyWorkload, error) {
	byName := make(map[string]TopologyWorkload, len(workloads))
	for _, workload := range workloads {
		byName[kubernetesName(workload.Name)] = workload
	}

	ordered := make([]TopologyWorkload, 0, len(workloads))
	state := make(map[string]int, len(workloads))
	var visit func(TopologyWorkload) error
	visit = func(workload TopologyWorkload) error {
		name := kubernetesName(workload.Name)
		switch state[name] {
		case 1:
			return fmt.Errorf("topology dependency cycle detected at workload %q", name)
		case 2:
			return nil
		}

		state[name] = 1
		for _, dependency := range normalizeTopologyDependencies(workload.DependsOn) {
			dependencyWorkload, ok := byName[dependency]
			if !ok {
				return fmt.Errorf("topology workload %q depends on unknown workload %q", name, dependency)
			}
			if dependency == name {
				return fmt.Errorf("topology workload %q cannot depend on itself", name)
			}
			if err := visit(dependencyWorkload); err != nil {
				return err
			}
		}
		state[name] = 2
		ordered = append(ordered, workload)
		return nil
	}

	for _, workload := range workloads {
		if err := visit(workload); err != nil {
			return nil, err
		}
	}
	return ordered, nil
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
		ID:                 strings.TrimSpace(alias.ID),
		Name:               strings.TrimSpace(alias.Name),
		Image:              strings.TrimSpace(alias.Image),
		ImageArchiveRef:    firstNonEmpty(alias.ImageArchiveRef, alias.ImageArchiveRefCamel),
		ImageArchiveObject: firstNonEmpty(alias.ImageArchiveObject, alias.ImageArchiveObjectCamel),
		Ports:              alias.Ports,
		Env:                env,
		Replicas:           alias.Replicas,
		Liveness:           liveness,
		DependsOn:          normalizeTopologyDependencies(alias.DependsOn),
		WaitFor:            normalizeTopologyWaitFor(alias.WaitFor),
		Required:           alias.Required,
		Command:            alias.Command,
		Args:               alias.Args,
		WorkingDir:         strings.TrimSpace(alias.WorkingDir),
		InitContainers:     alias.InitContainers,
		ConfigFiles:        alias.ConfigFiles,
		SecretFiles:        alias.SecretFiles,
		EmptyDirs:          alias.EmptyDirs,
		Resources:          alias.Resources,
		SecurityContext:    alias.SecurityContext,
		PodSecurityContext: alias.PodSecurityContext,
		Stateful:           alias.Stateful,
		Service:            alias.Service,
	}
	return nil
}

func normalizeTopologyDependencies(values []string) []string {
	dependencies := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		dependency := kubernetesName(value)
		if dependency == "" {
			continue
		}
		if _, ok := seen[dependency]; ok {
			continue
		}
		seen[dependency] = struct{}{}
		dependencies = append(dependencies, dependency)
	}
	return dependencies
}

func normalizeTopologyWaitFor(values []string) []string {
	targets := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		target := strings.TrimSpace(value)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	return targets
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
	orderedWorkloads, err := orderedTopologyWorkloads(workloads)
	if err != nil {
		return SandboxResponse{}, err
	}
	log.Printf("[CreateSandbox] scan=%s deploying topology with %d workload(s)", scanID, len(workloads))
	if len(workloads) == 0 {
		return SandboxResponse{}, errors.New("topology does not contain any workload")
	}
	mockDNSIP, err := a.createExternalDependencyMock(ctx, namespace, scanID, topology.ExternalMocks)
	if err != nil {
		return SandboxResponse{}, err
	}

	preferredEndpointWorkload = strings.TrimSpace(strings.ToLower(preferredEndpointWorkload))
	if preferredEndpointWorkload != "" {
		preferredEndpointWorkload = kubernetesName(preferredEndpointWorkload)
	}
	mockSecret := mockTopologySecret(scanID)
	warnAboutLocalImagesWithoutArchives(scanID, orderedWorkloads)
	if err := importTopologyImageArchives(ctx, orderedWorkloads); err != nil {
		return SandboxResponse{}, err
	}
	firstServiceName := kubernetesName(workloads[0].Name)
	firstReadyServiceName := ""
	firstReadyServicePort := int32(0)
	createdWorkloads := make([]TopologyWorkload, 0, len(workloads))
	statuses := make([]SandboxWorkloadStatus, 0, len(workloads))
	for index, workload := range orderedWorkloads {
		workload = sanitizeTopologySecrets(workload, mockSecret, orderedWorkloads)
		name := kubernetesName(workload.Name)

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
	if err := a.createTopologyNetworkPolicies(ctx, namespace, scanID, topology, createdWorkloads); err != nil {
		return SandboxResponse{
			Namespace: namespace,
			Workloads: statuses,
			Summary:   summarizeSandboxWorkloads(len(workloads), statuses, false),
		}, err
	}

	for _, workload := range createdWorkloads {
		name := kubernetesName(workload.Name)
		ports := workload.normalizedPorts()
		if err := a.waitForTopologyWorkloadReady(ctx, namespace, name, workload, topologyDeploymentReadyTimeout); err != nil {
			log.Printf("[CreateSandbox] deployment %s/%s is not ready; continuing topology deployment: %v", namespace, name, err)
			updateWorkloadStatus(statuses, name, "not_ready", "", err.Error())
			if workload.required() {
				return SandboxResponse{
					Namespace: namespace,
					Workloads: statuses,
					Summary:   summarizeSandboxWorkloads(len(workloads), statuses, false),
				}, fmt.Errorf("required topology workload %q is not ready: %w", name, err)
			}
			continue
		}
		endpoint := workloadEndpoint(namespace, name, ports)
		updateWorkloadStatus(statuses, name, "ready", endpoint, "")
	}

	for _, workload := range workloads {
		name := kubernetesName(workload.Name)
		status, ok := findWorkloadStatus(statuses, name)
		ports := workload.normalizedPorts()
		if ok && status.Status == "ready" && len(ports) > 0 {
			firstReadyServiceName = name
			firstReadyServicePort = ports[0].servicePort()
			break
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
		if preferredStatus.Status == "ready" {
			endpointServiceName = preferredEndpointWorkload
			endpointPort = preferredPorts[0].servicePort()
		} else if endpointServiceName != "" {
			log.Printf(
				"[CreateSandbox] scan=%s preferred endpoint workload %q is %s; falling back to ready workload %q: %s",
				scanID,
				preferredEndpointWorkload,
				preferredStatus.Status,
				endpointServiceName,
				strings.TrimSpace(preferredStatus.Error),
			)
		} else {
			log.Printf(
				"[CreateSandbox] scan=%s preferred endpoint workload %q is %s and no ready fallback exists; exposing endpoint anyway: %s",
				scanID,
				preferredEndpointWorkload,
				preferredStatus.Status,
				strings.TrimSpace(preferredStatus.Error),
			)
			endpointServiceName = preferredEndpointWorkload
			endpointPort = preferredPorts[0].servicePort()
		}
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

func sanitizeTopologySecrets(workload TopologyWorkload, secret string, workloads []TopologyWorkload) TopologyWorkload {
	if len(workload.Env) == 0 {
		return workload
	}

	sanitized := make(map[string]string, len(workload.Env))
	for key, value := range workload.Env {
		key = strings.TrimSpace(key)
		if shouldDropTopologyEnv(key) {
			continue
		}
		sanitizedValue, keep := replaceRedactedSecret(key, value, secret, workload, workloads)
		if !keep {
			log.Printf("[CreateSandbox] dropping non-secret redacted env %q from workload %q", key, workload.Name)
			continue
		}
		sanitizedValue = normalizeFunctionalEnvValue(key, sanitizedValue, workload, workloads)
		sanitized[key] = sanitizedValue
	}
	workload.Env = sanitized
	return workload
}

func normalizeFunctionalEnvValue(key, value string, workload TopologyWorkload, workloads []TopologyWorkload) string {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if strings.Contains(upper, "DATABASE_URL") {
		return normalizeDependencyURL(value, workload, workloads, []string{"db", "postgres"}, 5432)
	}
	if strings.Contains(upper, "REDIS_URL") {
		return normalizeDependencyURL(value, workload, workloads, []string{"redis"}, 6379)
	}
	if strings.Contains(upper, "BACKEND_URL") || strings.Contains(upper, "API_URL") {
		return normalizeDependencyURL(value, workload, workloads, []string{"backend", "api"}, 8080)
	}
	if strings.Contains(upper, "DB_HOST") || strings.HasSuffix(upper, "DATABASE_HOST") {
		return normalizeDependencyHost(value, workload, workloads, "db", "postgres")
	}
	if strings.Contains(upper, "REDIS_HOST") {
		return normalizeDependencyHost(value, workload, workloads, "redis")
	}
	return value
}

func normalizeDependencyURL(value string, workload TopologyWorkload, workloads []TopologyWorkload, roles []string, fallbackPort int32) string {
	trimmed := strings.TrimSpace(value)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return value
	}
	host := parsed.Hostname()
	normalizedHost := normalizeDependencyHost(host, workload, workloads, roles...)
	if normalizedHost == host {
		return value
	}
	port := parsed.Port()
	if port == "" {
		port = fmt.Sprintf("%d", dependencyPort(workloads, normalizedHost, fallbackPort))
	}
	parsed.Host = normalizedHost + ":" + port
	return parsed.String()
}

func normalizeDependencyHost(value string, workload TopologyWorkload, workloads []TopologyWorkload, roles ...string) string {
	host := strings.TrimSpace(value)
	if host == "" {
		return value
	}
	host = strings.Split(host, ":")[0]
	if topologyHasWorkload(workloads, host) {
		return value
	}
	if !isGenericDependencyHost(host, roles...) {
		return value
	}
	preferred := preferredDependencyHost(workload, workloads, roles...)
	if preferred == "" || preferred == kubernetesName(workload.Name) {
		return value
	}
	return preferred
}

func topologyHasWorkload(workloads []TopologyWorkload, name string) bool {
	name = kubernetesName(name)
	for _, workload := range workloads {
		if kubernetesName(workload.Name) == name {
			return true
		}
	}
	return false
}

func isGenericDependencyHost(host string, roles ...string) bool {
	host = kubernetesName(strings.TrimSpace(host))
	if host == "localhost" || host == "127-0-0-1" || host == "0-0-0-0" {
		return true
	}
	for _, role := range roles {
		role = kubernetesName(role)
		if host == role || host == role+"-host" || host == role+"-service" {
			return true
		}
	}
	return false
}

func replaceRedactedSecret(key, value, secret string, workload TopologyWorkload, workloads []TopologyWorkload) (string, bool) {
	trimmedValue := strings.TrimSpace(value)
	if !strings.EqualFold(trimmedValue, "REDACTED") && !strings.Contains(trimmedValue, "<REDACTED") {
		return value, true
	}
	if !isSecretLikeEnvKey(key) {
		return mockFunctionalEnvValue(key, workload, workloads), true
	}
	return mockValueForEnvKey(key, secret), true
}

func mockFunctionalEnvValue(key string, workload TopologyWorkload, workloads []TopologyWorkload) string {
	upper := strings.ToUpper(strings.TrimSpace(key))
	ports := workload.normalizedPorts()
	if isBooleanLikeEnvKey(upper) {
		return "false"
	}
	if upper == "NODE_ENV" {
		return "production"
	}
	if upper == "LOG_LEVEL" {
		return "info"
	}
	if upper == "PGSSLMODE" {
		return "disable"
	}
	if upper == "LANG" || strings.HasPrefix(upper, "LC_") {
		return "C.UTF-8"
	}
	if strings.Contains(upper, "POOL_MAX") || strings.Contains(upper, "POOL_MIN") || strings.Contains(upper, "CONCURRENCY") {
		return "1"
	}
	if strings.Contains(upper, "UPLOAD_MAX_SIZE") {
		return "1048576"
	}
	if strings.Contains(upper, "DATABASE_URL") {
		host := preferredDependencyHost(workload, workloads, "db", "postgres")
		return fmt.Sprintf("postgres://postgres:aegis-mock-secret@%s:5432/postgres", host)
	}
	if strings.Contains(upper, "REDIS_URL") {
		host := preferredDependencyHost(workload, workloads, "redis")
		return fmt.Sprintf("redis://%s:6379", host)
	}
	if strings.Contains(upper, "DB_HOST") || strings.HasSuffix(upper, "DATABASE_HOST") {
		return preferredDependencyHost(workload, workloads, "db", "postgres")
	}
	if strings.Contains(upper, "REDIS_HOST") {
		return preferredDependencyHost(workload, workloads, "redis")
	}
	if strings.Contains(upper, "BACKEND_URL") || strings.Contains(upper, "API_URL") {
		host := preferredDependencyHost(workload, workloads, "backend", "api")
		return fmt.Sprintf("http://%s:%d", host, dependencyPort(workloads, host, 8080))
	}
	if strings.Contains(upper, "FRONTEND_URL") || upper == "URL" || strings.Contains(upper, "PUBLIC_URL") {
		port := int32(80)
		if len(ports) > 0 {
			port = ports[0].servicePort()
		}
		return fmt.Sprintf("http://%s:%d", kubernetesName(workload.Name), port)
	}
	if strings.HasSuffix(upper, "_PORT") || upper == "PORT" {
		if strings.Contains(upper, "DB_") || strings.Contains(upper, "POSTGRES") || strings.Contains(upper, "DATABASE") {
			return "5432"
		}
		if strings.Contains(upper, "REDIS") {
			return "6379"
		}
		if strings.Contains(upper, "BACKEND") || strings.Contains(upper, "API") {
			host := preferredDependencyHost(workload, workloads, "backend", "api")
			return fmt.Sprintf("%d", dependencyPort(workloads, host, 8080))
		}
		if len(ports) > 0 {
			return fmt.Sprintf("%d", ports[0].servicePort())
		}
		return "80"
	}
	return "aegis-mock-value"
}

func isBooleanLikeEnvKey(upper string) bool {
	return upper == "DEBUG" ||
		strings.HasPrefix(upper, "ENABLE_") ||
		strings.HasSuffix(upper, "_ENABLED") ||
		strings.HasPrefix(upper, "DISABLE_") ||
		strings.HasSuffix(upper, "_DISABLED") ||
		strings.HasPrefix(upper, "FORCE_") ||
		strings.HasSuffix(upper, "_SECURE") ||
		strings.HasSuffix(upper, "_TLS") ||
		strings.HasSuffix(upper, "_SSL") ||
		strings.HasSuffix(upper, "_MONITOR_ONLY") ||
		strings.HasSuffix(upper, "_ROLLING_RESTART")
}

func preferredDependencyHost(workload TopologyWorkload, workloads []TopologyWorkload, roles ...string) string {
	prefix := workloadNamePrefix(workload.Name)
	var fallback string
	for _, role := range roles {
		role = kubernetesName(role)
		for _, candidate := range workloads {
			name := kubernetesName(candidate.Name)
			if name == "" || name == kubernetesName(workload.Name) {
				continue
			}
			if fallback == "" && strings.Contains(name, role) {
				fallback = name
			}
			if prefix != "" && strings.HasPrefix(name, prefix+"-") && strings.Contains(name, role) {
				return name
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return kubernetesName(workload.Name)
}

func workloadNamePrefix(name string) string {
	name = kubernetesName(name)
	for _, suffix := range []string{"-frontend", "-backend", "-server", "-api", "-db", "-postgres", "-redis"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return ""
}

func dependencyPort(workloads []TopologyWorkload, name string, fallback int32) int32 {
	for _, workload := range workloads {
		if kubernetesName(workload.Name) != kubernetesName(name) {
			continue
		}
		ports := workload.normalizedPorts()
		if len(ports) > 0 {
			return ports[0].servicePort()
		}
	}
	return fallback
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
	case "PATH", "HOME", "HOSTNAME", "PGDATA":
		return true
	default:
		return isImageRuntimeEnvKey(upper)
	}
}

func isImageRuntimeEnvKey(upper string) bool {
	if strings.HasSuffix(upper, "_VERSION") || strings.HasSuffix(upper, "_RELEASE") {
		return true
	}
	switch upper {
	case "PG_MAJOR", "PG_VERSION", "GOSU_VERSION", "NODE_VERSION", "YARN_VERSION", "NGINX_VERSION", "NJS_VERSION", "NJS_RELEASE", "PKG_RELEASE", "DYNPKG_RELEASE", "ACME_VERSION", "REDIS_VERSION":
		return true
	default:
		return false
	}
}

func warnAboutLocalImagesWithoutArchives(scanID string, workloads []TopologyWorkload) {
	for _, workload := range workloads {
		image := strings.TrimSpace(workload.Image)
		if image == "" || !looksLikeLocalImageReference(image) {
			continue
		}
		if strings.TrimSpace(firstNonEmpty(workload.ImageArchiveRef, workload.ImageArchiveObject)) != "" {
			continue
		}
		log.Printf(
			"[CreateSandbox] scan=%s workload=%s image=%s looks local but has no image archive metadata; pod may enter ImagePullBackOff unless the image already exists on the cluster node",
			scanID,
			kubernetesName(workload.Name),
			image,
		)
	}
}

func looksLikeLocalImageReference(image string) bool {
	image = strings.TrimSpace(image)
	if image == "" || strings.Contains(image, "@sha256:") {
		return false
	}
	if strings.Contains(image, "/") {
		return false
	}
	repository := strings.Split(image, ":")[0]
	if strings.Contains(repository, ".") {
		return false
	}
	switch repository {
	case "alpine", "busybox", "debian", "ubuntu", "nginx", "postgres", "mysql", "mariadb", "redis", "mongo", "node", "python", "golang", "httpd":
		return false
	default:
		return true
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

func importTopologyImageArchives(ctx context.Context, workloads []TopologyWorkload) error {
	imported := map[string]struct{}{}
	for _, workload := range workloads {
		archiveRef := strings.TrimSpace(firstNonEmpty(workload.ImageArchiveRef, workload.ImageArchiveObject))
		if archiveRef == "" {
			continue
		}
		if _, ok := imported[archiveRef]; ok {
			continue
		}
		if err := importTopologyImageArchive(ctx, workload); err != nil {
			return err
		}
		imported[archiveRef] = struct{}{}
	}
	return nil
}

func importTopologyImageArchive(ctx context.Context, workload TopologyWorkload) error {
	objectRef := strings.TrimSpace(firstNonEmpty(workload.ImageArchiveRef, workload.ImageArchiveObject))
	if objectRef == "" {
		return nil
	}
	file, err := os.CreateTemp("", "aegis-image-*.tar")
	if err != nil {
		return fmt.Errorf("create image archive temp file: %w", err)
	}
	archivePath := file.Name()
	closed := false
	defer func() {
		if !closed {
			if err := file.Close(); err != nil {
				log.Printf("[CreateSandbox] close image archive temp file %s: %v", archivePath, err)
			}
		}
		if err := os.Remove(archivePath); err != nil {
			log.Printf("[CreateSandbox] remove image archive temp file %s: %v", archivePath, err)
		}
	}()

	if err := downloadImageArchive(ctx, objectRef, file); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write image archive %s: %w", objectRef, err)
	}
	closed = true

	command := strings.TrimSpace(os.Getenv("AEGIS_IMAGE_IMPORT_COMMAND"))
	if command == "" {
		command = "docker load -i {archive}"
	}
	command = strings.ReplaceAll(command, "{archive}", archivePath)
	command = strings.ReplaceAll(command, "{image}", strings.TrimSpace(workload.Image))
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("import image archive for %s: %w: %s", workload.Image, err, strings.TrimSpace(string(output)))
	}
	log.Printf("[CreateSandbox] imported image archive for workload=%s image=%s", workload.Name, workload.Image)
	return nil
}

func downloadImageArchive(ctx context.Context, ref string, file *os.File) error {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return fmt.Errorf("build image archive request: %w", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return fmt.Errorf("download image archive %s: %w", ref, err)
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				log.Printf("[CreateSandbox] close image archive response body: %v", err)
			}
		}()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return fmt.Errorf("download image archive %s returned HTTP %d", ref, response.StatusCode)
		}
		_, err = io.Copy(file, response.Body)
		return err
	}

	bucket, object, err := imageArchiveObject(ref)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSpace(os.Getenv("MINIO_ENDPOINT"))
	if endpoint == "" {
		return errors.New("MINIO_ENDPOINT is required to import image archives")
	}
	secure := strings.EqualFold(os.Getenv("MINIO_SECURE"), "true")
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(
			os.Getenv("MINIO_ACCESS_KEY"),
			os.Getenv("MINIO_SECRET_KEY"),
			"",
		),
		Secure: secure,
	})
	if err != nil {
		return fmt.Errorf("create MinIO client: %w", err)
	}
	objectReader, err := client.GetObject(ctx, bucket, object, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("download image archive %s: %w", ref, err)
	}
	defer func() {
		if err := objectReader.Close(); err != nil {
			log.Printf("[CreateSandbox] close image archive object reader: %v", err)
		}
	}()
	if _, err := io.Copy(file, objectReader); err != nil {
		return fmt.Errorf("write image archive %s: %w", ref, err)
	}
	return nil
}

func imageArchiveObject(ref string) (string, string, error) {
	ref = strings.TrimSpace(ref)
	bucket := strings.TrimSpace(os.Getenv("MINIO_INGEST_BUCKET"))
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv("MINIO_BUCKET"))
	}
	if bucket == "" {
		bucket = "aegis-ingest"
	}
	object := ref
	if strings.HasPrefix(ref, "minio://") || strings.HasPrefix(ref, "s3://") {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(ref, "minio://"), "s3://")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return "", "", fmt.Errorf("invalid image archive reference %q", ref)
		}
		bucket = strings.TrimSpace(parts[0])
		object = strings.TrimSpace(parts[1])
	} else if strings.HasPrefix(ref, "minio:") {
		object = strings.TrimLeft(strings.TrimSpace(strings.TrimPrefix(ref, "minio:")), "/")
	}
	if strings.TrimSpace(object) == "" {
		return "", "", fmt.Errorf("invalid image archive reference %q", ref)
	}
	return bucket, object, nil
}

func (a *Activities) createDeployment(ctx context.Context, namespace, scanID, name string, workload TopologyWorkload, mockDNSIP string) error {
	replicas := int32(1)
	if workload.Replicas != nil && *workload.Replicas > 0 {
		replicas = *workload.Replicas
	}
	if err := a.createWorkloadFileResources(ctx, namespace, name, workload); err != nil {
		return err
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
	podSpec := corev1.PodSpec{
		RuntimeClassName: runtimeClassName,
		DNSPolicy:        dnsPolicy,
		DNSConfig:        dnsConfig,
		SecurityContext:  workload.PodSecurityContext,
		InitContainers:   workload.initContainers(),
		Volumes:          workload.volumes(name),
		Containers: []corev1.Container{{
			Name:            name,
			Image:           strings.TrimSpace(workload.Image),
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         workload.Command,
			Args:            workload.Args,
			WorkingDir:      workload.WorkingDir,
			Ports:           containerPorts,
			Env:             workload.envVars(),
			Resources:       workload.Resources,
			SecurityContext: workload.SecurityContext,
			VolumeMounts:    workload.volumeMounts(name),
			ReadinessProbe:  readinessProbe,
			LivenessProbe:   livenessProbe,
		}},
	}
	if len(workload.WaitFor) > 0 {
		podSpec.InitContainers = append(workload.waitForInitContainers(), podSpec.InitContainers...)
	}
	if workload.Stateful {
		return a.createStatefulSet(ctx, namespace, scanID, name, labels, replicas, podSpec, workload)
	}
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
				Spec: podSpec,
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

func (a *Activities) createStatefulSet(ctx context.Context, namespace, scanID, name string, labels map[string]string, replicas int32, podSpec corev1.PodSpec, workload TopologyWorkload) error {
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: podSpec,
			},
		},
	}

	_, err := a.k8s.AppsV1().StatefulSets(namespace).Create(ctx, statefulSet, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] statefulset %s/%s already exists", namespace, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create statefulset %s/%s: %w", namespace, name, err)
	}
	log.Printf("[CreateSandbox] statefulset %s/%s created replicas=%d image=%s", namespace, name, replicas, workload.Image)
	return nil
}

func (a *Activities) createWorkloadFileResources(ctx context.Context, namespace, name string, workload TopologyWorkload) error {
	if len(workload.ConfigFiles) > 0 {
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: workloadConfigMapName(name)},
			Data:       map[string]string{},
		}
		for _, file := range workload.ConfigFiles {
			configMap.Data[fileKey(file.Path)] = file.Content
		}
		if _, err := a.k8s.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create configmap %s/%s: %w", namespace, configMap.Name, err)
		}
	}
	if len(workload.SecretFiles) > 0 {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: workloadSecretName(name)},
			StringData: map[string]string{},
		}
		for _, file := range workload.SecretFiles {
			secret.StringData[fileKey(file.Path)] = file.Content
		}
		if _, err := a.k8s.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create secret %s/%s: %w", namespace, secret.Name, err)
		}
	}
	return nil
}

func (w TopologyWorkload) initContainers() []corev1.Container {
	containers := make([]corev1.Container, 0, len(w.InitContainers))
	for _, init := range w.InitContainers {
		name := kubernetesName(init.Name)
		if name == "" {
			name = "init"
		}
		containers = append(containers, corev1.Container{
			Name:            name,
			Image:           strings.TrimSpace(init.Image),
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         init.Command,
			Args:            init.Args,
			WorkingDir:      strings.TrimSpace(init.WorkingDir),
			Env:             topologyEnvVars(init.Env),
			VolumeMounts:    w.volumeMounts(kubernetesName(w.Name)),
		})
	}
	return containers
}

func (w TopologyWorkload) waitForInitContainers() []corev1.Container {
	containers := make([]corev1.Container, 0, len(w.WaitFor))
	for _, target := range w.WaitFor {
		parts := strings.Split(target, ":")
		host := kubernetesName(parts[0])
		port := "80"
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			port = strings.TrimSpace(parts[1])
		}
		containers = append(containers, corev1.Container{
			Name:            "wait-for-" + host,
			Image:           "busybox:1.36",
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c"},
			Args:            []string{fmt.Sprintf("until nc -z %s %s; do sleep 2; done", host, port)},
		})
	}
	return containers
}

func (w TopologyWorkload) volumes(name string) []corev1.Volume {
	volumes := []corev1.Volume{}
	if len(w.ConfigFiles) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: workloadConfigVolumeName(name),
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: workloadConfigMapName(name)},
			}},
		})
	}
	if len(w.SecretFiles) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: workloadSecretVolumeName(name),
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: workloadSecretName(name),
			}},
		})
	}
	for _, emptyDir := range w.EmptyDirs {
		volumes = append(volumes, corev1.Volume{
			Name:         kubernetesName(emptyDir.Name),
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return volumes
}

func (w TopologyWorkload) volumeMounts(name string) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{}
	for _, file := range w.ConfigFiles {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      workloadConfigVolumeName(name),
			MountPath: strings.TrimSpace(file.Path),
			SubPath:   fileKey(file.Path),
			ReadOnly:  true,
		})
	}
	for _, file := range w.SecretFiles {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      workloadSecretVolumeName(name),
			MountPath: strings.TrimSpace(file.Path),
			SubPath:   fileKey(file.Path),
			ReadOnly:  true,
		})
	}
	for _, emptyDir := range w.EmptyDirs {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      kubernetesName(emptyDir.Name),
			MountPath: strings.TrimSpace(emptyDir.MountPath),
		})
	}
	return mounts
}

func fileKey(filePath string) string {
	key := path.Base(strings.TrimSpace(filePath))
	if key == "." || key == "/" || key == "" {
		return "file"
	}
	return key
}

func workloadConfigMapName(name string) string    { return name + "-config" }
func workloadSecretName(name string) string       { return name + "-secret" }
func workloadConfigVolumeName(name string) string { return name + "-config" }
func workloadSecretVolumeName(name string) string { return name + "-secret" }

func (w TopologyWorkload) required() bool {
	return w.Required != nil && *w.Required
}

func (a *Activities) waitForTopologyWorkloadReady(ctx context.Context, namespace, name string, workload TopologyWorkload, timeout time.Duration) error {
	if workload.Stateful {
		return a.waitForStatefulSetReady(ctx, namespace, name, timeout)
	}
	return a.waitForDeploymentReady(ctx, namespace, name, timeout)
}

func (a *Activities) waitForStatefulSetReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	log.Printf("[CreateSandbox] waiting for statefulset %s/%s readiness (timeout=%s)", namespace, name, timeout)
	lastSummary := ""
	for {
		statefulSet, err := a.k8s.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read statefulset %s/%s: %w", namespace, name, err)
		}

		expected := int32(1)
		if statefulSet.Spec.Replicas != nil {
			expected = *statefulSet.Spec.Replicas
		}
		summary := fmt.Sprintf(
			"replicas=%d ready=%d updated=%d observed_generation=%d generation=%d",
			expected,
			statefulSet.Status.ReadyReplicas,
			statefulSet.Status.UpdatedReplicas,
			statefulSet.Status.ObservedGeneration,
			statefulSet.Generation,
		)
		if summary != lastSummary {
			log.Printf("[CreateSandbox] statefulset %s/%s status=%s", namespace, name, summary)
			lastSummary = summary
		}
		if statefulSet.Status.ObservedGeneration >= statefulSet.Generation && statefulSet.Status.ReadyReplicas >= expected {
			log.Printf("[CreateSandbox] statefulset %s/%s is Ready", namespace, name)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for statefulset %s/%s to become ready", namespace, name)
		case <-ticker.C:
		}
	}
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
	serviceType := corev1.ServiceTypeClusterIP
	switch strings.ToLower(strings.TrimSpace(workload.Service.Type)) {
	case "nodeport":
		serviceType = corev1.ServiceTypeNodePort
	case "loadbalancer", "load_balancer":
		serviceType = corev1.ServiceTypeLoadBalancer
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: topologyLabels(scanID, name),
		},
		Spec: corev1.ServiceSpec{
			Type:     serviceType,
			Selector: topologyLabels(scanID, name),
			Ports:    workload.servicePorts(),
		},
	}
	if workload.Service.Headless {
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.ClusterIP = corev1.ClusterIPNone
	}

	if err := a.createServiceObject(ctx, namespace, service); err != nil {
		return err
	}
	for _, alias := range workload.Service.Aliases {
		aliasName := kubernetesName(alias)
		if aliasName == "" || aliasName == name {
			continue
		}
		aliasService := service.DeepCopy()
		aliasService.Name = aliasName
		aliasService.ResourceVersion = ""
		aliasService.Spec.ClusterIP = ""
		if workload.Service.Headless {
			aliasService.Spec.ClusterIP = corev1.ClusterIPNone
		}
		if err := a.createServiceObject(ctx, namespace, aliasService); err != nil {
			return err
		}
	}
	log.Printf("[CreateSandbox] service %s/%s created with %d port(s)", namespace, name, len(service.Spec.Ports))
	return nil
}

func (a *Activities) createServiceObject(ctx context.Context, namespace string, service *corev1.Service) error {
	_, err := a.k8s.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] service %s/%s already exists", namespace, service.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create service %s/%s: %w", namespace, service.Name, err)
	}
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
	return topologyEnvVars(w.Env)
}

func topologyEnvVars(values map[string]string) []corev1.EnvVar {
	if len(values) == 0 {
		return nil
	}
	envVars := make([]corev1.EnvVar, 0, len(values))
	for key, value := range values {
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

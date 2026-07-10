package deployer

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

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
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: externalMockLabels(scanID),
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: ptrProtocol(corev1.ProtocolTCP), Port: ptrIntOrString(intstr.FromInt32(80))},
						{Protocol: ptrProtocol(corev1.ProtocolTCP), Port: ptrIntOrString(intstr.FromInt32(443))},
						{Protocol: ptrProtocol(corev1.ProtocolUDP), Port: ptrIntOrString(intstr.FromInt32(externalMockDNSPort))},
						{Protocol: ptrProtocol(corev1.ProtocolTCP), Port: ptrIntOrString(intstr.FromInt32(externalMockDNSPort))},
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

type topologyFlowMap map[string]map[string]struct{}

func (a *Activities) createTopologyNetworkPolicies(ctx context.Context, namespace, scanID string, topology *SandboxTopology, workloads []TopologyWorkload) error {
	flows := allowedTopologyFlows(topology, workloads)
	log.Printf("[CreateSandbox] scan=%s topology network policy flow_count=%d workload_count=%d", scanID, countTopologyFlows(flows), len(workloads))
	for _, workload := range workloads {
		name := kubernetesName(workload.Name)
		policy := topologyNetworkPolicy(namespace, scanID, name, workload, workloads, flows)
		_, err := a.k8s.NetworkingV1().NetworkPolicies(namespace).Create(ctx, policy, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			log.Printf("[CreateSandbox] networkpolicy %s/%s already exists", namespace, policy.Name)
			continue
		}
		if err != nil {
			return fmt.Errorf("create topology network policy %s/%s: %w", namespace, policy.Name, err)
		}
		log.Printf("[CreateSandbox] networkpolicy %s/%s created", namespace, policy.Name)
	}
	return nil
}

func topologyNetworkPolicy(namespace, scanID, name string, workload TopologyWorkload, workloads []TopologyWorkload, flows topologyFlowMap) *networkingv1.NetworkPolicy {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-allow-" + name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aegis-worker-deployer",
				"aegis-scan":                   scanID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: topologyLabels(scanID, name)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{externalMockEgressRule(scanID)},
		},
	}

	if targets := flows[name]; len(targets) > 0 {
		for _, target := range sortedFlowTargets(targets) {
			if targetWorkload, ok := findTopologyWorkload(workloads, target); ok {
				policy.Spec.Egress = append(policy.Spec.Egress, topologyPeerEgressRule(scanID, target, targetWorkload.normalizedPorts()))
			}
		}
	}

	for _, source := range sortedFlowSources(flows) {
		if _, ok := flows[source][name]; !ok {
			continue
		}
		policy.Spec.Ingress = append(policy.Spec.Ingress, topologyPeerIngressRule(scanID, source, workload.normalizedPorts()))
	}
	return policy
}

func allowedTopologyFlows(topology *SandboxTopology, workloads []TopologyWorkload) topologyFlowMap {
	flows := topologyFlowMap{}
	known := map[string]struct{}{}
	for _, workload := range workloads {
		known[kubernetesName(workload.Name)] = struct{}{}
	}
	add := func(source, target string) {
		source = kubernetesName(source)
		target = kubernetesName(target)
		if source == "" || target == "" || source == target {
			return
		}
		if _, ok := known[source]; !ok {
			return
		}
		if _, ok := known[target]; !ok {
			return
		}
		if flows[source] == nil {
			flows[source] = map[string]struct{}{}
		}
		flows[source][target] = struct{}{}
	}

	for _, workload := range workloads {
		source := kubernetesName(workload.Name)
		for _, dependency := range workload.DependsOn {
			add(source, dependency)
		}
		for _, target := range workload.WaitFor {
			add(source, strings.Split(target, ":")[0])
		}
	}
	if topology != nil {
		for _, connection := range append(topology.Connections, topology.Routes...) {
			add(connectionSource(connection), connectionTarget(connection))
		}
	}
	for source, targets := range inferredTopologyFlows(workloads) {
		for target := range targets {
			add(source, target)
		}
	}
	return flows
}

func inferredTopologyFlows(workloads []TopologyWorkload) topologyFlowMap {
	flows := topologyFlowMap{}
	add := func(source, target string) {
		source = kubernetesName(source)
		target = kubernetesName(target)
		if source == "" || target == "" || source == target {
			return
		}
		if flows[source] == nil {
			flows[source] = map[string]struct{}{}
		}
		flows[source][target] = struct{}{}
	}

	for _, source := range workloads {
		sourceName := kubernetesName(source.Name)
		prefix := workloadNamePrefix(sourceName)
		for _, target := range workloads {
			targetName := kubernetesName(target.Name)
			if targetName == "" || targetName == sourceName {
				continue
			}
			if envMentionsWorkload(source.Env, targetName) {
				add(sourceName, targetName)
				continue
			}
			if prefix != "" && strings.HasPrefix(targetName, prefix+"-") {
				if strings.Contains(sourceName, "frontend") && (strings.Contains(targetName, "backend") || strings.Contains(targetName, "api")) {
					add(sourceName, targetName)
				}
				if (strings.Contains(sourceName, "backend") || strings.Contains(sourceName, "server") || strings.Contains(sourceName, "api")) && (strings.Contains(targetName, "db") || strings.Contains(targetName, "postgres") || strings.Contains(targetName, "redis")) {
					add(sourceName, targetName)
				}
			}
		}
	}
	return flows
}

func envMentionsWorkload(env map[string]string, target string) bool {
	if len(env) == 0 || target == "" {
		return false
	}
	target = kubernetesName(target)
	for key, value := range env {
		combined := strings.ToLower(strings.TrimSpace(key) + "=" + strings.TrimSpace(value))
		if strings.Contains(combined, target) {
			return true
		}
	}
	return false
}

func connectionSource(connection TopologyConnection) string {
	return firstNonEmpty(connection.Source, connection.SourceName, connection.SourceNameCamel)
}

func connectionTarget(connection TopologyConnection) string {
	return firstNonEmpty(connection.Target, connection.TargetName, connection.TargetNameCamel)
}

func topologyPeerEgressRule(scanID, target string, ports []TopologyPort) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{topologyPeer(scanID, target)},
		Ports: topologyNetworkPolicyPorts(ports),
	}
}

func topologyPeerIngressRule(scanID, source string, ports []TopologyPort) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{topologyPeer(scanID, source)},
		Ports: topologyNetworkPolicyPorts(ports),
	}
}

func topologyPeer(scanID, name string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: topologyLabels(scanID, name)}}
}

func topologyNetworkPolicyPorts(ports []TopologyPort) []networkingv1.NetworkPolicyPort {
	if len(ports) == 0 {
		return nil
	}
	networkPolicyPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, port := range ports {
		protocol := corev1.ProtocolTCP
		if strings.EqualFold(port.Protocol, "UDP") {
			protocol = corev1.ProtocolUDP
		}
		networkPolicyPorts = append(networkPolicyPorts, networkingv1.NetworkPolicyPort{
			Protocol: ptrProtocol(protocol),
			Port:     ptrIntOrString(intstr.FromInt32(port.containerPort())),
		})
	}
	return networkPolicyPorts
}

func externalMockEgressRule(scanID string) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: externalMockLabels(scanID)}}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: ptrProtocol(corev1.ProtocolTCP), Port: ptrIntOrString(intstr.FromInt32(80))},
			{Protocol: ptrProtocol(corev1.ProtocolTCP), Port: ptrIntOrString(intstr.FromInt32(443))},
			{Protocol: ptrProtocol(corev1.ProtocolUDP), Port: ptrIntOrString(intstr.FromInt32(externalMockDNSPort))},
			{Protocol: ptrProtocol(corev1.ProtocolTCP), Port: ptrIntOrString(intstr.FromInt32(externalMockDNSPort))},
		},
	}
}

func findTopologyWorkload(workloads []TopologyWorkload, name string) (TopologyWorkload, bool) {
	for _, workload := range workloads {
		if kubernetesName(workload.Name) == name {
			return workload, true
		}
	}
	return TopologyWorkload{}, false
}

func sortedFlowTargets(targets map[string]struct{}) []string {
	values := make([]string, 0, len(targets))
	for target := range targets {
		values = append(values, target)
	}
	sort.Strings(values)
	return values
}

func sortedFlowSources(flows topologyFlowMap) []string {
	values := make([]string, 0, len(flows))
	for source := range flows {
		values = append(values, source)
	}
	sort.Strings(values)
	return values
}

func countTopologyFlows(flows topologyFlowMap) int {
	count := 0
	for _, targets := range flows {
		count += len(targets)
	}
	return count
}

func (a *Activities) sandboxRuntimeClassName(ctx context.Context) *string {
	runtimeClassName := strings.TrimSpace(os.Getenv(sandboxRuntimeClassEnv))
	if runtimeClassName == "" {
		return nil
	}
	if _, err := a.k8s.NodeV1().RuntimeClasses().Get(ctx, runtimeClassName, metav1.GetOptions{}); err != nil {
		log.Printf("[CreateSandbox] runtimeclass %q unavailable; using cluster default runtime: %v", runtimeClassName, err)
		return nil
	}
	return ptrString(runtimeClassName)
}

func (a *Activities) createPod(ctx context.Context, namespace, podName, scanID, image, mockDNSIP string) error {
	runtimeClassName := a.sandboxRuntimeClassName(ctx)
	dnsPolicy := corev1.DNSClusterFirst
	var dnsConfig *corev1.PodDNSConfig
	if strings.TrimSpace(mockDNSIP) != "" {
		dnsPolicy = corev1.DNSNone
		dnsConfig = sandboxDNSConfig(namespace, mockDNSIP)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
			Labels: map[string]string{
				"app":  "vulnerable-target",
				"scan": scanID,
			},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: runtimeClassName,
			DNSPolicy:        dnsPolicy,
			DNSConfig:        dnsConfig,
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

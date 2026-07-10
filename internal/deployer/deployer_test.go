package deployer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func TestCreateSandboxCreatesNamespacePodAndService(t *testing.T) {
	ctx := context.Background()
	t.Setenv(sandboxRuntimeClassEnv, sandboxRuntimeClassName)
	k8s := fake.NewSimpleClientset(fakeSandboxRuntimeClass())
	var createdPod *corev1.Pod
	k8s.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		pod := createAction.GetObject().(*corev1.Pod)
		createdPod = pod
		return true, pod, nil
	})
	k8s.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() != "target-scan-1" {
			return false, nil, nil
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "target-scan-1", Namespace: "aegis-war-room-scan-1"},
			Spec: corev1.PodSpec{
				RuntimeClassName: ptrString(sandboxRuntimeClassName),
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				}},
			},
		}, nil
	})
	activities := NewActivities(k8s)

	response, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID:      "scan-1",
		TargetImage: "nginx:latest",
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	if response.Namespace != "aegis-war-room-scan-1" {
		t.Fatalf("unexpected namespace: %s", response.Namespace)
	}
	if response.Endpoint != "http://svc-scan-1.aegis-war-room-scan-1.svc.cluster.local:80" {
		t.Fatalf("unexpected endpoint: %s", response.Endpoint)
	}

	if _, err := k8s.CoreV1().Namespaces().Get(ctx, response.Namespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace was not created: %v", err)
	}
	if _, err := k8s.CoreV1().Pods(response.Namespace).Get(ctx, "target-scan-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("pod was not created: %v", err)
	}
	pod, err := k8s.CoreV1().Pods(response.Namespace).Get(ctx, "target-scan-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod was not readable: %v", err)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != sandboxRuntimeClassName {
		t.Fatalf("expected pod runtimeClassName %q, got %#v", sandboxRuntimeClassName, pod.Spec.RuntimeClassName)
	}
	if createdPod == nil {
		t.Fatalf("target pod create was not observed")
	}
	if createdPod.Spec.DNSPolicy != corev1.DNSNone {
		t.Fatalf("expected target pod to use mock DNS, got %s", createdPod.Spec.DNSPolicy)
	}
	if createdPod.Spec.DNSConfig == nil || len(createdPod.Spec.DNSConfig.Nameservers) != 1 {
		t.Fatalf("expected target pod mock DNS config, got %#v", createdPod.Spec.DNSConfig)
	}
	if _, err := k8s.CoreV1().Services(response.Namespace).Get(ctx, "svc-scan-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("service was not created: %v", err)
	}
	mockService, err := k8s.CoreV1().Services(response.Namespace).Get(ctx, externalMockName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("external mock service was not created: %v", err)
	}
	if len(mockService.Spec.Ports) != 4 {
		t.Fatalf("expected external mock HTTP, HTTPS, and DNS ports, got %#v", mockService.Spec.Ports)
	}
	if _, err := k8s.AppsV1().Deployments(response.Namespace).Get(ctx, externalMockName, metav1.GetOptions{}); err != nil {
		t.Fatalf("external mock deployment was not created: %v", err)
	}
	mockConfig, err := k8s.CoreV1().ConfigMaps(response.Namespace).Get(ctx, externalMockName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("external mock configmap was not created: %v", err)
	}
	if !strings.Contains(mockConfig.Data["default.conf"], "listen 8443 ssl") {
		t.Fatalf("expected HTTPS mock listener, got %q", mockConfig.Data["default.conf"])
	}
	policy, err := k8s.NetworkingV1().NetworkPolicies(response.Namespace).Get(ctx, "default-deny-egress", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("network policy was not created: %v", err)
	}
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != "Egress" {
		t.Fatalf("expected egress-only network policy, got %#v", policy.Spec.PolicyTypes)
	}
	if networkPolicyAllowsPodSelector(policy.Spec.Egress, map[string]string{}) {
		t.Fatalf("default deny policy must not allow namespace-local pod-to-pod egress: %#v", policy.Spec.Egress)
	}
	if len(policy.Spec.Egress) != 2 || len(policy.Spec.Egress[0].Ports) != 2 || len(policy.Spec.Egress[1].Ports) != 4 {
		t.Fatalf("expected kube DNS and external mock egress only, got %#v", policy.Spec.Egress)
	}
}

func fakeSandboxRuntimeClass() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxRuntimeClassName},
		Handler:    "runsc",
	}
}

func TestCreateSandboxFailsFastOnImagePullError(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "target-scan-1", Namespace: namespace},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "target-container",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ImagePullBackOff",
							Message: "pull failed",
						},
					},
				}},
			},
		},
	)
	activities := NewActivities(k8s)

	_, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID:      "scan-1",
		TargetImage: "nginx:missing",
	})
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("expected image pull error, got %v", err)
	}
}

func TestCreateSandboxValidatesRequiredFields(t *testing.T) {
	activities := NewActivities(fake.NewSimpleClientset())

	for name, req := range map[string]SandboxRequest{
		"scan id":      {TargetImage: "nginx:latest"},
		"target image": {ScanID: "scan-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := activities.CreateSandbox(context.Background(), req); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestCreateSandboxReturnsCreateErrors(t *testing.T) {
	for name, reactor := range map[string]func(*fake.Clientset){
		"namespace": func(k8s *fake.Clientset) {
			k8s.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("namespace denied")
			})
		},
		"pod": func(k8s *fake.Clientset) {
			k8s.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("pod denied")
			})
		},
		"service": func(k8s *fake.Clientset) {
			k8s.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("service denied")
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			k8s := fake.NewSimpleClientset()
			reactor(k8s)
			activities := NewActivities(k8s)

			_, err := activities.CreateSandbox(context.Background(), SandboxRequest{
				ScanID:      "scan-1",
				TargetImage: "nginx:latest",
			})
			if err == nil || !strings.Contains(err.Error(), "denied") {
				t.Fatalf("expected create error, got %v", err)
			}
		})
	}
}

func TestCreateSandboxAllowsExistingResources(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "target-scan-1", Namespace: namespace},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				}},
			},
		},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-scan-1", Namespace: namespace}},
	)
	activities := NewActivities(k8s)

	if _, err := activities.CreateSandbox(ctx, SandboxRequest{ScanID: "scan-1", TargetImage: "nginx:latest"}); err != nil {
		t.Fatalf("CreateSandbox returned error for existing resources: %v", err)
	}
}

func TestCreateSandboxCreatesTopologyDeploymentsAndServices(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	t.Setenv(sandboxRuntimeClassEnv, sandboxRuntimeClassName)
	k8s := fake.NewSimpleClientset(fakeSandboxRuntimeClass())
	createdDeployments := map[string]*appsv1.Deployment{}
	createdDeploymentOrder := []string{}
	createdServices := map[string]*corev1.Service{}
	k8s.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		deployment := createAction.GetObject().(*appsv1.Deployment)
		createdDeployments[deployment.Name] = deployment
		createdDeploymentOrder = append(createdDeploymentOrder, deployment.Name)
		return true, deployment, nil
	})
	k8s.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		service := createAction.GetObject().(*corev1.Service)
		createdServices[service.Name] = service
		return true, service, nil
	})
	k8s.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		name := getAction.GetName()
		deployment := createdDeployments[name]
		if deployment == nil {
			return false, nil, nil
		}
		readyDeployment := deployment.DeepCopy()
		readyDeployment.Namespace = namespace
		readyDeployment.Generation = 1
		readyDeployment.Status = appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    *deployment.Spec.Replicas,
			AvailableReplicas:  *deployment.Spec.Replicas,
		}
		return true, readyDeployment, nil
	})
	activities := NewActivities(k8s)

	response, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID: "scan-1",
		TopologyJSON: `{
			"services": [
				{
					"name": "web frontend",
					"image": "nginx:1.27",
					"depends_on": ["api"],
					"wait_for": ["api:8080"],
					"command": ["/bin/sh"],
					"args": ["-c", "nginx -g 'daemon off;'"] ,
					"working_dir": "/app",
					"env": {"API_URL": "http://api:8080"},
					"init_containers": [{"name": "migrate db", "image": "busybox:1.36", "command": ["sh", "-c"], "args": ["echo migrate"]}],
					"config_files": [{"path": "/app/config.yml", "content": "debug: true"}],
					"secret_files": [{"path": "/app/secret.key", "content": "mock-secret"}],
					"empty_dirs": [{"name": "uploads", "mount_path": "/app/uploads"}],
					"resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"cpu": "100m", "memory": "128Mi"}},
					"security_context": {"runAsUser": 1000, "allowPrivilegeEscalation": false},
					"pod_security_context": {"fsGroup": 1000},
					"service": {"aliases": ["web-internal"]},
					"livenessProbe": {
						"httpGet": {
							"host": "api.stripe.com",
							"path": "/health",
							"port": 443
						}
					},
					"ports": [{"port": 80, "container_port": 8080}]
				},
				{
					"name": "api",
					"image": "ghcr.io/aegis/api:anon",
					"env_vars": [{"name": "DB_HOST", "value": "postgres"}],
					"ports": [{"name": "http", "port": 8080}]
				}
			]
		}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if response.Namespace != namespace {
		t.Fatalf("unexpected namespace: %s", response.Namespace)
	}
	if response.Endpoint != "http://web-frontend.aegis-war-room-scan-1.svc.cluster.local:80" {
		t.Fatalf("unexpected endpoint: %s", response.Endpoint)
	}
	if indexOfString(createdDeploymentOrder, "api") == -1 || indexOfString(createdDeploymentOrder, "web-frontend") == -1 {
		t.Fatalf("expected api and web deployments to be created, got %#v", createdDeploymentOrder)
	}
	if indexOfString(createdDeploymentOrder, "api") > indexOfString(createdDeploymentOrder, "web-frontend") {
		t.Fatalf("expected api dependency before web deployment, got %#v", createdDeploymentOrder)
	}

	webDeployment := createdDeployments["web-frontend"]
	if webDeployment == nil {
		t.Fatalf("web deployment was not created: %v", err)
	}
	if webDeployment.Spec.Template.Spec.RuntimeClassName == nil || *webDeployment.Spec.Template.Spec.RuntimeClassName != sandboxRuntimeClassName {
		t.Fatalf("expected deployment runtimeClassName %q, got %#v", sandboxRuntimeClassName, webDeployment.Spec.Template.Spec.RuntimeClassName)
	}
	webContainer := webDeployment.Spec.Template.Spec.Containers[0]
	if webContainer.Image != "nginx:1.27" {
		t.Fatalf("unexpected web image: %s", webContainer.Image)
	}
	if len(webContainer.Command) != 1 || webContainer.Command[0] != "/bin/sh" || webContainer.WorkingDir != "/app" {
		t.Fatalf("unexpected command or working dir: %#v wd=%s", webContainer.Command, webContainer.WorkingDir)
	}
	if webContainer.Resources.Requests.Cpu().String() != "50m" || webContainer.Resources.Limits.Memory().String() != "128Mi" {
		t.Fatalf("unexpected resources: %#v", webContainer.Resources)
	}
	if webContainer.SecurityContext == nil || webContainer.SecurityContext.RunAsUser == nil || *webContainer.SecurityContext.RunAsUser != 1000 {
		t.Fatalf("expected container security context: %#v", webContainer.SecurityContext)
	}
	if webDeployment.Spec.Template.Spec.SecurityContext == nil || webDeployment.Spec.Template.Spec.SecurityContext.FSGroup == nil || *webDeployment.Spec.Template.Spec.SecurityContext.FSGroup != 1000 {
		t.Fatalf("expected pod security context: %#v", webDeployment.Spec.Template.Spec.SecurityContext)
	}
	if len(webDeployment.Spec.Template.Spec.InitContainers) != 2 || webDeployment.Spec.Template.Spec.InitContainers[0].Name != "wait-for-api" {
		t.Fatalf("expected wait_for and custom init containers: %#v", webDeployment.Spec.Template.Spec.InitContainers)
	}
	if len(webContainer.VolumeMounts) != 3 {
		t.Fatalf("expected config, secret, and emptyDir mounts: %#v", webContainer.VolumeMounts)
	}
	if webContainer.ReadinessProbe == nil || webContainer.ReadinessProbe.TCPSocket == nil {
		t.Fatalf("expected readiness probe on web deployment: %#v", webContainer.ReadinessProbe)
	}
	if webContainer.ReadinessProbe.TCPSocket.Port.IntVal != 8080 {
		t.Fatalf("unexpected readiness probe port: %#v", webContainer.ReadinessProbe.TCPSocket.Port)
	}
	if webContainer.LivenessProbe == nil || webContainer.LivenessProbe.TCPSocket == nil {
		t.Fatalf("expected local TCP liveness probe on web deployment: %#v", webContainer.LivenessProbe)
	}
	if webContainer.LivenessProbe.HTTPGet != nil || webContainer.LivenessProbe.Exec != nil {
		t.Fatalf("expected external liveness probe to be removed: %#v", webContainer.LivenessProbe)
	}
	if webContainer.LivenessProbe.TCPSocket.Port.IntVal != 8080 {
		t.Fatalf("unexpected liveness probe port: %#v", webContainer.LivenessProbe.TCPSocket.Port)
	}
	if webDeployment.Spec.Template.Spec.DNSPolicy != corev1.DNSNone {
		t.Fatalf("expected topology deployment to use mock DNS, got %s", webDeployment.Spec.Template.Spec.DNSPolicy)
	}
	if webDeployment.Spec.Template.Spec.DNSConfig == nil || len(webDeployment.Spec.Template.Spec.DNSConfig.Nameservers) != 1 {
		t.Fatalf("expected mock DNS nameserver: %#v", webDeployment.Spec.Template.Spec.DNSConfig)
	}
	if len(webContainer.Env) != 1 || webContainer.Env[0].Name != "API_URL" || webContainer.Env[0].Value != "http://api:8080" {
		t.Fatalf("unexpected web env: %#v", webContainer.Env)
	}
	if len(webContainer.Ports) != 1 || webContainer.Ports[0].ContainerPort != 8080 {
		t.Fatalf("unexpected web ports: %#v", webContainer.Ports)
	}

	webService := createdServices["web-frontend"]
	if webService == nil {
		t.Fatalf("web service was not created: %v", err)
	}
	if createdServices["web-internal"] == nil {
		t.Fatalf("web alias service was not created")
	}
	if webService.Spec.Ports[0].Port != 80 || webService.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Fatalf("unexpected web service port: %#v", webService.Spec.Ports[0])
	}
	configMap, err := k8s.CoreV1().ConfigMaps(namespace).Get(ctx, "web-frontend-config", metav1.GetOptions{})
	if err != nil || configMap.Data["config.yml"] != "debug: true" {
		t.Fatalf("expected web config map, got %#v err=%v", configMap, err)
	}
	secret, err := k8s.CoreV1().Secrets(namespace).Get(ctx, "web-frontend-secret", metav1.GetOptions{})
	if err != nil || secret.StringData["secret.key"] != "mock-secret" {
		t.Fatalf("expected web secret, got %#v err=%v", secret, err)
	}
	mockService := createdServices[externalMockName]
	if mockService == nil {
		t.Fatalf("external dependency mock service was not created")
	}
	if len(mockService.Spec.Ports) != 4 {
		t.Fatalf("unexpected mock service ports: %#v", mockService.Spec.Ports)
	}
	mockDeployment := createdDeployments[externalMockName]
	if mockDeployment == nil {
		t.Fatalf("external dependency mock deployment was not created")
	}
	if len(mockDeployment.Spec.Template.Spec.Containers) != 3 {
		t.Fatalf("expected HTTP and DNS mock containers: %#v", mockDeployment.Spec.Template.Spec.Containers)
	}
	mockConfig, err := k8s.CoreV1().ConfigMaps(namespace).Get(ctx, externalMockName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("external dependency mock configmap was not created: %v", err)
	}
	if !strings.Contains(mockConfig.Data["Corefile"], "template IN A .") {
		t.Fatalf("expected CoreDNS wildcard template, got %q", mockConfig.Data["Corefile"])
	}
	if !strings.Contains(mockConfig.Data["default.conf"], "return 200") {
		t.Fatalf("expected HTTP mock to return 200, got %q", mockConfig.Data["default.conf"])
	}
	if !strings.Contains(mockConfig.Data["default.conf"], "listen 8443 ssl") {
		t.Fatalf("expected HTTPS mock to listen with TLS, got %q", mockConfig.Data["default.conf"])
	}
	httpContainer := mockDeployment.Spec.Template.Spec.Containers[0]
	if !strings.Contains(httpContainer.Image, "openresty/openresty") {
		t.Fatalf("expected OpenResty mock image for Lua latency support, got %q", httpContainer.Image)
	}
	if len(httpContainer.Args) != 1 || !strings.Contains(httpContainer.Args[0], "openssl req -x509") {
		t.Fatalf("expected mock TLS material to be generated at startup, got %#v", httpContainer.Args)
	}
	if !strings.Contains(httpContainer.Args[0], "/usr/local/openresty/bin/openresty -g 'daemon off;'") {
		t.Fatalf("expected mock to start OpenResty with absolute path, got %#v", httpContainer.Args)
	}

	apiDeployment := createdDeployments["api"]
	if apiDeployment == nil {
		t.Fatalf("api deployment was not created: %v", err)
	}
	apiContainer := apiDeployment.Spec.Template.Spec.Containers[0]
	if len(apiContainer.Env) != 1 || apiContainer.Env[0].Name != "DB_HOST" || apiContainer.Env[0].Value != "postgres" {
		t.Fatalf("unexpected api env: %#v", apiContainer.Env)
	}
}

func TestCreateSandboxCreatesPerWorkloadNetworkPoliciesFromTopologyGraph(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	createdDeployments := map[string]*appsv1.Deployment{}
	k8s.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		deployment := createAction.GetObject().(*appsv1.Deployment)
		createdDeployments[deployment.Name] = deployment
		return true, deployment, nil
	})
	k8s.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		deployment := createdDeployments[getAction.GetName()]
		if deployment == nil {
			return false, nil, nil
		}
		readyDeployment := deployment.DeepCopy()
		readyDeployment.Namespace = namespace
		readyDeployment.Generation = 1
		readyDeployment.Status = appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    *deployment.Spec.Replicas,
			AvailableReplicas:  *deployment.Spec.Replicas,
		}
		return true, readyDeployment, nil
	})
	activities := NewActivities(k8s)

	_, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID: "scan-1",
		TopologyJSON: `{
			"services": [
				{"name":"web","image":"nginx:1.27","depends_on":["api"],"ports":[{"port":80,"container_port":8080}]},
				{"name":"api","image":"api:latest","ports":[{"port":8080}],"depends_on":["postgres"]},
				{"name":"postgres","image":"postgres:16","ports":[{"port":5432}]}
			]
		}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	policies, err := k8s.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list network policies: %v", err)
	}
	defaultPolicy := findNetworkPolicy(policies.Items, "default-deny-egress")
	if defaultPolicy == nil {
		t.Fatalf("expected default deny egress policy")
	}
	if networkPolicyAllowsPodSelector(defaultPolicy.Spec.Egress, map[string]string{}) {
		t.Fatalf("default egress policy must not allow all pod-to-pod traffic: %#v", defaultPolicy.Spec.Egress)
	}

	webPolicy := findNetworkPolicy(policies.Items, "sandbox-allow-web")
	apiPolicy := findNetworkPolicy(policies.Items, "sandbox-allow-api")
	postgresPolicy := findNetworkPolicy(policies.Items, "sandbox-allow-postgres")
	if webPolicy == nil || apiPolicy == nil || postgresPolicy == nil {
		t.Fatalf("expected per-workload policies, got %#v", networkPolicyNames(policies.Items))
	}
	assertPolicyTypes(t, webPolicy, networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress)
	assertEgressPeer(t, webPolicy, topologyLabels("scan-1", "api"), int32(8080))
	assertNoEgressPeer(t, webPolicy, topologyLabels("scan-1", "postgres"))
	assertIngressPeer(t, apiPolicy, topologyLabels("scan-1", "web"), int32(8080))
	assertEgressPeer(t, apiPolicy, topologyLabels("scan-1", "postgres"), int32(5432))
	assertIngressPeer(t, postgresPolicy, topologyLabels("scan-1", "api"), int32(5432))
	assertNoIngressPeer(t, postgresPolicy, topologyLabels("scan-1", "web"))
}

func TestAllowedTopologyFlowsIncludesConnectionsAndRoutes(t *testing.T) {
	topology := &SandboxTopology{
		Connections: []TopologyConnection{{Source: "web", Target: "api"}},
		Routes:      []TopologyConnection{{SourceNameCamel: "api", TargetNameCamel: "postgres"}},
	}
	workloads := []TopologyWorkload{
		{Name: "web", Image: "nginx"},
		{Name: "api", Image: "api"},
		{Name: "postgres", Image: "postgres"},
		{Name: "unlinked", Image: "busybox"},
	}

	flows := allowedTopologyFlows(topology, workloads)
	if _, ok := flows["web"]["api"]; !ok {
		t.Fatalf("expected connection flow web -> api, got %#v", flows)
	}
	if _, ok := flows["api"]["postgres"]; !ok {
		t.Fatalf("expected route flow api -> postgres, got %#v", flows)
	}
	if _, ok := flows["web"]["postgres"]; ok {
		t.Fatalf("unexpected transitive flow web -> postgres: %#v", flows)
	}
	if _, ok := flows["unlinked"]; ok {
		t.Fatalf("unexpected flow for unlinked workload: %#v", flows)
	}
}

func TestAllowedTopologyFlowsInfersCommonAppDependencies(t *testing.T) {
	workloads := []TopologyWorkload{
		{Name: "portfolio-frontend", Image: "frontend", Env: map[string]string{"BACKEND_URL": "http://portfolio-backend:8080"}},
		{Name: "portfolio-backend", Image: "backend", Env: map[string]string{"DB_HOST": "portfolio-db"}},
		{Name: "portfolio-db", Image: "postgres"},
		{Name: "outline-server", Image: "outline"},
		{Name: "outline-redis", Image: "redis"},
	}

	flows := allowedTopologyFlows(&SandboxTopology{}, workloads)
	if _, ok := flows["portfolio-frontend"]["portfolio-backend"]; !ok {
		t.Fatalf("expected inferred frontend -> backend flow, got %#v", flows)
	}
	if _, ok := flows["portfolio-backend"]["portfolio-db"]; !ok {
		t.Fatalf("expected inferred backend -> db flow, got %#v", flows)
	}
	if _, ok := flows["outline-server"]["outline-redis"]; !ok {
		t.Fatalf("expected inferred server -> redis flow, got %#v", flows)
	}
}

func TestMockFunctionalEnvValueBuildsUsableDefaults(t *testing.T) {
	workloads := []TopologyWorkload{
		{Name: "portfolio-frontend", Image: "frontend", Ports: []TopologyPort{{Port: 80}}},
		{Name: "portfolio-backend", Image: "backend", Ports: []TopologyPort{{Port: 8080}}},
		{Name: "portfolio-db", Image: "postgres", Ports: []TopologyPort{{Port: 5432}}},
		{Name: "outline-server", Image: "outline", Ports: []TopologyPort{{Port: 3000}}},
		{Name: "outline-redis", Image: "redis", Ports: []TopologyPort{{Port: 6379}}},
	}

	frontend := workloads[0]
	backend := workloads[1]
	outline := workloads[3]

	if got := mockFunctionalEnvValue("BACKEND_URL", frontend, workloads); got != "http://portfolio-backend:8080" {
		t.Fatalf("unexpected BACKEND_URL mock: %q", got)
	}
	if got := mockFunctionalEnvValue("DATABASE_URL", backend, workloads); got != "postgres://postgres:aegis-mock-secret@portfolio-db:5432/postgres" {
		t.Fatalf("unexpected DATABASE_URL mock: %q", got)
	}
	if got := mockFunctionalEnvValue("DB_HOST", backend, workloads); got != "portfolio-db" {
		t.Fatalf("unexpected DB_HOST mock: %q", got)
	}
	if got := mockFunctionalEnvValue("REDIS_URL", outline, workloads); got != "redis://outline-redis:6379" {
		t.Fatalf("unexpected REDIS_URL mock: %q", got)
	}
	if got := mockFunctionalEnvValue("PORT", outline, workloads); got != "3000" {
		t.Fatalf("unexpected PORT mock: %q", got)
	}
	if got := mockFunctionalEnvValue("PUBLIC_URL", frontend, workloads); got != "http://portfolio-frontend:80" {
		t.Fatalf("unexpected PUBLIC_URL mock: %q", got)
	}
	if got := mockFunctionalEnvValue("UNRELATED", frontend, workloads); got != "aegis-mock-value" {
		t.Fatalf("unexpected generic mock: %q", got)
	}
}

func TestLooksLikeLocalImageReference(t *testing.T) {
	for _, image := range []string{"portfolio-backend", "portfolio-backend:latest"} {
		if !looksLikeLocalImageReference(image) {
			t.Fatalf("expected %q to look local", image)
		}
	}
	for _, image := range []string{"", "nginx", "postgres:16", "ghcr.io/acme/api:latest", "localhost:5000/api:latest", "api@sha256:deadbeef"} {
		if looksLikeLocalImageReference(image) {
			t.Fatalf("expected %q to not look local", image)
		}
	}
}

func indexOfString(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func findNetworkPolicy(policies []networkingv1.NetworkPolicy, name string) *networkingv1.NetworkPolicy {
	for i := range policies {
		if policies[i].Name == name {
			return &policies[i]
		}
	}
	return nil
}

func networkPolicyNames(policies []networkingv1.NetworkPolicy) []string {
	names := make([]string, 0, len(policies))
	for _, policy := range policies {
		names = append(names, policy.Name)
	}
	return names
}

func assertPolicyTypes(t *testing.T, policy *networkingv1.NetworkPolicy, expected ...networkingv1.PolicyType) {
	t.Helper()
	if len(policy.Spec.PolicyTypes) != len(expected) {
		t.Fatalf("unexpected policy types for %s: %#v", policy.Name, policy.Spec.PolicyTypes)
	}
	for i, policyType := range expected {
		if policy.Spec.PolicyTypes[i] != policyType {
			t.Fatalf("unexpected policy types for %s: %#v", policy.Name, policy.Spec.PolicyTypes)
		}
	}
}

func assertEgressPeer(t *testing.T, policy *networkingv1.NetworkPolicy, labels map[string]string, port int32) {
	t.Helper()
	if !egressPeerExists(policy.Spec.Egress, labels, port) {
		t.Fatalf("expected egress peer labels=%#v port=%d in %s: %#v", labels, port, policy.Name, policy.Spec.Egress)
	}
}

func assertNoEgressPeer(t *testing.T, policy *networkingv1.NetworkPolicy, labels map[string]string) {
	t.Helper()
	if egressPeerExists(policy.Spec.Egress, labels, 0) {
		t.Fatalf("unexpected egress peer labels=%#v in %s: %#v", labels, policy.Name, policy.Spec.Egress)
	}
}

func assertIngressPeer(t *testing.T, policy *networkingv1.NetworkPolicy, labels map[string]string, port int32) {
	t.Helper()
	if !ingressPeerExists(policy.Spec.Ingress, labels, port) {
		t.Fatalf("expected ingress peer labels=%#v port=%d in %s: %#v", labels, port, policy.Name, policy.Spec.Ingress)
	}
}

func assertNoIngressPeer(t *testing.T, policy *networkingv1.NetworkPolicy, labels map[string]string) {
	t.Helper()
	if ingressPeerExists(policy.Spec.Ingress, labels, 0) {
		t.Fatalf("unexpected ingress peer labels=%#v in %s: %#v", labels, policy.Name, policy.Spec.Ingress)
	}
}

func egressPeerExists(rules []networkingv1.NetworkPolicyEgressRule, labels map[string]string, port int32) bool {
	for _, rule := range rules {
		for _, peer := range rule.To {
			if selectorMatchesLabels(peer.PodSelector, labels) && networkPolicyPortsContain(rule.Ports, port) {
				return true
			}
		}
	}
	return false
}

func ingressPeerExists(rules []networkingv1.NetworkPolicyIngressRule, labels map[string]string, port int32) bool {
	for _, rule := range rules {
		for _, peer := range rule.From {
			if selectorMatchesLabels(peer.PodSelector, labels) && networkPolicyPortsContain(rule.Ports, port) {
				return true
			}
		}
	}
	return false
}

func networkPolicyAllowsPodSelector(rules []networkingv1.NetworkPolicyEgressRule, labels map[string]string) bool {
	for _, rule := range rules {
		for _, peer := range rule.To {
			if selectorMatchesLabels(peer.PodSelector, labels) && len(rule.Ports) == 0 {
				return true
			}
		}
	}
	return false
}

func selectorMatchesLabels(selector *metav1.LabelSelector, labels map[string]string) bool {
	if selector == nil {
		return false
	}
	if len(selector.MatchLabels) != len(labels) {
		return false
	}
	for key, value := range labels {
		if selector.MatchLabels[key] != value {
			return false
		}
	}
	return true
}

func networkPolicyPortsContain(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	if port == 0 {
		return true
	}
	for _, policyPort := range ports {
		if policyPort.Port != nil && policyPort.Port.IntVal == port {
			return true
		}
	}
	return false
}

func TestCreateSandboxCreatesStatefulTopologyWorkload(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	createdStatefulSets := map[string]*appsv1.StatefulSet{}
	k8s.PrependReactor("create", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		statefulSet := createAction.GetObject().(*appsv1.StatefulSet)
		createdStatefulSets[statefulSet.Name] = statefulSet
		return true, statefulSet, nil
	})
	k8s.PrependReactor("get", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		statefulSet := createdStatefulSets[getAction.GetName()]
		if statefulSet == nil {
			return false, nil, nil
		}
		readyStatefulSet := statefulSet.DeepCopy()
		readyStatefulSet.Namespace = namespace
		readyStatefulSet.Generation = 1
		readyStatefulSet.Status = appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			ReadyReplicas:      *statefulSet.Spec.Replicas,
			UpdatedReplicas:    *statefulSet.Spec.Replicas,
		}
		return true, readyStatefulSet, nil
	})
	activities := NewActivities(k8s)

	response, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID:       "scan-1",
		TopologyJSON: `{"deployments":[{"name":"postgres","image":"postgres:16","stateful":true,"service":{"headless":true},"ports":[{"port":5432}]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if response.Endpoint != "http://postgres.aegis-war-room-scan-1.svc.cluster.local:5432" {
		t.Fatalf("unexpected endpoint: %s", response.Endpoint)
	}
	statefulSet := createdStatefulSets["postgres"]
	if statefulSet == nil || statefulSet.Spec.ServiceName != "postgres" {
		t.Fatalf("expected postgres statefulset, got %#v", statefulSet)
	}
	service, err := k8s.CoreV1().Services(namespace).Get(ctx, "postgres", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected postgres service: %v", err)
	}
	if service.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatalf("expected headless service, got %#v", service.Spec)
	}
}

func TestCreateSandboxUsesDefaultRuntimeWhenGVisorIsUnavailable(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	createdDeployments := map[string]*appsv1.Deployment{}
	k8s.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		deployment := createAction.GetObject().(*appsv1.Deployment)
		createdDeployments[deployment.Name] = deployment
		return true, deployment, nil
	})
	k8s.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		service := createAction.GetObject().(*corev1.Service)
		return true, service, nil
	})
	k8s.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		name := getAction.GetName()
		deployment := createdDeployments[name]
		if deployment == nil {
			return false, nil, nil
		}
		readyDeployment := deployment.DeepCopy()
		readyDeployment.Namespace = namespace
		readyDeployment.Generation = 1
		readyDeployment.Status = appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    *deployment.Spec.Replicas,
			AvailableReplicas:  *deployment.Spec.Replicas,
		}
		return true, readyDeployment, nil
	})

	activities := NewActivities(k8s)
	_, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID: "scan-1",
		TopologyJSON: `{
			"services": [
				{"name": "web", "image": "nginx:1.27", "ports": [{"port": 80}]}
			]
		}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	webDeployment := createdDeployments["web"]
	if webDeployment == nil {
		t.Fatalf("web deployment was not created")
	}
	if webDeployment.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatalf("expected default runtime when gvisor is unavailable, got %#v", webDeployment.Spec.Template.Spec.RuntimeClassName)
	}
	mockDeployment := createdDeployments[externalMockName]
	if mockDeployment == nil {
		t.Fatalf("external mock deployment was not created")
	}
	if mockDeployment.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatalf("expected external mock default runtime when gvisor is unavailable, got %#v", mockDeployment.Spec.Template.Spec.RuntimeClassName)
	}
}

func TestCreateSandboxHonorsPreferredTopologyEndpoint(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	createdDeployments := map[string]*appsv1.Deployment{}
	k8s.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		deployment := createAction.GetObject().(*appsv1.Deployment)
		createdDeployments[deployment.Name] = deployment
		return true, deployment, nil
	})
	k8s.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		service := createAction.GetObject().(*corev1.Service)
		return true, service, nil
	})
	k8s.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		name := getAction.GetName()
		deployment := createdDeployments[name]
		if deployment == nil {
			return false, nil, nil
		}
		readyDeployment := deployment.DeepCopy()
		readyDeployment.Namespace = namespace
		readyDeployment.Generation = 1
		readyDeployment.Status = appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    *deployment.Spec.Replicas,
			AvailableReplicas:  *deployment.Spec.Replicas,
		}
		return true, readyDeployment, nil
	})

	activities := NewActivities(k8s)
	response, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID:                    "scan-1",
		PreferredEndpointWorkload: "api",
		TopologyJSON: `{
			"services": [
				{
					"name": "web frontend",
					"image": "nginx:1.27",
					"ports": [{"port": 80, "container_port": 8080}]
				},
				{
					"name": "api",
					"image": "ghcr.io/aegis/api:anon",
					"ports": [{"port": 8080}]
				}
			]
		}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if response.EndpointWorkload != "api" {
		t.Fatalf("unexpected endpoint workload: %s", response.EndpointWorkload)
	}
	if response.Endpoint != "http://api.aegis-war-room-scan-1.svc.cluster.local:8080" {
		t.Fatalf("unexpected endpoint: %s", response.Endpoint)
	}
}

func TestCreateSandboxFallsBackWhenPreferredTopologyEndpointIsNotReady(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	createdDeployments := map[string]*appsv1.Deployment{}
	k8s.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		deployment := createAction.GetObject().(*appsv1.Deployment)
		createdDeployments[deployment.Name] = deployment
		return true, deployment, nil
	})
	k8s.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		service := createAction.GetObject().(*corev1.Service)
		return true, service, nil
	})
	k8s.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		name := getAction.GetName()
		deployment := createdDeployments[name]
		if deployment == nil {
			return false, nil, nil
		}

		status := appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    *deployment.Spec.Replicas,
		}
		if name == "ready-api" {
			status.AvailableReplicas = *deployment.Spec.Replicas
		}

		deploymentCopy := deployment.DeepCopy()
		deploymentCopy.Namespace = namespace
		deploymentCopy.Generation = 1
		deploymentCopy.Status = status
		return true, deploymentCopy, nil
	})

	activities := NewActivities(k8s)
	response, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID:                    "scan-1",
		PreferredEndpointWorkload: "broken-app",
		TopologyJSON: `{
			"services": [
				{"name": "broken app", "image": "broken:latest", "ports": [{"port": 8080}]},
				{"name": "ready api", "image": "nginx:1.27", "ports": [{"port": 9090}]}
			]
		}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if response.EndpointWorkload != "ready-api" {
		t.Fatalf("unexpected endpoint workload: %s", response.EndpointWorkload)
	}
	if response.Endpoint != "http://ready-api.aegis-war-room-scan-1.svc.cluster.local:9090" {
		t.Fatalf("unexpected endpoint: %s", response.Endpoint)
	}
	status, ok := findWorkloadStatus(response.Workloads, "broken-app")
	if !ok {
		t.Fatalf("missing broken-app status: %#v", response.Workloads)
	}
	if status.Status != "not_ready" {
		t.Fatalf("unexpected broken-app status: %s", status.Status)
	}
}

func TestCreateSandboxContinuesWhenTopologyWorkloadIsNotReady(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	createdDeployments := map[string]*appsv1.Deployment{}
	createdServices := map[string]*corev1.Service{}
	origReadyTimeout := topologyDeploymentReadyTimeout
	topologyDeploymentReadyTimeout = time.Millisecond
	defer func() { topologyDeploymentReadyTimeout = origReadyTimeout }()

	k8s.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		deployment := createAction.GetObject().(*appsv1.Deployment)
		createdDeployments[deployment.Name] = deployment
		return true, deployment, nil
	})
	k8s.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		service := createAction.GetObject().(*corev1.Service)
		createdServices[service.Name] = service
		return true, service, nil
	})
	k8s.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		name := getAction.GetName()
		deployment := createdDeployments[name]
		if deployment == nil {
			return false, nil, nil
		}

		status := appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    *deployment.Spec.Replicas,
		}
		if name == "ready-api" {
			status.AvailableReplicas = *deployment.Spec.Replicas
		}

		deploymentCopy := deployment.DeepCopy()
		deploymentCopy.Namespace = namespace
		deploymentCopy.Generation = 1
		deploymentCopy.Status = status
		return true, deploymentCopy, nil
	})

	activities := NewActivities(k8s)
	response, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID: "scan-1",
		TopologyJSON: `{
			"services": [
				{"name": "broken app", "image": "broken:latest", "ports": [{"port": 8080}]},
				{"name": "ready api", "image": "nginx:1.27", "ports": [{"port": 9090}]}
			]
		}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox should continue past an unready workload: %v", err)
	}
	if response.Endpoint != "http://ready-api.aegis-war-room-scan-1.svc.cluster.local:9090" {
		t.Fatalf("unexpected endpoint: %s", response.Endpoint)
	}
	if createdDeployments["broken-app"] == nil || createdDeployments["ready-api"] == nil {
		t.Fatalf("expected both deployments to be created: %#v", createdDeployments)
	}
	if createdServices["broken-app"] == nil || createdServices["ready-api"] == nil {
		t.Fatalf("expected both services to be created: %#v", createdServices)
	}
}

func TestCreateSandboxSanitizesRedactedSecretsAcrossWorkloads(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	createdDeployments := map[string]*appsv1.Deployment{}
	k8s.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		deployment := createAction.GetObject().(*appsv1.Deployment)
		createdDeployments[deployment.Name] = deployment
		return true, deployment, nil
	})
	k8s.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		service := createAction.GetObject().(*corev1.Service)
		return true, service, nil
	})
	k8s.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		name := getAction.GetName()
		deployment := createdDeployments[name]
		if deployment == nil {
			return false, nil, nil
		}
		readyDeployment := deployment.DeepCopy()
		readyDeployment.Namespace = namespace
		readyDeployment.Generation = 1
		readyDeployment.Status = appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    *deployment.Spec.Replicas,
			AvailableReplicas:  *deployment.Spec.Replicas,
		}
		return true, readyDeployment, nil
	})

	activities := NewActivities(k8s)
	response, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID: "scan-1",
		TopologyJSON: `{
			"deployments": [
				{
					"name": "api",
					"image": "node:20",
					"env": {
						"DB_PASS": "REDACTED",
						"AWS_ACCESS_KEY_ID": "REDACTED",
						"API_KEY": "REDACTED",
						"JWT_SECRET": "REDACTED",
						"NODE_ENV": "production",
						"PATH": "REDACTED",
						"PUBLIC_URL": "REDACTED",
						"label:com.docker.compose.service": "api"
					},
					"ports": [{"port": 3000}]
				},
				{
					"name": "postgres",
					"image": "postgres:16",
					"env_vars": [{"name": "POSTGRES_PASSWORD", "value": "REDACTED"}],
					"ports": [{"port": 5432}]
				}
			]
		}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if response.Namespace != namespace {
		t.Fatalf("unexpected namespace: %s", response.Namespace)
	}

	apiDeployment := createdDeployments["api"]
	if apiDeployment == nil {
		t.Fatalf("api deployment was not created")
	}
	var apiPassword string
	apiEnv := map[string]string{}
	for _, env := range apiDeployment.Spec.Template.Spec.Containers[0].Env {
		apiEnv[env.Name] = env.Value
		if env.Name == "DB_PASS" {
			apiPassword = env.Value
		}
		if env.Value == "REDACTED" {
			t.Fatalf("api deployment still contains REDACTED: %#v", apiDeployment.Spec.Template.Spec.Containers[0].Env)
		}
	}
	if apiPassword == "" {
		t.Fatalf("api deployment is missing DB_PASS env")
	}

	postgresDeployment := createdDeployments["postgres"]
	if postgresDeployment == nil {
		t.Fatalf("postgres deployment was not created")
	}
	var postgresPassword string
	for _, env := range postgresDeployment.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "POSTGRES_PASSWORD" {
			postgresPassword = env.Value
		}
		if env.Value == "REDACTED" {
			t.Fatalf("postgres deployment still contains REDACTED: %#v", postgresDeployment.Spec.Template.Spec.Containers[0].Env)
		}
	}
	if postgresPassword == "" {
		t.Fatalf("postgres deployment is missing POSTGRES_PASSWORD env")
	}
	if apiPassword != postgresPassword {
		t.Fatalf("expected shared mock secret, got api=%q postgres=%q", apiPassword, postgresPassword)
	}
	if !strings.HasPrefix(apiPassword, topologyMockSecretPrefix) {
		t.Fatalf("unexpected mock secret format: %q", apiPassword)
	}
	if apiEnv["AWS_ACCESS_KEY_ID"] != "AKIA0000000000000000" {
		t.Fatalf("unexpected AWS_ACCESS_KEY_ID mock: %#v", apiEnv)
	}
	if apiEnv["API_KEY"] != "aegis-mock-api-key" {
		t.Fatalf("unexpected API_KEY mock: %#v", apiEnv)
	}
	if apiEnv["JWT_SECRET"] != "aegis-mock-jwt-secret" {
		t.Fatalf("unexpected JWT_SECRET mock: %#v", apiEnv)
	}
	if _, ok := apiEnv["PATH"]; ok {
		t.Fatalf("runtime PATH env should not be injected: %#v", apiEnv)
	}
	if !strings.HasPrefix(apiEnv["PUBLIC_URL"], "http://api:") {
		t.Fatalf("non-secret redacted env should be replaced with functional mock value: %#v", apiEnv)
	}
	if _, ok := apiEnv["label:com.docker.compose.service"]; ok {
		t.Fatalf("label-like env should be dropped: %#v", apiEnv)
	}
	if apiEnv["NODE_ENV"] != "production" {
		t.Fatalf("expected non-redacted env to be preserved: %#v", apiEnv)
	}
	if response.Summary.Requested != 2 || response.Summary.Ready != 2 || response.Summary.EndpointSelected != true {
		t.Fatalf("unexpected sandbox summary: %#v", response.Summary)
	}
	if len(response.Workloads) != 2 {
		t.Fatalf("expected workload statuses, got %#v", response.Workloads)
	}
}

func TestParseTopologyValidatesTypingAndCollisions(t *testing.T) {
	t.Run("valid nested topology", func(t *testing.T) {
		req := SandboxRequest{
			ScanID: "scan-1",
			Topology: &SandboxTopology{Deployments: []TopologyWorkload{{
				Name:  "postgres",
				Image: "postgres:16",
				Env: map[string]string{
					"POSTGRES_DB": "app",
				},
				Ports: []TopologyPort{{Port: 5432}},
			}}},
		}
		topology, err := req.parseTopology()
		if err != nil {
			t.Fatalf("parseTopology returned error: %v", err)
		}
		if len(topology.workloads()) != 1 {
			t.Fatalf("unexpected workload count: %d", len(topology.workloads()))
		}
	})

	t.Run("protobuf port number", func(t *testing.T) {
		req := SandboxRequest{TopologyJSON: `{"containers":[{"name":"api","image":"api:latest","ports":[{"number":8080,"protocol":"tcp"}]}]}`}
		topology, err := req.parseTopology()
		if err != nil {
			t.Fatalf("parseTopology returned error: %v", err)
		}
		port := topology.workloads()[0].Ports[0]
		if port.servicePort() != 8080 || port.containerPort() != 8080 {
			t.Fatalf("unexpected parsed port: %#v", port)
		}
	})

	t.Run("orders dependencies before dependents", func(t *testing.T) {
		req := SandboxRequest{TopologyJSON: `{
			"services":[{"name":"web","image":"web:latest","depends_on":["postgres"]}],
			"deployments":[{"name":"postgres","image":"postgres:16"}]
		}`}
		topology, err := req.parseTopology()
		if err != nil {
			t.Fatalf("parseTopology returned error: %v", err)
		}
		ordered, err := orderedTopologyWorkloads(topology.workloads())
		if err != nil {
			t.Fatalf("orderedTopologyWorkloads returned error: %v", err)
		}
		if len(ordered) != 2 || ordered[0].Name != "postgres" || ordered[1].Name != "web" {
			t.Fatalf("unexpected dependency order: %#v", ordered)
		}
	})

	t.Run("invalid env type", func(t *testing.T) {
		req := SandboxRequest{TopologyJSON: `{"services":[{"name":"api","image":"api:latest","env":42}]}`}
		if _, err := req.parseTopology(); err == nil || !strings.Contains(err.Error(), "env must be an object") {
			t.Fatalf("expected env typing error, got %v", err)
		}
	})

	t.Run("name collision", func(t *testing.T) {
		req := SandboxRequest{TopologyJSON: `{"services":[{"name":"api.v1","image":"api:1"},{"name":"api v1","image":"api:2"}]}`}
		if _, err := req.parseTopology(); err == nil || !strings.Contains(err.Error(), "collides") {
			t.Fatalf("expected collision error, got %v", err)
		}
	})

	t.Run("unknown dependency", func(t *testing.T) {
		req := SandboxRequest{TopologyJSON: `{"services":[{"name":"api","image":"api:latest","depends_on":["postgres"]}]}`}
		if _, err := req.parseTopology(); err == nil || !strings.Contains(err.Error(), "depends on unknown workload") {
			t.Fatalf("expected unknown dependency error, got %v", err)
		}
	})

	t.Run("dependency cycle", func(t *testing.T) {
		req := SandboxRequest{TopologyJSON: `{"services":[{"name":"api","image":"api:latest","depends_on":["web"]},{"name":"web","image":"web:latest","depends_on":["api"]}]}`}
		if _, err := req.parseTopology(); err == nil || !strings.Contains(err.Error(), "dependency cycle") {
			t.Fatalf("expected dependency cycle error, got %v", err)
		}
	})
}

func TestImageArchiveObjectParsesReferences(t *testing.T) {
	t.Setenv("MINIO_INGEST_BUCKET", "ingest-bucket")

	tests := []struct {
		name       string
		ref        string
		wantBucket string
		wantObject string
		wantErr    bool
	}{
		{name: "short minio ref", ref: "minio:company/images/web.tar", wantBucket: "ingest-bucket", wantObject: "company/images/web.tar"},
		{name: "s3 ref", ref: "s3://custom-bucket/images/web.tar", wantBucket: "custom-bucket", wantObject: "images/web.tar"},
		{name: "bare object", ref: "company/images/api.tar", wantBucket: "ingest-bucket", wantObject: "company/images/api.tar"},
		{name: "invalid s3 ref", ref: "s3://missing-object", wantErr: true},
		{name: "empty ref", ref: " ", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bucket, object, err := imageArchiveObject(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("imageArchiveObject returned error: %v", err)
			}
			if bucket != tc.wantBucket || object != tc.wantObject {
				t.Fatalf("unexpected archive object bucket=%q object=%q", bucket, object)
			}
		})
	}
}

func TestDownloadImageArchiveFromHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/image.tar" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("archive-bytes"))
	}))
	defer server.Close()

	file, err := os.CreateTemp("", "aegis-image-test-*.tar")
	if err != nil {
		t.Fatalf("CreateTemp returned error: %v", err)
	}
	path := file.Name()
	t.Cleanup(func() {
		_ = file.Close()
		_ = os.Remove(path)
	})

	if err := downloadImageArchive(context.Background(), server.URL+"/image.tar", file); err != nil {
		t.Fatalf("downloadImageArchive returned error: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(content) != "archive-bytes" {
		t.Fatalf("unexpected archive content: %q", content)
	}
}

func TestImportTopologyImageArchivesDeduplicatesHTTPArchives(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("archive-bytes"))
	}))
	defer server.Close()
	t.Setenv("AEGIS_IMAGE_IMPORT_COMMAND", "test -s {archive}")

	workloads := []TopologyWorkload{
		{Name: "api", Image: "local-api:latest", ImageArchiveRef: server.URL + "/image.tar"},
		{Name: "api-copy", Image: "local-api:latest", ImageArchiveRef: server.URL + "/image.tar"},
	}
	if err := importTopologyImageArchives(context.Background(), workloads); err != nil {
		t.Fatalf("importTopologyImageArchives returned error: %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected one archive download, got %d", requests)
	}
}

func TestCreateSandboxDoesNotReadNamespaces(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset()
	k8s.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("namespace read should not be required")
	})
	k8s.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() != "target-scan-1" {
			return false, nil, nil
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "target-scan-1", Namespace: namespace},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				}},
			},
		}, nil
	})
	activities := NewActivities(k8s)

	if _, err := activities.CreateSandbox(ctx, SandboxRequest{ScanID: "scan-1", TargetImage: "nginx:latest"}); err != nil {
		t.Fatalf("CreateSandbox should not require namespace reads: %v", err)
	}

	for _, action := range k8s.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
			t.Fatalf("CreateSandbox performed a forbidden namespace read")
		}
	}
}

func TestWaitForPodReadyReturnsReadAndTimeoutErrors(t *testing.T) {
	t.Run("read error", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		k8s.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("read failed")
		})
		activities := NewActivities(k8s)

		err := activities.waitForPodReady(context.Background(), "aegis-war-room-scan-1", "target-scan-1", time.Nanosecond)
		if err == nil || !strings.Contains(err.Error(), "read failed") {
			t.Fatalf("expected read error, got %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		k8s := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "target-scan-1",
			Namespace: "aegis-war-room-scan-1",
		}})
		activities := NewActivities(k8s)

		err := activities.waitForPodReady(context.Background(), "aegis-war-room-scan-1", "target-scan-1", time.Nanosecond)
		if err == nil || !strings.Contains(err.Error(), "timeout waiting for pod") {
			t.Fatalf("expected timeout error, got %v", err)
		}
	})
}

func TestDestroySandboxDeletesNamespace(t *testing.T) {
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	activities := NewActivities(k8s)

	result, err := activities.DestroySandbox(ctx, "scan-1")
	if err != nil {
		t.Fatalf("DestroySandbox returned error: %v", err)
	}
	if result != "CLEANED" {
		t.Fatalf("unexpected result: %s", result)
	}

	_, err = k8s.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected namespace to be deleted, got %v", err)
	}
}

func TestDestroySandboxCanRetainNamespaceForDebugging(t *testing.T) {
	t.Setenv(retainSandboxEnv, "true")
	ctx := context.Background()
	namespace := "aegis-war-room-scan-1"
	k8s := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	activities := NewActivities(k8s)

	result, err := activities.DestroySandbox(ctx, "scan-1")
	if err != nil {
		t.Fatalf("DestroySandbox returned error: %v", err)
	}
	if result != "RETAINED" {
		t.Fatalf("unexpected result: %s", result)
	}

	if _, err := k8s.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected namespace to be retained, got %v", err)
	}
}

func TestDestroySandboxHandlesValidationNotFoundAndDeleteErrors(t *testing.T) {
	activities := NewActivities(fake.NewSimpleClientset())
	if _, err := activities.DestroySandbox(context.Background(), " "); err == nil {
		t.Fatalf("expected validation error")
	}

	result, err := activities.DestroySandbox(context.Background(), "missing")
	if err != nil {
		t.Fatalf("DestroySandbox should ignore missing namespace: %v", err)
	}
	if result != "CLEANED" {
		t.Fatalf("unexpected result: %s", result)
	}

	k8s := fake.NewSimpleClientset()
	k8s.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete denied")
	})
	activities = NewActivities(k8s)
	if _, err := activities.DestroySandbox(context.Background(), "scan-1"); err == nil || !strings.Contains(err.Error(), "delete denied") {
		t.Fatalf("expected delete error, got %v", err)
	}
}

func TestValidateSandboxNamespaceRejectsUnsafeNames(t *testing.T) {
	for _, namespace := range []string{"default", "kube-system", "aegis-war-room-"} {
		if err := validateSandboxNamespace(namespace); err == nil {
			t.Fatalf("expected %q to be rejected", namespace)
		}
	}
}

func TestCheckImageErrorsIgnoresNonPullStates(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "running"},
				{
					Name: "waiting",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
					},
				},
			},
		},
	}

	if err := checkImageErrors(pod); err != nil {
		t.Fatalf("expected non-pull states to be ignored: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if got := sandboxNamespace("scan-1"); got != "aegis-war-room-scan-1" {
		t.Fatalf("unexpected sandbox namespace: %s", got)
	}
	if err := validateSandboxNamespace("aegis-war-room-scan-1"); err != nil {
		t.Fatalf("expected sandbox namespace to be accepted: %v", err)
	}

	t.Setenv("AEGIS_TEST_VALUE", "  configured  ")
	if got := getenv("AEGIS_TEST_VALUE", "fallback"); got != "configured" {
		t.Fatalf("unexpected env value: %s", got)
	}
	t.Setenv("AEGIS_TEST_VALUE", " ")
	if got := getenv("AEGIS_TEST_VALUE", "fallback"); got != "fallback" {
		t.Fatalf("unexpected fallback value: %s", got)
	}

	t.Setenv("AEGIS_TEST_BOOL", "yes")
	if !envBool("AEGIS_TEST_BOOL") {
		t.Fatalf("expected yes to be parsed as true")
	}
	t.Setenv("AEGIS_TEST_BOOL", "no")
	if envBool("AEGIS_TEST_BOOL") {
		t.Fatalf("expected no to be parsed as false")
	}
}

func TestTemporalClientOptionsWithoutTLS(t *testing.T) {
	t.Setenv("TEMPORAL_TLS_ENABLE", "false")

	options, err := temporalClientOptions("temporal:7233", "default")
	if err != nil {
		t.Fatalf("temporalClientOptions returned error: %v", err)
	}
	if options.HostPort != "temporal:7233" {
		t.Fatalf("unexpected host: %s", options.HostPort)
	}
	if options.Namespace != "default" {
		t.Fatalf("unexpected namespace: %s", options.Namespace)
	}
	if options.ConnectionOptions.TLS != nil {
		t.Fatalf("expected plaintext temporal options")
	}
}

func TestTemporalClientOptionsWithTLS(t *testing.T) {
	t.Setenv("TEMPORAL_TLS_ENABLE", "true")
	t.Setenv("TEMPORAL_TLS_SERVER_NAME", "temporal.internal")

	options, err := temporalClientOptions("temporal:7233", "default")
	if err != nil {
		t.Fatalf("temporalClientOptions returned error: %v", err)
	}
	if options.ConnectionOptions.TLS == nil {
		t.Fatalf("expected TLS config")
	}
	if options.ConnectionOptions.TLS.ServerName != "temporal.internal" {
		t.Fatalf("unexpected server name: %s", options.ConnectionOptions.TLS.ServerName)
	}
}

func TestTemporalTLSConfigValidationErrors(t *testing.T) {
	t.Run("invalid ca", func(t *testing.T) {
		caFile := t.TempDir() + "/ca.crt"
		if err := os.WriteFile(caFile, []byte("not a cert"), 0o600); err != nil {
			t.Fatalf("write ca: %v", err)
		}
		t.Setenv("TEMPORAL_TLS_CA_PATH", caFile)

		if _, err := temporalTLSConfig(); err == nil || !strings.Contains(err.Error(), "parse ca certificate") {
			t.Fatalf("expected ca parse error, got %v", err)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		t.Setenv("TEMPORAL_TLS_CERT_PATH", "/tmp/client.crt")

		if _, err := temporalTLSConfig(); err == nil || !strings.Contains(err.Error(), "must be configured together") {
			t.Fatalf("expected paired cert/key error, got %v", err)
		}
	})
}

func TestNewKubernetesClientReturnsErrorWithoutConfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "")

	if _, err := newKubernetesClient(); err == nil {
		t.Fatalf("expected kubernetes config error")
	}
}

func TestNewKubernetesClientReturnsErrorForInvalidKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "/definitely/missing/kubeconfig")

	if _, err := newKubernetesClient(); err == nil {
		t.Fatalf("expected kubeconfig error")
	}
}

func TestRun_KubernetesClientError(t *testing.T) {
	// Backup and restore globals
	origNewK8s := newK8sClient
	defer func() { newK8sClient = origNewK8s }()

	newK8sClient = func() (kubernetes.Interface, error) {
		return nil, errors.New("fake k8s error")
	}

	err := Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fake k8s error") {
		t.Fatalf("expected fake k8s error, got %v", err)
	}
}

type testMockClient struct {
	client.Client
}

func (m *testMockClient) Close() {}

type testMockWorker struct {
	worker.Worker
}

func (m *testMockWorker) RegisterActivityWithOptions(activity interface{}, options activity.RegisterOptions) {
	// noop
}

func (m *testMockWorker) Run(interruptCh <-chan interface{}) error {
	return nil
}

func TestRun_TemporalConnectionFailureAndSuccess(t *testing.T) {
	// Backup and restore globals
	origNewK8s := newK8sClient
	origDial := temporalDial
	origNewWorker := newWorker
	origAttempts := temporalConnectMaxAttempts
	origDelay := temporalConnectRetryDelay
	defer func() {
		newK8sClient = origNewK8s
		temporalDial = origDial
		newWorker = origNewWorker
		temporalConnectMaxAttempts = origAttempts
		temporalConnectRetryDelay = origDelay
	}()

	newK8sClient = func() (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(), nil
	}

	temporalConnectRetryDelay = 1 * time.Millisecond

	t.Run("fails after max attempts", func(t *testing.T) {
		temporalConnectMaxAttempts = 3
		dialCount := 0
		temporalDial = func(options client.Options) (client.Client, error) {
			dialCount++
			return nil, errors.New("dial failed")
		}

		err := Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "connect temporal: dial failed") {
			t.Fatalf("expected connection error, got %v", err)
		}
		if dialCount != 3 {
			t.Fatalf("expected 3 dial attempts, got %d", dialCount)
		}
	})

	t.Run("succeeds after retrying", func(t *testing.T) {
		t.Setenv("TEMPORAL_WORKER_STOP_TIMEOUT_SECONDS", "123")
		temporalConnectMaxAttempts = 5
		dialCount := 0
		temporalDial = func(options client.Options) (client.Client, error) {
			dialCount++
			if dialCount < 3 {
				return nil, errors.New("dial failed temporary")
			}
			return &testMockClient{}, nil
		}

		workerCreated := false
		newWorker = func(c client.Client, taskQueue string, options worker.Options) worker.Worker {
			workerCreated = true
			if options.WorkerStopTimeout != 123*time.Second {
				t.Fatalf("expected worker stop timeout 123s, got %s", options.WorkerStopTimeout)
			}
			return &testMockWorker{}
		}

		err := Run(context.Background())
		if err != nil {
			t.Fatalf("expected successful Run, got error: %v", err)
		}
		if dialCount != 3 {
			t.Fatalf("expected 3 dial attempts before success, got %d", dialCount)
		}
		if !workerCreated {
			t.Fatalf("expected worker to be created")
		}
	})

	t.Run("respects context cancellation during retry", func(t *testing.T) {
		temporalConnectMaxAttempts = 5
		dialCount := 0

		ctx, cancel := context.WithCancel(context.Background())
		temporalDial = func(options client.Options) (client.Client, error) {
			dialCount++
			cancel()
			return nil, errors.New("dial failed")
		}

		err := Run(ctx)
		if err == nil || !strings.Contains(err.Error(), "connect temporal cancelled") {
			t.Fatalf("expected cancelled error, got %v", err)
		}
	})
}

func TestSeedTargetDatabasesCreatesAnonymizedSeedJob(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "aegis-war-room-scan-1"}})
	activities := NewActivities(k8s)

	response, err := activities.SeedTargetDatabases(ctx, SeedDatabaseRequest{
		ScanID: "scan-1",
		DatabaseSchemas: []DatabaseSchema{{
			Engine:       "postgresql",
			Host:         "postgres.aegis-war-room-scan-1.svc.cluster.local",
			Port:         5432,
			DatabaseName: "app",
			Username:     "app_user",
			Password:     "real-password",
		}},
		RestoreSQL: "INSERT INTO users(email, full_name, password, api_key) VALUES ('john.doe@example.com', 'John Doe', 'real-password', 'sk_live_realSecret');",
	})
	if err != nil {
		t.Fatalf("SeedTargetDatabases returned error: %v", err)
	}
	if !response.Anonymized || response.SeededCount != 1 {
		t.Fatalf("unexpected seed response: %#v", response)
	}
	configMap, err := k8s.CoreV1().ConfigMaps("aegis-war-room-scan-1").Get(ctx, "db-seed-app-sql", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected seed configmap: %v", err)
	}
	seedSQL := configMap.Data["seed.sql"]
	for _, forbidden := range []string{"john.doe@example.com", "John Doe", "sk_live_realSecret"} {
		if strings.Contains(seedSQL, forbidden) {
			t.Fatalf("seed SQL leaked %q: %s", forbidden, seedSQL)
		}
	}
	if !strings.Contains(seedSQL, "user@example.test") || !strings.Contains(seedSQL, "PRENOM_1 NOM_1") {
		t.Fatalf("seed SQL was not anonymized as expected: %s", seedSQL)
	}
	job, err := k8s.BatchV1().Jobs("aegis-war-room-scan-1").Get(ctx, "db-seed-app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected seed job: %v", err)
	}
	if job.Spec.Template.Spec.Containers[0].Env[0].Name != "PGHOST" {
		t.Fatalf("expected psql env vars, got %#v", job.Spec.Template.Spec.Containers[0].Env)
	}
}

func TestCreateSandboxConfiguresExternalMockScenariosAndDebugBundle(t *testing.T) {
	ctx := context.Background()
	previousTimeout := topologyDeploymentReadyTimeout
	topologyDeploymentReadyTimeout = time.Millisecond
	defer func() { topologyDeploymentReadyTimeout = previousTimeout }()
	k8s := fake.NewSimpleClientset()
	activities := NewActivities(k8s)

	_, err := activities.CreateSandbox(ctx, SandboxRequest{
		ScanID:       "scan-1",
		TopologyJSON: `{"containers":[{"name":"web","image":"nginx","ports":[{"number":80}]}],"externalMocks":[{"host":"api.stripe.com","routes":[{"method":"POST","path":"/oauth/token","status":201,"body":"{\"token\":\"mock\"}"}]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	mockConfig, err := k8s.CoreV1().ConfigMaps("aegis-war-room-scan-1").Get(ctx, externalMockName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected external mock configmap: %v", err)
	}
	if !strings.Contains(mockConfig.Data["default.conf"], "location = /oauth/token") || !strings.Contains(mockConfig.Data["default.conf"], "api.stripe.com") {
		t.Fatalf("expected scripted external mock route, got %s", mockConfig.Data["default.conf"])
	}
	if _, err := k8s.CoreV1().ConfigMaps("aegis-war-room-scan-1").Get(ctx, sandboxDebugBundleName("scan-1"), metav1.GetOptions{}); err != nil {
		t.Fatalf("expected debug bundle configmap: %v", err)
	}
	if _, err := k8s.CoreV1().ConfigMaps("aegis-war-room-scan-1").Get(ctx, externalMockTrafficConfigMapName("scan-1"), metav1.GetOptions{}); err != nil {
		t.Fatalf("expected traffic bundle configmap: %v", err)
	}
}

func TestExternalMockNginxConfigEscapesRoutePayloadAndAppliesLatency(t *testing.T) {
	config := externalMockNginxConfig([]ExternalMockScenario{{
		Host: "api.example.test",
		Routes: []ExternalMockRoute{{
			Method:  "POST",
			Path:    "/v1/token",
			Status:  202,
			Latency: "250ms",
			Headers: map[string]string{
				"X-Trace-ID":      "trace-\"quoted\"",
				"bad header ; {}": "kept safe",
			},
			Body: "{\"token\":\"mock-\\\"quoted\\\"\",\"lines\":\"one\\ntwo\"}",
		}},
	}})

	for _, want := range []string{
		"location = /v1/token",
		"if ngx.var.host ~= \"api.example.test\" then",
		"if ngx.var.request_method ~= \"POST\" then",
		"ngx.sleep(0.25)",
		"ngx.status = 202",
		"ngx.header[\"X-Trace-ID\"] = \"trace-\\\"quoted\\\"\"",
		"ngx.header[\"bad-header\"] = \"kept safe\"",
		"ngx.print(\"{\\\"token\\\":\\\"mock-\\\\\\\"quoted\\\\\\\"\\\",\\\"lines\\\":\\\"one\\\\ntwo\\\"}\")",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("expected config to contain %q, got %s", want, config)
		}
	}
}

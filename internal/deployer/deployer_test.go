package deployer

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	k8s := fake.NewSimpleClientset()
	k8s.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() != "target-scan-1" {
			return false, nil, nil
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "target-scan-1", Namespace: "aegis-war-room-scan-1"},
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
	if _, err := k8s.CoreV1().Services(response.Namespace).Get(ctx, "svc-scan-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("service was not created: %v", err)
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
	k8s := fake.NewSimpleClientset()
	createdDeployments := map[string]*appsv1.Deployment{}
	createdServices := map[string]*corev1.Service{}
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
					"env": {"API_URL": "http://api:8080"},
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

	webDeployment := createdDeployments["web-frontend"]
	if webDeployment == nil {
		t.Fatalf("web deployment was not created: %v", err)
	}
	webContainer := webDeployment.Spec.Template.Spec.Containers[0]
	if webContainer.Image != "nginx:1.27" {
		t.Fatalf("unexpected web image: %s", webContainer.Image)
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
	if webService.Spec.Ports[0].Port != 80 || webService.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Fatalf("unexpected web service port: %#v", webService.Spec.Ports[0])
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

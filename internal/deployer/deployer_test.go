package deployer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

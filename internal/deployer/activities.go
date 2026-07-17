package deployer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (a *Activities) CreateSandbox(ctx context.Context, req SandboxRequest) (response SandboxResponse, err error) {
	req.ScanID = strings.TrimSpace(req.ScanID)
	req.TargetImage = strings.TrimSpace(req.TargetImage)
	if req.ScanID == "" {
		return SandboxResponse{}, errors.New("scan_id is required")
	}
	topology, err := req.parseTopology()
	if err != nil {
		return SandboxResponse{}, err
	}
	if req.TargetImage == "" && topology == nil {
		return SandboxResponse{}, errors.New("target_image or topology_json is required")
	}

	namespace := sandboxNamespace(req.ScanID)
	if err := validateSandboxNamespace(namespace); err != nil {
		return SandboxResponse{}, err
	}
	defer func() {
		if err == nil {
			return
		}
		failureResponse := response
		failureResponse.Namespace = namespace
		debugRef, bundleErr := a.createSandboxDebugBundle(ctx, namespace, req.ScanID, failureResponse, topology)
		if bundleErr != nil {
			log.Printf("[CreateSandbox] scan=%s debug bundle collection failed after sandbox error: %v", req.ScanID, bundleErr)
			return
		}
		log.Printf("[CreateSandbox] scan=%s debug bundle collected after sandbox error: %s", req.ScanID, debugRef)
	}()

	podName := "target-" + req.ScanID
	serviceName := "svc-" + req.ScanID

	log.Printf(
		"[CreateSandbox] scan=%s image=%s namespace=%s pod=%s service=%s",
		req.ScanID,
		req.TargetImage,
		namespace,
		podName,
		serviceName,
	)

	log.Printf("[CreateSandbox] scan=%s creating namespace %s", req.ScanID, namespace)
	if err := a.createNamespace(ctx, namespace, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}
	log.Printf("[CreateSandbox] scan=%s namespace %s ready", req.ScanID, namespace)
	if err := a.createSandboxNetworkPolicy(ctx, namespace, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}

	if topology != nil {
		response, err = a.createTopologySandbox(ctx, req.ScanID, namespace, topology, req.PreferredEndpointWorkload)
		if err == nil {
			if debugRef, bundleErr := a.createSandboxDebugBundle(ctx, namespace, req.ScanID, response, topology); bundleErr == nil {
				response.DebugBundle = debugRef
			} else {
				log.Printf("[CreateSandbox] scan=%s debug bundle collection failed: %v", req.ScanID, bundleErr)
			}
		}
		return response, err
	}

	mockDNSIP, err := a.createExternalDependencyMock(ctx, namespace, req.ScanID, nil)
	if err != nil {
		return SandboxResponse{}, err
	}

	log.Printf("[CreateSandbox] scan=%s creating pod %s/%s", req.ScanID, namespace, podName)
	if err := a.createPod(ctx, namespace, podName, req.ScanID, req.TargetImage, mockDNSIP); err != nil {
		return SandboxResponse{}, err
	}
	log.Printf("[CreateSandbox] scan=%s pod %s/%s created", req.ScanID, namespace, podName)

	log.Printf("[CreateSandbox] scan=%s creating service %s/%s", req.ScanID, namespace, serviceName)
	if err := a.createService(ctx, namespace, serviceName, req.ScanID); err != nil {
		return SandboxResponse{}, err
	}
	log.Printf("[CreateSandbox] scan=%s service %s/%s created", req.ScanID, namespace, serviceName)

	log.Printf("[CreateSandbox] scan=%s waiting for pod %s/%s to become Ready", req.ScanID, namespace, podName)
	if err := a.waitForPodReady(ctx, namespace, podName, time.Minute); err != nil {
		return SandboxResponse{}, err
	}

	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:80", serviceName, namespace)
	log.Printf("[CreateSandbox] scan=%s sandbox ready endpoint=%s", req.ScanID, endpoint)
	response = SandboxResponse{
		Namespace: namespace,
		Endpoint:  endpoint,
	}
	if debugRef, bundleErr := a.createSandboxDebugBundle(ctx, namespace, req.ScanID, response, nil); bundleErr == nil {
		response.DebugBundle = debugRef
	} else {
		log.Printf("[CreateSandbox] scan=%s debug bundle collection failed: %v", req.ScanID, bundleErr)
	}
	return response, nil
}

func (a *Activities) DestroySandbox(ctx context.Context, scanID string) (string, error) {
	scanID = strings.TrimSpace(scanID)
	if scanID == "" {
		return "", errors.New("scan_id is required")
	}

	namespace := sandboxNamespace(scanID)
	if err := validateSandboxNamespace(namespace); err != nil {
		return "", err
	}
	if keepSandboxNamespaces() {
		log.Printf("[DestroySandbox] scan=%s retaining namespace %s because %s=true", scanID, namespace, retainSandboxEnv)
		return "RETAINED", nil
	}

	log.Printf("[DestroySandbox] scan=%s deleting namespace %s", scanID, namespace)
	err := a.k8s.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		log.Printf("[DestroySandbox] scan=%s namespace %s already absent", scanID, namespace)
		return "CLEANED", nil
	}
	if err != nil {
		return "", fmt.Errorf("delete namespace %s: %w", namespace, err)
	}
	log.Printf("[DestroySandbox] scan=%s namespace %s deleted", scanID, namespace)
	return "CLEANED", nil
}

package deployer

import (
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const (
	defaultTemporalHost      = "localhost:7233"
	defaultTemporalNamespace = "default"
	defaultTaskQueue         = "DEPLOYER_TASK_QUEUE"
	sandboxNamespacePrefix   = "aegis-war-room-"
	topologyMockSecretPrefix = "aegis-mock-secret"
	sandboxRuntimeClassName  = "gvisor"
	sandboxRuntimeClassEnv   = "SANDBOX_RUNTIME_CLASS"
	retainSandboxEnv         = "RETAIN_WAR_ROOM_NAMESPACES"
	externalMockName         = "external-api-mock"
	externalMockHTTPPort     = int32(8080)
	externalMockHTTPSPort    = int32(8443)
	externalMockDNSPort      = int32(53)
	fallbackKubeDNSIP        = "10.43.0.10"
	fallbackExternalMockIP   = "10.43.0.200"
)

var (
	newK8sClient                   = newKubernetesClient
	temporalDial                   = client.Dial
	newWorker                      = worker.New
	temporalConnectMaxAttempts     = 0
	temporalConnectRetryDelay      = 2 * time.Second
	topologyDeploymentReadyTimeout = 15 * time.Second
)

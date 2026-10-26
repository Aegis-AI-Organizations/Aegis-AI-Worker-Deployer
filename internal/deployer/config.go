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

	// Resource quota env vars and defaults — override to tune per-sandbox limits.
	sandboxQuotaRequestsCPUEnv    = "SANDBOX_QUOTA_REQUESTS_CPU"
	sandboxQuotaRequestsMemEnv    = "SANDBOX_QUOTA_REQUESTS_MEMORY"
	sandboxQuotaLimitsCPUEnv      = "SANDBOX_QUOTA_LIMITS_CPU"
	sandboxQuotaLimitsMemEnv      = "SANDBOX_QUOTA_LIMITS_MEMORY"
	sandboxQuotaPodsEnv           = "SANDBOX_QUOTA_PODS"
	// Generous defaults: a topology with 10 services × 128Mi default request
	// + external-api-mock (3 containers) already exceeds 512Mi.
	// Operators can tighten via env vars after profiling real workloads.
	defaultSandboxQuotaRequestsCPU = "2"
	defaultSandboxQuotaRequestsMem = "4Gi"
	defaultSandboxQuotaLimitsCPU   = "8"
	defaultSandboxQuotaLimitsMem   = "8Gi"
	defaultSandboxQuotaPods        = "20"

	// LimitRange env vars and defaults — defaults prevent ResourceQuota from
	// rejecting pods that declare no explicit resource requests/limits.
	sandboxLRDefaultCPUEnv    = "SANDBOX_LIMITRANGE_DEFAULT_CPU"
	sandboxLRDefaultMemEnv    = "SANDBOX_LIMITRANGE_DEFAULT_MEMORY"
	sandboxLRDefaultReqCPUEnv = "SANDBOX_LIMITRANGE_DEFAULT_REQUEST_CPU"
	sandboxLRDefaultReqMemEnv = "SANDBOX_LIMITRANGE_DEFAULT_REQUEST_MEMORY"
	sandboxLRMaxCPUEnv        = "SANDBOX_LIMITRANGE_MAX_CPU"
	sandboxLRMaxMemEnv        = "SANDBOX_LIMITRANGE_MAX_MEMORY"
	defaultSandboxLRDefaultCPU    = "200m"
	defaultSandboxLRDefaultMem    = "256Mi"
	defaultSandboxLRDefaultReqCPU = "100m"
	defaultSandboxLRDefaultReqMem = "128Mi"
	defaultSandboxLRMaxCPU        = "1"
	defaultSandboxLRMaxMem        = "1Gi"
	externalMockName         = "external-api-mock"
	externalMockHTTPPort     = int32(8080)
	externalMockHTTPSPort    = int32(8443)
	externalMockDNSPort      = int32(53)
	fallbackKubeDNSIP        = "10.43.0.10"
	fallbackExternalMockIP   = "10.43.0.200"
	defaultAegisRedactImage  = "ghcr.io/aegis-ai-organizations/aegis-ai-agent:latest"
)

var (
	newK8sClient                   = newKubernetesClient
	temporalDial                   = client.Dial
	newWorker                      = worker.New
	temporalConnectMaxAttempts     = 0
	temporalConnectRetryDelay      = 2 * time.Second
	topologyDeploymentReadyTimeout = 60 * time.Second
	databaseSeedJobReadyTimeout    = 5 * time.Minute
)

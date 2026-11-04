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
	sandboxQuotaRequestsCPUEnv = "SANDBOX_QUOTA_REQUESTS_CPU"
	sandboxQuotaRequestsMemEnv = "SANDBOX_QUOTA_REQUESTS_MEMORY"
	sandboxQuotaLimitsCPUEnv   = "SANDBOX_QUOTA_LIMITS_CPU"
	sandboxQuotaLimitsMemEnv   = "SANDBOX_QUOTA_LIMITS_MEMORY"
	sandboxQuotaPodsEnv        = "SANDBOX_QUOTA_PODS"
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
	sandboxLRDefaultCPUEnv        = "SANDBOX_LIMITRANGE_DEFAULT_CPU"
	sandboxLRDefaultMemEnv        = "SANDBOX_LIMITRANGE_DEFAULT_MEMORY"
	sandboxLRDefaultReqCPUEnv     = "SANDBOX_LIMITRANGE_DEFAULT_REQUEST_CPU"
	sandboxLRDefaultReqMemEnv     = "SANDBOX_LIMITRANGE_DEFAULT_REQUEST_MEMORY"
	sandboxLRMaxCPUEnv            = "SANDBOX_LIMITRANGE_MAX_CPU"
	sandboxLRMaxMemEnv            = "SANDBOX_LIMITRANGE_MAX_MEMORY"
	defaultSandboxLRDefaultCPU    = "200m"
	defaultSandboxLRDefaultMem    = "256Mi"
	defaultSandboxLRDefaultReqCPU = "100m"
	defaultSandboxLRDefaultReqMem = "128Mi"
	defaultSandboxLRMaxCPU        = "1"
	// 5Gi, not 1Gi: must stay >= the jvm resource profile's 4Gi limit
	// below, or Kubernetes rejects any pod using that profile (LimitRange
	// Max caps what a container's own explicit limits may declare).
	defaultSandboxLRMaxMem = "5Gi"

	// Resource profiles — named resource classes for the simple
	// (non-topology) sandbox path, resolved by resourcesForProfile in
	// kubernetes_resources.go. Unlike the LimitRange defaults above, these
	// aren't global: a caller opts in per-request via
	// SandboxRequest.ResourceProfile when a target needs more than a
	// lightweight microservice (e.g. a JVM target OOM-killed by the
	// 256Mi/200m default — see docker-vuln/cve-2017-5638's README).
	resourceProfileJVM             = "jvm"
	sandboxProfileJVMReqCPUEnv     = "SANDBOX_PROFILE_JVM_REQUEST_CPU"
	sandboxProfileJVMReqMemEnv     = "SANDBOX_PROFILE_JVM_REQUEST_MEMORY"
	sandboxProfileJVMLimCPUEnv     = "SANDBOX_PROFILE_JVM_LIMIT_CPU"
	sandboxProfileJVMLimMemEnv     = "SANDBOX_PROFILE_JVM_LIMIT_MEMORY"
	defaultSandboxProfileJVMReqCPU = "500m"
	defaultSandboxProfileJVMReqMem = "512Mi"
	defaultSandboxProfileJVMLimCPU = "1"
	// 4Gi, not 1536Mi: measured live against aegis-cve-2017-5638.
	// 1536Mi OOM-killed it ~49s after boot even idle, from its own
	// Maven/Jetty startup spike (idle steady-state settles ~550-600Mi).
	// 2560Mi survived idle but still got OOM-killed ~90s into an actual
	// nuclei scan -- the CVE-tagged template run's concurrent request
	// load against this old Jetty app drives memory well past idle
	// usage. 4Gi leaves real headroom for that load instead of chasing
	// the exact minimum.
	defaultSandboxProfileJVMLimMem = "4Gi"

	externalMockName        = "external-api-mock"
	externalMockHTTPPort    = int32(8080)
	externalMockHTTPSPort   = int32(8443)
	externalMockDNSPort     = int32(53)
	fallbackKubeDNSIP       = "10.43.0.10"
	fallbackExternalMockIP  = "10.43.0.200"
	defaultAegisRedactImage = "ghcr.io/aegis-ai-organizations/aegis-ai-agent:latest"
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

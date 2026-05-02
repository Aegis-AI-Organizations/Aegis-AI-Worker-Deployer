# 🏗️ Deployer Architecture: Digital Twin Rendering Engine

The **Aegis AI Deployer Worker** is the platform's "Replication Engine". Written in **Go** for its native Kubernetes integration, it is responsible for reconstructing isolated, high-fidelity replicas of client infrastructure to enable safe offensive testing.

---

## 🏗️ Core Design Principles

The Deployer Worker is built for **accuracy**, **isolation**, and **speed**:

1. **Graph-to-Manifest Translation**: Reconstructs infrastructure state by querying the **Neo4j** attack graph and generating corresponding Kubernetes manifests and network policies.
2. **Deterministic Sandboxing**: Provisions ephemeral environments in dedicated `sandbox-*` namespaces, ensuring every Digital Twin is a clean, isolated replica of the target.
3. **Internal Orchestration**: Directed by the `Aegis-AI-Brain` via **Temporal**, ensuring that the provisioning of complex "Mission Zones" is resilient and reliable.

---

## 🔐 Security & Sandbox Bounding

Since these workers deploy potentially vulnerable infrastructure, they implement extreme security boundaries.

- **Kernel Isolation (gVisor)**: Every Digital Twin component is provisioned using the **gVisor** (`runsc`) runtime class, providing strong syscall-level isolation from the underlying host.
- **Micro-segmentation (Cilium)**: Automatically applies strict **Cilium Cluster-wide Network Policies** to the sandbox, preventing any unintended lateral movement to the internal Aegis Core or other sandboxes.
- **RBAC Segmentation**: The worker operates with a restricted **ServiceAccount**, authorized only to manage resources within the explicitly designated sandbox namespaces.

---

## 🌊 Dynamic Scaling (KEDA)

The Deployer pool is managed by **KEDA** (Kubernetes Event-Driven Autoscaling) to handle parallel campaign deployments:

- **Demand-Reactive Scaling**: Adjusts the worker count based on the number of "Deployment" tasks in the Temporal queue.
- **Scale-to-Zero**: When no deployments are active, the pool scales down to **0 replicas**, optimizing resource usage.

---

## 🛰️ Deployment Logic

The worker handles the translation of:
- **Service Topologies**: Auto-scaling groups, deployments, and stateful sets.
- **Network Metadata**: Ingress rules, load balancers, and internal DNS entries.
- **Security Contexts**: Replicating the exact privilege level and runtime constraints of the real target.

```mermaid
graph TD
    Neo4j[(Neo4j Graph)] -- "Topology Data" --> Deployer[Deployer Worker (Go)]
    Deployer -- "Render" --> K8s[Target K8s Cluster]
    K8s -- "Deploy" --> Sandbox[Isolated Digital Twin]
    Sandbox -- "Ready" --> Brain[Brain Orchestrator]
```

---

*Aegis AI Infrastructure & Digital Twins — 2026*

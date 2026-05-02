# 🏗️ Aegis AI — Deployer Worker

**Project ID:** AEGIS-CORE-2026

> The **Aegis AI Deployer Worker** is the platform's "Digital Twin" rendering engine. Orchestrated by **Temporal**, these Go-based workers reconstruct isolated, exact replicas of a client's live infrastructure based on telemetry graphs, enabling safe and intensive offensive security testing.

---

## 🏗️ Role in the Ecosystem

The Deployer acts as the bridge between "Ingested Data" and "Actionable Sandboxes".

- **Digital Twin Generation**: Translates network graphs from Neo4j into deployable Kubernetes manifests.
- **Sandbox Provisioning**: Automates the lifecycle of ephemeral environments in isolated `sandbox-*` namespaces.
- **Security Bounding**: Configures **gVisor** runtimes and **Cilium** policies for the cloned environment to prevent cluster-wide leaks.

```mermaid
graph TD
    Neo4j[(Neo4j Graph)] -- "Topology Data" --> Deployer[Deployer Worker (Go)]
    Deployer -- "Render" --> K8s[Target K8s Cluster]
    K8s -- "Deploy" --> Sandbox[Isolated Digital Twin]
    Sandbox -- "Ready" --> Brain[Brain Orchestrator]
```

---

## 🛠️ Tech Stack

| Component | Technology | Version |
|---|---|---|
| Language | **Go** | 1.22+ |
| K8s Integration | **client-go**, Helm SDK | — |
| Graph Engine | Neo4j Go Driver | — |
| Orchestration | **Temporal SDK** | 1.x |

---

## 🔐 Security & Isolation

- **Kernel Isolation**: All Digital Twins are deployed using the **gVisor** (`runsc`) runtime class.
- **Air-Gapped Networking**: Enforces strict network policies that block all outbound traffic from the sandbox to the internal Aegis Core or the public internet (unless whitelisted).
- **Environment Sanitation**: Automatically rotates secrets and sanitizes sensitive data before cloning.

---

## 🐳 Deployment (Kubernetes)

Autoscaled by **KEDA** to handle parallel twin generation for multiple customer campaigns.

```yaml
# Helm values example
image:
  repository: ghcr.io/aegis-ai/aegis-worker-deployer
  tag: latest
keda:
  enabled: true
  minReplicas: 0
  maxReplicas: 10
```

---

## 🛠️ Development

```bash
# Run locally
go run main.go

# Run unit tests
go test ./...
```

---

*Aegis AI — Infrastructure & Digital Twins — 2026*

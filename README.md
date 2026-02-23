# 🏗️ Aegis AI - Worker Pool: Deployer

**Project ID:** AEGIS-CORE-2026

## 🎯 System Architecture & Role
The **Aegis AI Worker Deployer** is the "Digital Twin" rendering engine. Managed by the Temporal Control Plane, this Go-based worker reconstructs an exact, isolated replica of the client's live infrastructure based on the telemetry and network graphs ingested by our Rust Agents.

* **Tech Stack:** Go (Golang), Kubernetes `client-go`, Neo4j Driver (Graph Traversal), ClickHouse SDK.
* **Role:**
  * **State Ingestion:** Reads the current architectural state of the client (services, network policies, open ports) built from the `aegis-worker-ingest` pipeline.
  * **On-the-fly Translation:** Converts graph topologies and logs into deployable Kubernetes manifests or Terraform states.
  * **Clone Deployment:** Provisions the ephemeral sandboxes in an isolated environment (utilizing gVisor for kernel isolation and Cilium for strict network bounding).
  * **Handoff:** Signals the Brain orchestrator that the Digital Twin is ready for offensive security testing.
* **Architecture Justification:** Go is the industry standard for cloud-native orchestration. Its native integration with Kubernetes SDKs allows the Deployer to rapidly translate complex graphs into live, sandboxed environments with minimal latency.

## 🔐 Security & DevSecOps Mandates
* **Air-Gapped Execution:** The cloned environments deployed by this worker have absolutely no outbound internet access or routing back to the client's real production environment.
* **Secret Injection:** All cluster credentials and deployment identities are injected dynamically into the worker's memory via **Infisical**.

## 🐳 Docker Deployment
Designed to scale horizontally to support parallel Digital Twin generation for multiple clients.

```bash
docker pull ghcr.io/aegis-ai/aegis-worker-deployer:latest

infisical run --env=prod -- docker run -d \
  --name aegis-worker-deployer \
  --read-only \
  --cap-drop=ALL \
  --security-opt no-new-privileges:true \
  --user 10001:10001 \
  -e TEMPORAL_ADDRESS="temporal.aegis.internal:7233" \
  -e INFISICAL_TOKEN=$INFISICAL_TOKEN \
  ghcr.io/aegis-ai/aegis-worker-deployer:latest

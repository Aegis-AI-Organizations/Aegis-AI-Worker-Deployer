# API Reference: Worker Deployer

The `Aegis-AI-Worker-Deployer` exposes a fast and secure API for interacting with the infrastructure.

## Internal gRPC Endpoints (v1)

### `DeployWorker(DeployRequest) returns (DeployResponse)`
- **Description**: Submits a request to instantiate a new isolated worker (e.g., Pentest or Ingest worker).
- **DeployRequest Structure**:
  - `tenant_id` (string): Identifies the customer/environment.
  - `worker_type` (Enum): E.g., `WORKER_PENTEST`, `WORKER_INGEST`.
  - `target_metadata` (map): The target domains or contexts the worker must assess.
- **DeployResponse**:
  - `worker_id` (string): A UUID to track this running instance.
  - `status` (Enum): `DEPLOYING`, `FAILED`, `READY`.

### `GetWorkerStatus(StatusRequest) returns (StatusResponse)`
- **Description**: Returns the real-time resource usage and heartbeat of a specific worker container.

### `TerminateWorker(TerminateRequest) returns (TerminateResponse)`
- **Description**: Signals the deployer to force-kill a worker container and purge its temporary data volumes.

*Note: All API endpoints are tightly coupled with the Protobuf specs defined in the `Aegis-AI-Proto` repository.*

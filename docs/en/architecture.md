# Aegis AI Worker Deployer Architecture

## Overview

The `Aegis-AI-Worker-Deployer` is a core microservice built in Go. Its primary responsibility is to handle the deployment and orchestration of dynamic Pentest/Fixer workers within the Aegis AI ecosystem.

## Flow

1. **Trigger**: Receives a deployment request via gRPC or message queue (e.g., RabbitMQ).
2. **Validation**: Validates the payload and required permissions.
3. **Execution**: Interfaces with the target infrastructure (Kubernetes via client-go, or Docker Engine) to spawn the isolated worker container.
4. **Monitoring**: Monitors the lifecycle of the deployed worker until task completion.
5. **Teardown**: Gracefully tears down the worker and cleans up allocated resources.

## Project Structure (Go Standard Layout)

- **`cmd/deployer/`**: Contains the main application entry point (`main.go`). It binds dependencies and starts the service.
- **`internal/deployer/`**: Contains the private application and business logic. Code here is not importable by other repositories.
- **`pkg/`**: (Optional) Library code that is safe to use by external applications.
- **`docs/`**: Documentation and design specifications.

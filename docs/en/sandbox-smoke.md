# Sandbox Smoke Validation

This repository includes a canonical vulnerable sandbox fixture and a smoke/debug script to validate that the deployer produced an isolated, reachable clone.

## Files

- `examples/sandbox-topology.vulnerable-webapp.json`: topology payload for the vulnerable web application plus PostgreSQL.
- `examples/sandbox-request.vulnerable-webapp.json`: complete `CreateSandbox` request shape with `scan_id`, preferred endpoint, and topology.
- `scripts/sandbox_smoke.sh`: local validation helper for Kubernetes resources, endpoint reachability, events, and optional cleanup.

## Expected Sandbox Shape

The fixture deploys:

- `vulnerable-webapp`: HTTP entrypoint on service port `80`, container port `5000`.
- `postgres`: PostgreSQL service on port `5432`.
- `depends_on`: the app declares `postgres` as a dependency so the deployer creates the database workload first.
- `wait_for`: the app gets an init container that waits for `postgres:5432` before starting.
- `config_files` and `empty_dirs`: the app receives mounted configuration and writable upload storage.
- `stateful` and `headless`: PostgreSQL is rendered as a StatefulSet with a stable DNS identity.
- App-to-DB mapping through `DATABASE_URL` and `POSTGRES_*` env vars.
- External dependency URL pointing to `https://payments.example.test/api/v1`, which should be intercepted by the sandbox external mock/DNS layer.

The expected pentest loop is:

1. Brain sends the topology request to the Deployer worker.
2. Deployer creates namespace `aegis-war-room-<scan_id>`.
3. Deployer creates workloads, services, default-deny egress, and external mock DNS/HTTP/HTTPS services.
4. Brain seeds PostgreSQL with realistic data and `aegis-flag-1234`.
5. Worker Pentest exploits SQLi and reports `loot_proof` and `exfiltrated_data`.

## Fidelity Fields Supported By The Deployer

Topology workloads can now express these Kubernetes fidelity controls:

- Startup: `command`, `args`, `working_dir`, `init_containers`.
- Dependency startup: `depends_on` for creation order and `wait_for` for init-container TCP waits such as `postgres:5432`.
- Files and storage: `config_files`, `secret_files`, and `empty_dirs` mounted into the workload container.
- Workload identity: `stateful: true` renders a StatefulSet instead of a Deployment.
- Services: `service.headless`, `service.type`, named ports, and same-namespace DNS aliases through `service.aliases`.
- Resources: Kubernetes `resources.requests` and `resources.limits`.
- Security: container `security_context` and pod `pod_security_context`.
- Readiness policy: `required: true` fails sandbox creation if that workload never becomes ready.

Fields intentionally handled outside this worker:

- Database dump restore from MinIO/S3 belongs in Brain seeding activities.
- Scenario-rich external API responses and traffic capture require the external mock runtime to persist request logs.
- Ingress/TLS virtual-host exposure requires cluster ingress/controller configuration and should be added with infra manifests.

## Smoke Test

After a sandbox is created, run:

```bash
scripts/sandbox_smoke.sh --scan-id smoke-sqli-001
```

If the CreateSandbox response returned a different endpoint, pass it explicitly:

```bash
scripts/sandbox_smoke.sh \
  --scan-id smoke-sqli-001 \
  --endpoint http://vulnerable-webapp.aegis-war-room-smoke-sqli-001.svc.cluster.local:80
```

To inspect an arbitrary namespace:

```bash
scripts/sandbox_smoke.sh --namespace aegis-war-room-scan-123
```

To clean up after inspection:

```bash
scripts/sandbox_smoke.sh --scan-id smoke-sqli-001 --cleanup
```

The script refuses to delete namespaces that do not start with `aegis-war-room-`.

## What Good Looks Like

- The topology JSON validates.
- Namespace exists.
- Network policies are present.
- Services include the app, database, and external mock.
- Dependency order creates database/cache workloads before dependent apps when `depends_on` is present.
- Deployments become available or show actionable errors.
- Pods become ready or show actionable errors.
- Endpoint responds from inside the namespace.
- Events do not show unresolved image pulls, denied mounts, or scheduling failures.

## Debugging

Useful commands when the smoke script fails:

```bash
kubectl describe namespace aegis-war-room-smoke-sqli-001
kubectl get all -n aegis-war-room-smoke-sqli-001 -o wide
kubectl describe pod -n aegis-war-room-smoke-sqli-001 <pod-name>
kubectl logs -n aegis-war-room-smoke-sqli-001 deploy/vulnerable-webapp
kubectl get events -n aegis-war-room-smoke-sqli-001 --sort-by=.lastTimestamp
```

If pods fail with `ImagePullBackOff`, publish or retag the vulnerable app image used in `examples/sandbox-topology.vulnerable-webapp.json`.

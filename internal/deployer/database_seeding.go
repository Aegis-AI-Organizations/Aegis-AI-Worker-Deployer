package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	defaultSeedFlag           = "aegis-flag-1234"
	databaseSeedFailurePrefix = "Database Seeding Failed"
)

var (
	emailPattern      = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	awsKeyPattern     = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	apiKeyPattern     = regexp.MustCompile(`(?i)(sk|pk|rk)_(live|test)_[A-Za-z0-9_\-]+`)
	jwtPattern        = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)
	passwordPattern   = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key)'?\s*[,=]\s*'[^']+'`)
	personNamePattern = regexp.MustCompile(`'([A-Z][a-z]{2,})\s+([A-Z][a-z]{2,})'`)
)

func (a *Activities) SeedTargetDatabases(ctx context.Context, req SeedDatabaseRequest) (SeedDatabaseResponse, error) {
	req.ScanID = strings.TrimSpace(req.ScanID)
	if req.ScanID == "" {
		return SeedDatabaseResponse{}, fmt.Errorf("scan_id is required")
	}
	if strings.TrimSpace(req.SeedFlag) == "" {
		req.SeedFlag = defaultSeedFlag
	}
	namespace := sandboxNamespace(req.ScanID)
	if err := validateSandboxNamespace(namespace); err != nil {
		return SeedDatabaseResponse{}, err
	}

	targets := normalizeDatabaseSchemas(req.DatabaseSchemas, namespace)
	if len(targets) == 0 {
		return SeedDatabaseResponse{Namespace: namespace, SeedFlag: req.SeedFlag, Anonymized: true}, nil
	}
	seeded := make([]string, 0, len(targets))
	for _, target := range targets {
		sql := req.RestoreSQL
		if strings.TrimSpace(sql) == "" {
			sql = syntheticSeedSQL(req.SeedFlag)
		}
		sql = anonymizeSQL(sql)
		if !strings.Contains(sql, req.SeedFlag) {
			sql += "\n-- aegis seed flag: " + req.SeedFlag + "\n"
		}
		jobName, err := a.createDatabaseSeedJob(ctx, namespace, req.ScanID, target, sql)
		if err != nil {
			return SeedDatabaseResponse{}, err
		}
		if err := a.waitForDatabaseSeedJob(ctx, namespace, jobName, databaseSeedJobReadyTimeout); err != nil {
			return SeedDatabaseResponse{}, err
		}
		seeded = append(seeded, databaseSeedTargetName(target))
	}
	sort.Strings(seeded)
	if err := a.updateSeedDebugContract(ctx, namespace, req.ScanID, targets, seeded, req.SeedFlag); err != nil {
		return SeedDatabaseResponse{}, err
	}
	return SeedDatabaseResponse{
		Namespace:     namespace,
		Seeded:        seeded,
		SeededCount:   len(seeded),
		SeedFlag:      req.SeedFlag,
		Anonymized:    true,
		DebugBundle:   fmt.Sprintf("configmap/%s", sandboxDebugBundleName(req.ScanID)),
		TrafficBundle: fmt.Sprintf("configmap/%s", externalMockTrafficConfigMapName(req.ScanID)),
		SeedContract:  fmt.Sprintf("configmap/%s#seed_contract.json", sandboxDebugBundleName(req.ScanID)),
	}, nil
}

func normalizeDatabaseSchemas(schemas []DatabaseSchema, namespace string) []DatabaseSchema {
	targets := make([]DatabaseSchema, 0, len(schemas))
	for _, schema := range schemas {
		if !strings.EqualFold(strings.TrimSpace(schema.Engine), "postgresql") && !strings.EqualFold(strings.TrimSpace(schema.Engine), "postgres") {
			continue
		}
		if schema.Port <= 0 {
			schema.Port = 5432
		}
		if strings.TrimSpace(schema.DatabaseName) == "" {
			schema.DatabaseName = "postgres"
		}
		if strings.TrimSpace(schema.Username) == "" {
			schema.Username = "postgres"
		}
		if strings.TrimSpace(schema.Password) == "" {
			schema.Password = "postgres"
		}
		if strings.TrimSpace(schema.Host) == "" {
			source := kubernetesName(firstNonEmpty(schema.SourceContainerName, schema.SourceContainerID))
			if source == "" || source == "workload" {
				continue
			}
			schema.Host = fmt.Sprintf("%s.%s.svc.cluster.local", source, namespace)
		}
		targets = append(targets, schema)
	}
	return targets
}

func syntheticSeedSQL(seedFlag string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS users (
    id SERIAL PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    password TEXT NOT NULL,
    role TEXT NOT NULL,
    seed_flag TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS credentials (
    id SERIAL PRIMARY KEY,
    owner_email TEXT NOT NULL UNIQUE,
    service_name TEXT NOT NULL,
    username TEXT NOT NULL,
    password TEXT NOT NULL,
    seed_flag TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_tokens (
    id SERIAL PRIMARY KEY,
    owner_email TEXT NOT NULL UNIQUE,
    token_name TEXT NOT NULL,
    token_value TEXT NOT NULL,
    scopes TEXT NOT NULL,
    seed_flag TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS projects (
    id SERIAL PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    owner_email TEXT NOT NULL,
    visibility TEXT NOT NULL,
    seed_flag TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_events (
    id SERIAL PRIMARY KEY,
    actor_email TEXT NOT NULL,
    action TEXT NOT NULL,
    ip_address TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    seed_flag TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS app_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    seed_flag TEXT NOT NULL
);
INSERT INTO users (email, password, role, seed_flag) VALUES
    ('admin@example.test', 'aegis-mock-secret', 'admin', '%s'),
    ('analyst@example.test', 'aegis-mock-secret', 'user', '%s'),
    ('service-account@example.test', 'aegis-mock-secret', 'service', '%s')
ON CONFLICT (email) DO UPDATE SET password = EXCLUDED.password, role = EXCLUDED.role, seed_flag = EXCLUDED.seed_flag;
INSERT INTO credentials (owner_email, service_name, username, password, seed_flag) VALUES
    ('admin@example.test', 'internal-admin', 'admin', 'aegis-mock-secret', '%s'),
    ('analyst@example.test', 'staging-s3', 'analyst', 'aegis-mock-secret', '%s')
ON CONFLICT (owner_email) DO UPDATE SET service_name = EXCLUDED.service_name, username = EXCLUDED.username, password = EXCLUDED.password, seed_flag = EXCLUDED.seed_flag;
INSERT INTO api_tokens (owner_email, token_name, token_value, scopes, seed_flag) VALUES
    ('service-account@example.test', 'ci-runner', 'sk_test_aegis_mock', 'read:projects,write:artifacts', '%s')
ON CONFLICT (owner_email) DO UPDATE SET token_name = EXCLUDED.token_name, token_value = EXCLUDED.token_value, scopes = EXCLUDED.scopes, seed_flag = EXCLUDED.seed_flag;
INSERT INTO projects (slug, owner_email, visibility, seed_flag) VALUES
    ('customer-portal', 'admin@example.test', 'private', '%s'),
    ('internal-runbook', 'analyst@example.test', 'internal', '%s')
ON CONFLICT (slug) DO UPDATE SET owner_email = EXCLUDED.owner_email, visibility = EXCLUDED.visibility, seed_flag = EXCLUDED.seed_flag;
INSERT INTO audit_events (actor_email, action, ip_address, metadata, seed_flag) VALUES
    ('admin@example.test', 'login.success', '198.51.100.10', '{"user_agent":"aegis-mock-browser"}'::jsonb, '%s'),
    ('service-account@example.test', 'token.used', '203.0.113.25', '{"token_name":"ci-runner"}'::jsonb, '%s');
INSERT INTO app_settings (key, value, seed_flag) VALUES
    ('smtp.host', 'smtp.mock.aegis.test', '%s'),
    ('stripe.webhook_secret', 'aegis-mock-secret', '%s'),
    ('oauth.client_secret', 'aegis-mock-secret', '%s')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, seed_flag = EXCLUDED.seed_flag;
`, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag, seedFlag)
}

func anonymizeSQL(sql string) string {
	sql = emailPattern.ReplaceAllString(sql, "user@example.test")
	sql = awsKeyPattern.ReplaceAllString(sql, "AKIA0000000000000000")
	sql = apiKeyPattern.ReplaceAllString(sql, "sk_test_aegis_mock")
	sql = jwtPattern.ReplaceAllString(sql, "aegis-mock-token")
	sql = passwordPattern.ReplaceAllStringFunc(sql, func(value string) string {
		prefix := value[:strings.LastIndex(value, "'")+1]
		return prefix + "aegis-mock-secret'"
	})
	sql = personNamePattern.ReplaceAllString(sql, "'PRENOM_1 NOM_1'")
	return sql
}

func (a *Activities) createDatabaseSeedJob(ctx context.Context, namespace, scanID string, target DatabaseSchema, sql string) (string, error) {
	name := kubernetesName("db-seed-" + databaseSeedTargetName(target))
	if len(name) > 50 {
		name = kubernetesName("db-seed-" + target.DatabaseName)
	}
	mode := int32(420)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-sql", Labels: topologyLabels(scanID, name)},
		Data: map[string]string{
			"seed.sql": sql,
		},
	}
	if _, err := a.k8s.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create database seed configmap %s/%s: %w", namespace, configMap.Name, err)
	}
	backoff := int32(1)
	redactImage := strings.TrimSpace(getenv("AEGIS_REDACT_IMAGE", defaultAegisRedactImage))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: topologyLabels(scanID, name)},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: topologyLabels(scanID, name)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{{
						Name:            "aegis-redact",
						Image:           redactImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/aegis-redact"},
						Args:            []string{"--input", "/aegis-input/seed.sql", "--mode", "sql", "--output", "/aegis/seed.sql"},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "seed-sql-source", MountPath: "/aegis-input/seed.sql", SubPath: "seed.sql", ReadOnly: true},
							{Name: "seed-sql-redacted", MountPath: "/aegis"},
						},
					}},
					Containers: []corev1.Container{{
						Name:            "psql",
						Image:           "postgres:16-alpine",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c"},
						Args:            []string{"psql -v ON_ERROR_STOP=1 -f /aegis/seed.sql"},
						Env: []corev1.EnvVar{
							{Name: "PGHOST", Value: target.Host},
							{Name: "PGPORT", Value: fmt.Sprintf("%d", target.Port)},
							{Name: "PGDATABASE", Value: target.DatabaseName},
							{Name: "PGUSER", Value: target.Username},
							{Name: "PGPASSWORD", Value: target.Password},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "seed-sql-redacted", MountPath: "/aegis"}},
					}},
					Volumes: []corev1.Volume{
						{Name: "seed-sql-source", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name}, DefaultMode: &mode}}},
						{Name: "seed-sql-redacted", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	if _, err := a.k8s.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create database seed job %s/%s: %w", namespace, job.Name, err)
	}
	return job.Name, nil
}

func (a *Activities) waitForDatabaseSeedJob(ctx context.Context, namespace, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	job, err := a.k8s.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return databaseSeedFailuref("read job %s/%s: %w", namespace, name, err)
	}
	if done, err := databaseSeedJobTerminalError(namespace, job); done || err != nil {
		return err
	}

	watcher, err := a.k8s.BatchV1().Jobs(namespace).Watch(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()})
	if err != nil {
		return databaseSeedFailuref("watch job %s/%s: %w", namespace, name, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return databaseSeedFailuref("timeout waiting for job %s/%s after %s: %w", namespace, name, timeout, ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return databaseSeedFailuref("watch closed before job %s/%s completed", namespace, name)
			}
			if event.Type == watch.Error {
				return databaseSeedFailuref("watch error for job %s/%s", namespace, name)
			}
			job, ok := event.Object.(*batchv1.Job)
			if !ok || job.Name != name {
				continue
			}
			if done, err := databaseSeedJobTerminalError(namespace, job); done || err != nil {
				return err
			}
		}
	}
}

func databaseSeedJobTerminalError(namespace string, job *batchv1.Job) (bool, error) {
	for _, condition := range job.Status.Conditions {
		switch condition.Type {
		case batchv1.JobComplete:
			if condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		case batchv1.JobFailed:
			if condition.Status == corev1.ConditionTrue {
				return true, databaseSeedFailuref("job %s/%s failed: %s", namespace, job.Name, firstNonEmpty(condition.Message, condition.Reason, "unknown failure"))
			}
		}
	}
	return false, nil
}

func databaseSeedFailuref(format string, args ...any) error {
	return fmt.Errorf("%s: "+format, append([]any{databaseSeedFailurePrefix}, args...)...)
}

func databaseSeedTargetName(target DatabaseSchema) string {
	return firstNonEmpty(target.DatabaseName, target.SourceContainerName, target.Host, "postgres")
}

func (a *Activities) updateSeedDebugContract(ctx context.Context, namespace, scanID string, targets []DatabaseSchema, seeded []string, seedFlag string) error {
	name := sandboxDebugBundleName(scanID)
	configMap, err := a.k8s.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get sandbox debug bundle %s/%s: %w", namespace, name, err)
	}
	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}
	contract := map[string]any{
		"version":        "2026-07-15",
		"scan_id":        scanID,
		"namespace":      namespace,
		"seed_flag":      seedFlag,
		"seeded_targets": seeded,
		"tables": []string{
			"users",
			"credentials",
			"api_tokens",
			"projects",
			"audit_events",
			"app_settings",
		},
		"personas": []map[string]string{
			{"email": "admin@example.test", "role": "admin", "password": "aegis-mock-secret"},
			{"email": "analyst@example.test", "role": "user", "password": "aegis-mock-secret"},
			{"email": "service-account@example.test", "role": "service", "password": "aegis-mock-secret"},
		},
		"database_targets": sanitizeDatabaseSchemas(targets),
		"crewai_guidance": []string{
			"Generate only synthetic data marked with seed_flag.",
			"Keep credentials non-production and use aegis-mock-secret for secret-like values.",
			"Prefer realistic business objects linked to users, projects, tokens, and audit events.",
		},
	}
	data, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal seed debug contract: %w", err)
	}
	configMap.Data["seed_contract.json"] = string(data)
	if _, err := a.k8s.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update sandbox debug bundle %s/%s: %w", namespace, name, err)
	}
	return nil
}

func sanitizeDatabaseSchemas(schemas []DatabaseSchema) []map[string]any {
	result := make([]map[string]any, 0, len(schemas))
	for _, schema := range schemas {
		result = append(result, map[string]any{
			"engine":                schema.Engine,
			"host":                  schema.Host,
			"port":                  schema.Port,
			"database_name":         schema.DatabaseName,
			"username":              schema.Username,
			"source_container_id":   schema.SourceContainerID,
			"source_container_name": schema.SourceContainerName,
			"tables":                schema.Tables,
		})
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func sandboxDebugBundleName(scanID string) string { return kubernetesName("aegis-debug-" + scanID) }

func externalMockTrafficConfigMapName(scanID string) string {
	return kubernetesName("external-mock-traffic-" + scanID)
}

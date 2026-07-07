package deployer

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultSeedFlag = "aegis-flag-1234"

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
		if err := a.createDatabaseSeedJob(ctx, namespace, req.ScanID, target, sql); err != nil {
			return SeedDatabaseResponse{}, err
		}
		seeded = append(seeded, databaseSeedTargetName(target))
	}
	sort.Strings(seeded)
	return SeedDatabaseResponse{
		Namespace:     namespace,
		Seeded:        seeded,
		SeededCount:   len(seeded),
		SeedFlag:      req.SeedFlag,
		Anonymized:    true,
		DebugBundle:   fmt.Sprintf("configmap/%s", sandboxDebugBundleName(req.ScanID)),
		TrafficBundle: fmt.Sprintf("configmap/%s", externalMockTrafficConfigMapName(req.ScanID)),
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
INSERT INTO users (email, password, role, seed_flag) VALUES
    ('admin@example.test', 'aegis-mock-secret', 'admin', '%s'),
    ('analyst@example.test', 'aegis-mock-secret', 'user', '%s')
ON CONFLICT (email) DO UPDATE SET password = EXCLUDED.password, role = EXCLUDED.role, seed_flag = EXCLUDED.seed_flag;
INSERT INTO credentials (owner_email, service_name, username, password, seed_flag) VALUES
    ('admin@example.test', 'internal-admin', 'admin', 'aegis-mock-secret', '%s')
ON CONFLICT (owner_email) DO UPDATE SET service_name = EXCLUDED.service_name, username = EXCLUDED.username, password = EXCLUDED.password, seed_flag = EXCLUDED.seed_flag;
`, seedFlag, seedFlag, seedFlag)
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

func (a *Activities) createDatabaseSeedJob(ctx context.Context, namespace, scanID string, target DatabaseSchema, sql string) error {
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
		return fmt.Errorf("create database seed configmap %s/%s: %w", namespace, configMap.Name, err)
	}
	backoff := int32(1)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: topologyLabels(scanID, name)},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: topologyLabels(scanID, name)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
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
						VolumeMounts: []corev1.VolumeMount{{Name: "seed-sql", MountPath: "/aegis/seed.sql", SubPath: "seed.sql", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{Name: "seed-sql", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name}, DefaultMode: &mode}}}},
				},
			},
		},
	}
	if _, err := a.k8s.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create database seed job %s/%s: %w", namespace, job.Name, err)
	}
	return nil
}

func databaseSeedTargetName(target DatabaseSchema) string {
	return firstNonEmpty(target.DatabaseName, target.SourceContainerName, target.Host, "postgres")
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

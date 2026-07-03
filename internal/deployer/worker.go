package deployer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func Start() {
	if err := Run(context.Background()); err != nil {
		log.Fatalf("deployer worker stopped: %v", err)
	}
}

func Run(ctx context.Context) error {
	_ = ctx

	k8s, err := newK8sClient()
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	var temporalClient client.Client
	temporalHost := getenv("TEMPORAL_HOST", defaultTemporalHost)
	temporalNamespace := getenv("TEMPORAL_NAMESPACE", defaultTemporalNamespace)
	log.Printf("Connecting Deployer worker to Temporal at %s (namespace=%s)...", temporalHost, temporalNamespace)
	temporalOptions, err := temporalClientOptions(temporalHost, temporalNamespace)
	if err != nil {
		return err
	}

	for attempt := 1; ; attempt++ {
		if err = ctx.Err(); err != nil {
			return fmt.Errorf("connect temporal cancelled: %w", err)
		}

		temporalClient, err = temporalDial(temporalOptions)
		if err == nil {
			break
		}

		if temporalConnectMaxAttempts > 0 && attempt >= temporalConnectMaxAttempts {
			return fmt.Errorf("connect temporal: %w", err)
		}

		log.Printf("Failed to connect to Temporal at %s (attempt %d): %v", temporalHost, attempt, err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("connect temporal cancelled: %w", ctx.Err())
		case <-time.After(temporalConnectRetryDelay):
		}
	}
	defer temporalClient.Close()

	stopTimeout := envDurationSeconds("TEMPORAL_WORKER_STOP_TIMEOUT_SECONDS", 14*time.Minute)
	w := newWorker(temporalClient, getenv("DEPLOYER_TASK_QUEUE", defaultTaskQueue), worker.Options{
		WorkerStopTimeout: stopTimeout,
	})
	activities := NewActivities(k8s)
	w.RegisterActivityWithOptions(activities.CreateSandbox, activity.RegisterOptions{Name: "CreateSandbox"})
	w.RegisterActivityWithOptions(activities.DestroySandbox, activity.RegisterOptions{Name: "DestroySandbox"})

	log.Printf("Aegis AI Worker Deployer listening on queue %s", getenv("DEPLOYER_TASK_QUEUE", defaultTaskQueue))
	return w.Run(worker.InterruptCh())
}

func temporalClientOptions(host, namespace string) (client.Options, error) {
	options := client.Options{
		HostPort:  host,
		Namespace: namespace,
	}
	if !envBool("TEMPORAL_TLS_ENABLE") {
		return options, nil
	}

	tlsConfig, err := temporalTLSConfig()
	if err != nil {
		return client.Options{}, fmt.Errorf("configure temporal tls: %w", err)
	}
	options.ConnectionOptions.TLS = tlsConfig
	return options, nil
}

func temporalTLSConfig() (*tls.Config, error) {
	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: getenv("TEMPORAL_TLS_SERVER_NAME", ""),
	}

	if caPath := getenv("TEMPORAL_TLS_CA_PATH", ""); caPath != "" {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read ca certificate: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("parse ca certificate")
		}
		config.RootCAs = roots
	}

	certPath := getenv("TEMPORAL_TLS_CERT_PATH", "")
	keyPath := getenv("TEMPORAL_TLS_KEY_PATH", "")
	if certPath == "" && keyPath == "" {
		return config, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("TEMPORAL_TLS_CERT_PATH and TEMPORAL_TLS_KEY_PATH must be configured together")
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	config.Certificates = []tls.Certificate{certificate}
	return config, nil
}

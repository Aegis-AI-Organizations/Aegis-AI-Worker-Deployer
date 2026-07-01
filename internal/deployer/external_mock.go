package deployer

import (
	"context"
	"fmt"
	"log"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func (a *Activities) createExternalDependencyMock(ctx context.Context, namespace, scanID string) (string, error) {
	mockIP, err := a.createExternalMockService(ctx, namespace, scanID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(mockIP) == "" {
		mockIP = fallbackExternalMockIP
	}
	kubeDNSIP := a.kubeDNSIP(ctx)
	if err := a.createExternalMockConfigMap(ctx, namespace, scanID, mockIP, kubeDNSIP); err != nil {
		return "", err
	}
	if err := a.createExternalMockDeployment(ctx, namespace, scanID); err != nil {
		return "", err
	}
	return mockIP, nil
}

func (a *Activities) createExternalMockService(ctx context.Context, namespace, scanID string) (string, error) {
	labels := externalMockLabels(scanID)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   externalMockName,
			Labels: labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt32(externalMockHTTPPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "https",
					Port:       443,
					TargetPort: intstr.FromInt32(externalMockHTTPSPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "dns-tcp",
					Port:       externalMockDNSPort,
					TargetPort: intstr.FromInt32(externalMockDNSPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "dns-udp",
					Port:       externalMockDNSPort,
					TargetPort: intstr.FromInt32(externalMockDNSPort),
					Protocol:   corev1.ProtocolUDP,
				},
			},
		},
	}

	created, err := a.k8s.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := a.k8s.CoreV1().Services(namespace).Get(ctx, externalMockName, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("read external mock service %s/%s: %w", namespace, externalMockName, getErr)
		}
		log.Printf("[CreateSandbox] service %s/%s already exists", namespace, externalMockName)
		return existing.Spec.ClusterIP, nil
	}
	if err != nil {
		return "", fmt.Errorf("create external mock service %s/%s: %w", namespace, externalMockName, err)
	}
	log.Printf("[CreateSandbox] service %s/%s created for external dependency mocking", namespace, externalMockName)
	return created.Spec.ClusterIP, nil
}

func (a *Activities) createExternalMockConfigMap(ctx context.Context, namespace, scanID, mockIP, kubeDNSIP string) error {
	if strings.TrimSpace(kubeDNSIP) == "" {
		kubeDNSIP = fallbackKubeDNSIP
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   externalMockName,
			Labels: externalMockLabels(scanID),
		},
		Data: map[string]string{
			"Corefile": externalMockCorefile(mockIP, kubeDNSIP),
			"default.conf": fmt.Sprintf(`server {
    listen %d default_server;
    access_log off;
    location / {
        add_header Content-Type text/plain;
        return 200 '';
    }
}

server {
    listen %d ssl default_server;
    access_log off;
    ssl_certificate /etc/nginx/mock-tls/tls.crt;
    ssl_certificate_key /etc/nginx/mock-tls/tls.key;
    location / {
        add_header Content-Type text/plain;
        return 200 '';
    }
}
`, externalMockHTTPPort, externalMockHTTPSPort),
		},
	}
	_, err := a.k8s.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] configmap %s/%s already exists", namespace, externalMockName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create external mock configmap %s/%s: %w", namespace, externalMockName, err)
	}
	log.Printf("[CreateSandbox] configmap %s/%s created for external dependency mocking", namespace, externalMockName)
	return nil
}

func (a *Activities) createExternalMockDeployment(ctx context.Context, namespace, scanID string) error {
	replicas := int32(1)
	labels := externalMockLabels(scanID)
	mode := int32(420)
	runtimeClassName := a.sandboxRuntimeClassName(ctx)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   externalMockName,
			Labels: labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RuntimeClassName: runtimeClassName,
					Containers: []corev1.Container{
						{
							Name:            "http",
							Image:           "nginx:1.27-alpine",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/bin/sh", "-c"},
							Args: []string{
								"mkdir -p /etc/nginx/mock-tls && " +
									"openssl req -x509 -nodes -newkey rsa:2048 " +
									"-keyout /etc/nginx/mock-tls/tls.key " +
									"-out /etc/nginx/mock-tls/tls.crt " +
									"-days 1 -subj /CN=external-api-mock >/dev/null 2>&1 && " +
									"nginx -g 'daemon off;'",
							},
							Ports: []corev1.ContainerPort{{
								Name:          "http",
								ContainerPort: externalMockHTTPPort,
								Protocol:      corev1.ProtocolTCP,
							}, {
								Name:          "https",
								ContainerPort: externalMockHTTPSPort,
								Protocol:      corev1.ProtocolTCP,
							}},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "external-mock-config",
									MountPath: "/etc/nginx/conf.d/default.conf",
									SubPath:   "default.conf",
									ReadOnly:  true,
								},
								{
									Name:      "external-mock-tls",
									MountPath: "/etc/nginx/mock-tls",
								},
							},
						},
						{
							Name:            "dns",
							Image:           "coredns/coredns:1.11.3",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            []string{"-conf", "/etc/coredns/Corefile"},
							Ports: []corev1.ContainerPort{
								{Name: "dns-tcp", ContainerPort: externalMockDNSPort, Protocol: corev1.ProtocolTCP},
								{Name: "dns-udp", ContainerPort: externalMockDNSPort, Protocol: corev1.ProtocolUDP},
							},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "external-mock-config",
								MountPath: "/etc/coredns/Corefile",
								SubPath:   "Corefile",
								ReadOnly:  true,
							}},
						},
					},
					Volumes: []corev1.Volume{{
						Name: "external-mock-config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: externalMockName},
								DefaultMode:          &mode,
							},
						},
					}, {
						Name: "external-mock-tls",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}},
				},
			},
		},
	}
	_, err := a.k8s.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Printf("[CreateSandbox] deployment %s/%s already exists", namespace, externalMockName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create external mock deployment %s/%s: %w", namespace, externalMockName, err)
	}
	log.Printf("[CreateSandbox] deployment %s/%s created for external dependency mocking", namespace, externalMockName)
	return nil
}

func (a *Activities) kubeDNSIP(ctx context.Context) string {
	for _, serviceName := range []string{"kube-dns", "coredns"} {
		service, err := a.k8s.CoreV1().Services("kube-system").Get(ctx, serviceName, metav1.GetOptions{})
		if err == nil && strings.TrimSpace(service.Spec.ClusterIP) != "" {
			return service.Spec.ClusterIP
		}
	}
	return fallbackKubeDNSIP
}

func externalMockCorefile(mockIP, kubeDNSIP string) string {
	return fmt.Sprintf(`cluster.local:53 {
    errors
    cache 30
    forward . %s
}
.:53 {
    errors
    template IN A . {
        match .*
        answer "{{ .Name }} 60 IN A %s"
    }
}
`, kubeDNSIP, mockIP)
}

func externalMockLabels(scanID string) map[string]string {
	return map[string]string{
		"app":                          externalMockName,
		"app.kubernetes.io/name":       externalMockName,
		"app.kubernetes.io/managed-by": "aegis-worker-deployer",
		"aegis-scan":                   scanID,
	}
}

func sandboxDNSConfig(namespace, nameserver string) *corev1.PodDNSConfig {
	return &corev1.PodDNSConfig{
		Nameservers: []string{nameserver},
		Searches: []string{
			namespace + ".svc.cluster.local",
			"svc.cluster.local",
			"cluster.local",
		},
		Options: []corev1.PodDNSConfigOption{{
			Name:  "ndots",
			Value: ptrString("5"),
		}},
	}
}

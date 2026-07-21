package controllers

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

const signerSecretName = "proxy-server-ca"

func newOwnerReference(config *proxyv1alpha1.ManagedProxyConfiguration) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         proxyv1alpha1.GroupVersion.String(),
		Kind:               "ManagedProxyConfiguration",
		Name:               config.Name,
		UID:                config.UID,
		BlockOwnerDeletion: ptr.To(true),
	}
}

func newServiceAccount(config *proxyv1alpha1.ManagedProxyConfiguration) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.Spec.ProxyServer.Namespace,
			Name:      common.AddonName,
			OwnerReferences: []metav1.OwnerReference{
				newOwnerReference(config),
			},
		},
	}
}

func newProxySecret(config *proxyv1alpha1.ManagedProxyConfiguration, caData []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.Spec.ProxyServer.Namespace,
			Name:      signerSecretName,
			OwnerReferences: []metav1.OwnerReference{
				newOwnerReference(config),
			},
		},
		Data: map[string][]byte{
			"ca.crt": caData,
		},
	}
}
func newProxyService(config *proxyv1alpha1.ManagedProxyConfiguration) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.Spec.ProxyServer.Namespace,
			Name:      config.Spec.ProxyServer.InClusterServiceName,
			OwnerReferences: []metav1.OwnerReference{
				newOwnerReference(config),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				common.LabelKeyComponentName: common.ComponentNameProxyServer,
			},
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name: "proxy-server",
					Port: 8090,
				},
				{
					Name: "agent-server",
					Port: 8091,
				},
			},
		},
	}
}

func newProxyServerDeployment(config *proxyv1alpha1.ManagedProxyConfiguration, imagePullPolicy string, tlsConfig *sdktls.TLSConfig) *appsv1.Deployment {
	deployAnnotations := map[string]string{}
	if hash := tlsConfigHash(tlsConfig); hash != "" {
		deployAnnotations[common.AnnotationKeyTLSConfigHash] = hash
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.Spec.ProxyServer.Namespace,
			Name:      config.Name,
			OwnerReferences: []metav1.OwnerReference{
				newOwnerReference(config),
			},
			Labels: map[string]string{
				common.AnnotationKeyConfigurationGeneration: strconv.Itoa(int(config.Generation)),
			},
			Annotations: deployAnnotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &config.Spec.ProxyServer.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					common.LabelKeyComponentName: common.ComponentNameProxyServer,
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						common.LabelKeyComponentName: common.ComponentNameProxyServer,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: common.AddonName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:            common.ComponentNameProxyServer,
							Image:           config.Spec.ProxyServer.Image,
							ImagePullPolicy: corev1.PullPolicy(imagePullPolicy), // TODO @xuezhaojun, the image pull policy should be configurable and by default should be IfNotPresent. Will update this later to a better solution. Currently, using the image pull policy from the command line flag.
							Command: []string{
								"/proxy-server",
							},
							// TLS configuration from ocm-tls-profile ConfigMap is injected
							// via proxyServerArgs (both --cipher-suites and --tls-min-version)
							Args: proxyServerArgs(config, tlsConfig),
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								Privileged:               ptr.To(false),
								RunAsNonRoot:             ptr.To(true),
								ReadOnlyRootFilesystem:   ptr.To(true),
								AllowPrivilegeEscalation: ptr.To(false),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "proxy-server-ca-certs",
									ReadOnly:  true,
									MountPath: "/etc/server-ca-pki/",
								},
								{
									Name:      "proxy-server-certs",
									ReadOnly:  true,
									MountPath: "/etc/server-pki/",
								},
								{
									Name:      "proxy-agent-certs",
									ReadOnly:  true,
									MountPath: "/etc/agent-pki/",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "proxy-server-ca-certs",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: signerSecretName,
								},
							},
						},
						{
							Name: "proxy-server-certs",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: config.Spec.Authentication.Dump.Secrets.SigningProxyServerSecretName,
								},
							},
						},
						{
							Name: "proxy-agent-certs",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: config.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName,
								},
							},
						},
					},
					NodeSelector: config.Spec.ProxyServer.NodePlacement.NodeSelector,
					Tolerations:  config.Spec.ProxyServer.NodePlacement.Tolerations,
				},
			},
		},
	}
}

func newProxyServerRole(config *proxyv1alpha1.ManagedProxyConfiguration) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.Spec.ProxyServer.Namespace,
			Name:      "cluster-proxy-addon-agent:portforward",
			OwnerReferences: []metav1.OwnerReference{
				newOwnerReference(config),
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Verbs:     []string{"*"},
				Resources: []string{"pods", "pods/portforward"},
			},
		},
	}
}

func newProxyServerRoleBinding(config *proxyv1alpha1.ManagedProxyConfiguration) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.Spec.ProxyServer.Namespace,
			Name:      "cluster-proxy-addon-agent:portforward",
			OwnerReferences: []metav1.OwnerReference{
				newOwnerReference(config),
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "Role",
			Name: "cluster-proxy-addon-agent:portforward",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: rbacv1.GroupKind,
				Name: common.SubjectGroupClusterProxy,
			},
		},
	}

}

// newProxyServerNetworkPolicy returns a NetworkPolicy for the ANP proxy-server pods.
// Applied only when ManagedProxyConfiguration.spec.networkPolicies.enabled=true.
// Allows ingress on service ports 8090/8091 (empty from for portability);
// allows DNS and Kubernetes API egress.
func newProxyServerNetworkPolicy(config *proxyv1alpha1.ManagedProxyConfiguration) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port8090 := intstr.FromInt32(8090)
	port8091 := intstr.FromInt32(8091)
	port53 := intstr.FromInt32(53)
	port5353 := intstr.FromInt32(5353)
	port443 := intstr.FromInt32(443)
	port6443 := intstr.FromInt32(6443)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.Spec.ProxyServer.Namespace,
			Name:      config.Name + "-proxy-server",
			OwnerReferences: []metav1.OwnerReference{
				newOwnerReference(config),
			},
			Labels: map[string]string{
				common.LabelKeyComponentName: common.ComponentNameProxyServer,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					common.LabelKeyComponentName: common.ComponentNameProxyServer,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Empty From: port-based allow (ingress / in-cluster clients).
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &port8090},
						{Protocol: &tcp, Port: &port8091},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &port53},
						{Protocol: &tcp, Port: &port53},
						{Protocol: &udp, Port: &port5353},
						{Protocol: &tcp, Port: &port5353},
					},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &port443},
						{Protocol: &tcp, Port: &port6443},
					},
				},
			},
		},
	}
}

func proxyServerArgs(config *proxyv1alpha1.ManagedProxyConfiguration, tlsConfig *sdktls.TLSConfig) []string {
	args := append([]string{
		"--server-count=" + strconv.Itoa(int(config.Spec.ProxyServer.Replicas)),
		"--proxy-strategies=destHost",
		"--server-ca-cert=/etc/server-ca-pki/ca.crt",
		"--server-cert=/etc/server-pki/tls.crt",
		"--server-key=/etc/server-pki/tls.key",
		"--cluster-ca-cert=/etc/server-ca-pki/ca.crt",
		"--cluster-cert=/etc/agent-pki/tls.crt",
		"--cluster-key=/etc/agent-pki/tls.key",
	}, config.Spec.ProxyServer.AdditionalArgs...)

	if tlsConfig != nil {
		if len(tlsConfig.CipherSuites) > 0 {
			args = append(args, "--cipher-suites="+sdktls.CipherSuitesToString(tlsConfig.CipherSuites))
		}

		if tlsConfig.MinVersion != 0 {
			args = append(args, "--tls-min-version="+sdktls.VersionToString(tlsConfig.MinVersion))
		}
	}

	return args
}

func tlsConfigHash(tlsConfig *sdktls.TLSConfig) string {
	if tlsConfig == nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(sdktls.CipherSuitesToString(tlsConfig.CipherSuites)))
	h.Write([]byte{0})
	h.Write([]byte(sdktls.VersionToString(tlsConfig.MinVersion)))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

package agent

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"open-cluster-management.io/cluster-proxy/pkg/addon/operator/authentication/selfsigned"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	csrv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	"open-cluster-management.io/addon-framework/pkg/agent"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ agent.AgentAddon = &proxyAgent{}

const (
	ProxyAgentSignerName = "open-cluster-management.io/proxy-agent-signer"
)

func NewProxyAgent(runtimeClient client.Client, nativeClient kubernetes.Interface, signer selfsigned.SelfSigner) agent.AgentAddon {
	return &proxyAgent{
		runtimeClient: runtimeClient,
		nativeClient:  nativeClient,
		selfSigner:    signer,
	}
}

type proxyAgent struct {
	runtimeClient client.Reader
	nativeClient  kubernetes.Interface
	selfSigner    selfsigned.SelfSigner
}

func (p *proxyAgent) Manifests(managedCluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) ([]runtime.Object, error) {
	// prepping
	clusterAddon := &addonv1alpha1.ClusterManagementAddOn{}
	if err := p.runtimeClient.Get(context.TODO(), types.NamespacedName{
		Name: addon.Name,
	}, clusterAddon); err != nil {
		return nil, err
	}
	proxyConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
	if err := p.runtimeClient.Get(context.TODO(), types.NamespacedName{
		Name: clusterAddon.Spec.AddOnConfiguration.CRName,
	}, proxyConfig); err != nil {
		return nil, err
	}

	// find the referenced proxy load-balancer prescribed in the proxy config if there's any
	var proxyServerLoadBalancer *corev1.Service
	if proxyConfig.Spec.ProxyServer.Entrypoint.Type == proxyv1alpha1.EntryPointTypeLoadBalancerService {
		entrySvc, err := p.nativeClient.CoreV1().
			Services(proxyConfig.Spec.ProxyServer.Namespace).
			Get(context.TODO(),
				proxyConfig.Spec.ProxyServer.Entrypoint.LoadBalancerService.Name,
				metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, "failed getting proxy loadbalancer")
		}
		if len(entrySvc.Status.LoadBalancer.Ingress) == 0 {
			return nil, fmt.Errorf("the load-balancer service for proxy-server ingress is not yet provisioned")
		}
		proxyServerLoadBalancer = entrySvc
	}

	objs := []runtime.Object{
		newNamespace(addon.Spec.InstallNamespace),
		newCASecret(addon.Spec.InstallNamespace, AgentCASecretName, p.selfSigner.CAData()),
		newClusterService(addon.Spec.InstallNamespace, managedCluster.Name),
		newAgentDeployment(managedCluster.Name, addon.Spec.InstallNamespace, proxyConfig, proxyServerLoadBalancer),
	}
	return objs, nil
}

func (p *proxyAgent) GetAgentAddonOptions() agent.AgentAddonOptions {
	return agent.AgentAddonOptions{
		AddonName:       common.AddonName,
		InstallStrategy: agent.InstallAllStrategy(common.AddonInstallNamespace),
		Registration: &agent.RegistrationOption{
			CSRConfigurations: func(cluster *clusterv1.ManagedCluster) []addonv1alpha1.RegistrationConfig {
				return []addonv1alpha1.RegistrationConfig{
					{
						SignerName: ProxyAgentSignerName,
						Subject: addonv1alpha1.Subject{
							User: common.SubjectUserClusterProxyAgent,
							Groups: []string{
								common.SubjectGroupClusterProxy,
							},
						},
					},
					{
						SignerName: csrv1.KubeAPIServerClientSignerName,
						Subject: addonv1alpha1.Subject{
							User: common.SubjectUserClusterAddonAgent,
							Groups: []string{
								common.SubjectGroupClusterProxy,
							},
						},
					},
				}
			},
			CSRApproveCheck: func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn, csr *csrv1.CertificateSigningRequest) bool {
				return cluster.Spec.HubAcceptsClient
			},
			PermissionConfig: p.setupPermission,
			CSRSign: func(csr *csrv1.CertificateSigningRequest) []byte {
				if csr.Spec.SignerName != ProxyAgentSignerName {
					return nil
				}
				b, _ := pem.Decode(csr.Spec.Request)
				parsed, err := x509.ParseCertificateRequest(b.Bytes)
				if err != nil {
					return nil
				}
				validity := time.Hour * 24 * 180
				caCert := p.selfSigner.CA().Config.Certs[0]
				tmpl := &x509.Certificate{
					SerialNumber:       caCert.SerialNumber,
					Subject:            parsed.Subject,
					DNSNames:           parsed.DNSNames,
					IPAddresses:        parsed.IPAddresses,
					EmailAddresses:     parsed.EmailAddresses,
					URIs:               parsed.URIs,
					PublicKeyAlgorithm: parsed.PublicKeyAlgorithm,
					PublicKey:          parsed.PublicKey,
					Extensions:         parsed.Extensions,
					ExtraExtensions:    parsed.ExtraExtensions,
					IsCA:               false,
					KeyUsage:           x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
					ExtKeyUsage: []x509.ExtKeyUsage{
						x509.ExtKeyUsageServerAuth,
						x509.ExtKeyUsageClientAuth,
					},
				}
				now := time.Now()
				tmpl.NotBefore = now
				tmpl.NotAfter = now.Add(validity)

				rsaKey := p.selfSigner.CA().Config.Key.(*rsa.PrivateKey)
				der, err := x509.CreateCertificate(
					rand.Reader,
					tmpl,
					p.selfSigner.CA().Config.Certs[0],
					parsed.PublicKey,
					rsaKey)
				if err != nil {
					return nil
				}
				return pem.EncodeToMemory(&pem.Block{
					Type:  "CERTIFICATE",
					Bytes: der,
				})
			},
		},
	}
}

func (p *proxyAgent) setupPermission(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) error {
	// prepping
	clusterAddon := &addonv1alpha1.ClusterManagementAddOn{}
	if err := p.runtimeClient.Get(context.TODO(), types.NamespacedName{
		Name: addon.Name,
	}, clusterAddon); err != nil {
		return err
	}
	proxyConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
	if err := p.runtimeClient.Get(context.TODO(), types.NamespacedName{
		Name: clusterAddon.Spec.AddOnConfiguration.CRName,
	}, proxyConfig); err != nil {
		return err
	}

	namespace := cluster.Name

	// TODO: consider switching to SSA at some point
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "cluster-proxy-addon-agent",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         addonv1alpha1.GroupVersion.String(),
					Kind:               "ManagedClusterAddOn",
					Name:               addon.Name,
					BlockOwnerDeletion: pointer.Bool(true),
					UID:                addon.UID,
				},
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"coordination.k8s.io"},
				Verbs:     []string{"*"},
				Resources: []string{"leases"},
			},
		},
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "cluster-proxy-addon-agent",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         addonv1alpha1.GroupVersion.String(),
					Kind:               "ManagedClusterAddOn",
					Name:               addon.Name,
					BlockOwnerDeletion: pointer.Bool(true),
					UID:                addon.UID,
				},
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "Role",
			Name: "cluster-proxy-addon-agent",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: rbacv1.GroupKind,
				Name: common.SubjectGroupClusterProxy,
			},
		},
	}

	if _, err := p.nativeClient.RbacV1().Roles(namespace).Create(
		context.TODO(),
		role,
		metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	if _, err := p.nativeClient.RbacV1().RoleBindings(namespace).Create(
		context.TODO(),
		roleBinding,
		metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

const (
	ApiserverNetworkProxyLabelAddon     = "open-cluster-management.io/addon"
	ApiserverNetworkProxyLabelComponent = "open-cluster-management.io/component"

	AgentSecretName   = "cluster-proxy-open-cluster-management.io-proxy-agent-signer-client-cert"
	AgentCASecretName = "cluster-proxy-ca"
)

func newNamespace(targetNamespace string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: targetNamespace,
		},
	}
}

func newAgentDeployment(clusterName, targetNamespace string, proxyConfig *proxyv1alpha1.ManagedProxyConfiguration, proxyLoadBalancer *corev1.Service) *appsv1.Deployment {
	// this is how we set the right ingress endpoint for proxy servers to
	// receive handshakes from proxy agents:
	// 1. upon "Hostname" type, use the prescribed hostname directly
	// 2. upon "LoadBalancerService" type, use the first entry in the ip lists
	// 3. otherwise defaulted to the in-cluster service endpoint
	serviceEntryPoint := proxyConfig.Spec.ProxyServer.InClusterServiceName + "." + proxyConfig.Spec.ProxyServer.Namespace
	addonAgentArgs := []string{
		"--hub-kubeconfig=/etc/kubeconfig/kubeconfig",
		"--cluster-name=" + clusterName,
		"--proxy-server-namespace=" + proxyConfig.Spec.ProxyServer.Namespace,
	}
	switch proxyConfig.Spec.ProxyServer.Entrypoint.Type {
	case proxyv1alpha1.EntryPointTypeHostname:
		serviceEntryPoint = proxyConfig.Spec.ProxyServer.Entrypoint.Hostname.Value
	case proxyv1alpha1.EntryPointTypeLoadBalancerService:
		serviceEntryPoint = proxyLoadBalancer.Status.LoadBalancer.Ingress[0].IP
	case proxyv1alpha1.EntryPointTypePortForward:
		serviceEntryPoint = "127.0.0.1"
		addonAgentArgs = append(addonAgentArgs,
			"--enable-port-forward-proxy=true")
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyConfig.Name + "-" + common.ComponentNameProxyAgent,
			Namespace: targetNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &proxyConfig.Spec.ProxyAgent.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					ApiserverNetworkProxyLabelAddon:     common.AddonName,
					ApiserverNetworkProxyLabelComponent: common.ComponentNameProxyAgent,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						ApiserverNetworkProxyLabelAddon:     common.AddonName,
						ApiserverNetworkProxyLabelComponent: common.ComponentNameProxyAgent,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            common.ComponentNameProxyAgent,
							Image:           proxyConfig.Spec.ProxyAgent.Image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"/proxy-agent",
							},
							Args: []string{
								"--proxy-server-host=" + serviceEntryPoint,
								"--agent-identifiers=" +
									"host=" + clusterName + "&" +
									"host=" + clusterName + "." + targetNamespace + "&" +
									"host=" + clusterName + "." + targetNamespace + ".svc.cluster.local",
								"--ca-cert=/etc/ca/ca.crt",
								"--agent-cert=/etc/tls/tls.crt",
								"--agent-key=/etc/tls/tls.key",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "ca",
									ReadOnly:  true,
									MountPath: "/etc/ca/",
								},
								{
									Name:      "hub",
									ReadOnly:  true,
									MountPath: "/etc/tls/",
								},
							},
						},
						{
							Name:  "addon-agent",
							Image: proxyConfig.Spec.ProxyAgent.Image,
							Command: []string{
								"/agent",
							},
							Args: addonAgentArgs,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "hub-kubeconfig",
									ReadOnly:  true,
									MountPath: "/etc/kubeconfig/",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "ca",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: AgentCASecretName,
								},
							},
						},
						{
							Name: "hub",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: AgentSecretName,
								},
							},
						},
						{
							Name: "hub-kubeconfig",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "cluster-proxy-hub-kubeconfig",
								},
							},
						},
					},
				},
			},
		},
	}
}

func newCASecret(namespace, name string, caData []byte) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Data: map[string][]byte{
			selfsigned.TLSCACert: caData,
		},
	}
}

func newClusterService(namespace, name string) *corev1.Service {
	const nativeKubernetesInClusterService = "kubernetes.default.svc.cluster.local"
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: nativeKubernetesInClusterService,
		},
	}
}

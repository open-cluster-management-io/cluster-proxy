package controllers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
	"open-cluster-management.io/sdk-go/pkg/certrotation"
)

func TestProxyServerRolloutAfterHostnameCertificateRotation(t *testing.T) {
	r, config, native := newCertificateRolloutReconciler(t)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	r.SecretLister = corev1listers.NewSecretLister(indexer)
	r.newCertRotatorFunc = func(namespace, name string, sans ...string) selfsigned.CertRotation {
		return &certrotation.TargetRotation{
			Namespace: namespace,
			Name:      name,
			Validity:  time.Hour * 24,
			HostNames: sans,
			Lister:    r.SecretLister,
			Client:    native.CoreV1(),
		}
	}
	cacheSecrets := func() {
		secrets, err := native.CoreV1().Secrets(config.Spec.ProxyServer.Namespace).List(context.Background(), metav1.ListOptions{})
		requireNoError(t, err)
		for i := range secrets.Items {
			requireNoError(t, indexer.Update(secrets.Items[i].DeepCopy()))
		}
	}
	// The old controller issued certificates without the load balancer's DNS SAN.
	requireNoError(t, r.ensureRotation(config, ""))
	cacheSecrets()
	oldDeployment := newProxyServerDeployment(config, r.imagePullPolicy, nil)
	created, _, err := r.ensure(config.Generation, appsv1.SchemeGroupVersion.WithKind("Deployment"), oldDeployment)
	requireNoError(t, err)
	assert.True(t, created)

	// An existing Deployment without the fingerprint is updated once, even with an unchanged image and generation.
	modified, err := r.deployProxyServer(config)
	requireNoError(t, err)
	assert.True(t, modified)
	before := getCertificateRolloutDeployment(t, r, config)
	assert.NotEmpty(t, before.Spec.Template.Annotations[common.AnnotationKeyServingCertHash])
	modified, err = r.deployProxyServer(config)
	requireNoError(t, err)
	assert.False(t, modified)

	address, err := r.ensureEntrypoint(config)
	requireNoError(t, err)
	requireNoError(t, r.ensureRotation(config, address))
	for _, name := range []string{
		config.Spec.Authentication.Dump.Secrets.SigningProxyServerSecretName,
		config.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName,
	} {
		secret, err := native.CoreV1().Secrets(config.Spec.ProxyServer.Namespace).Get(context.Background(), name, metav1.GetOptions{})
		requireNoError(t, err)
		block, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
		if block == nil {
			t.Fatal("serving certificate is not PEM encoded")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		requireNoError(t, err)
		requireNoError(t, certificate.VerifyHostname(address))
	}

	// Keep the informer stale: deployment must use the certificates just written to the API.
	modified, err = r.deployProxyServer(config)
	requireNoError(t, err)
	assert.True(t, modified)
	after := getCertificateRolloutDeployment(t, r, config)
	assert.NotEqual(t, before.Spec.Template.Annotations, after.Spec.Template.Annotations)
	assert.Equal(t, before.Spec.Template.Spec, after.Spec.Template.Spec)
	assert.Equal(t, before.Spec.Strategy, after.Spec.Strategy)
	assert.Equal(t, int64(1), config.Generation)

	cacheSecrets()
	requireNoError(t, r.ensureRotation(config, address))
	modified, err = r.deployProxyServer(config)
	requireNoError(t, err)
	assert.False(t, modified)
	assert.Equal(t, after, getCertificateRolloutDeployment(t, r, config))
}

func TestProxyServerRolloutForServingCertificateChanges(t *testing.T) {
	testcases := []struct {
		name        string
		secretName  string
		key         string
		metadata    bool
		changeCA    bool
		wantRollout bool
	}{
		{name: "proxy server certificate", secretName: "proxy-server", key: corev1.TLSCertKey, wantRollout: true},
		{name: "proxy server key", secretName: "proxy-server", key: corev1.TLSPrivateKeyKey, wantRollout: true},
		{name: "agent server certificate", secretName: "agent-server", key: corev1.TLSCertKey, wantRollout: true},
		{name: "agent server key", secretName: "agent-server", key: corev1.TLSPrivateKeyKey, wantRollout: true},
		{name: "CA certificate", changeCA: true, wantRollout: true},
		{name: "secret metadata", secretName: "proxy-server", metadata: true},
		{name: "unmounted data", secretName: "proxy-server", key: "unmounted"},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			r, config, native := newCertificateRolloutReconciler(t)
			modified, err := r.deployProxyServer(config)
			requireNoError(t, err)
			assert.True(t, modified)
			before := getCertificateRolloutDeployment(t, r, config)

			if tc.changeCA {
				r.SelfSigner, err = selfsigned.NewGeneratedSelfSigner()
				requireNoError(t, err)
			} else {
				secret, err := native.CoreV1().Secrets(config.Spec.ProxyServer.Namespace).Get(context.Background(), tc.secretName, metav1.GetOptions{})
				requireNoError(t, err)
				if tc.metadata {
					secret.Labels = map[string]string{"example.com/label": "updated"}
					secret.Annotations = map[string]string{"example.com/annotation": "updated"}
					secret.ResourceVersion = "2"
				} else {
					secret.Data[tc.key] = []byte("updated")
				}
				_, err = native.CoreV1().Secrets(config.Spec.ProxyServer.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{})
				requireNoError(t, err)
			}

			modified, err = r.deployProxyServer(config)
			requireNoError(t, err)
			assert.Equal(t, tc.wantRollout, modified)
			after := getCertificateRolloutDeployment(t, r, config)
			if tc.wantRollout {
				assert.NotEqual(t, before.Spec.Template.Annotations, after.Spec.Template.Annotations)
				assert.Equal(t, before.Spec.Template.Spec, after.Spec.Template.Spec)
			} else {
				assert.Equal(t, before, after)
			}
			modified, err = r.deployProxyServer(config)
			requireNoError(t, err)
			assert.False(t, modified)
		})
	}
}

func TestProxyServerCertificateReadFailurePreservesDeployment(t *testing.T) {
	testcases := []struct {
		name       string
		secretName string
		key        string
		denyRead   bool
		wantError  string
	}{
		{name: "missing proxy server secret", secretName: "proxy-server", wantError: "not found"},
		{name: "missing agent server secret", secretName: "agent-server", wantError: "not found"},
		{name: "missing certificate", secretName: "proxy-server", key: corev1.TLSCertKey, wantError: "has no tls.crt"},
		{name: "empty private key", secretName: "agent-server", key: corev1.TLSPrivateKeyKey, wantError: "has no tls.key"},
		{name: "API failure", denyRead: true, wantError: "API unavailable"},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			r, config, native := newCertificateRolloutReconciler(t)
			_, err := r.deployProxyServer(config)
			requireNoError(t, err)
			before := getCertificateRolloutDeployment(t, r, config)
			if tc.denyRead {
				native.PrependReactor("get", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("API unavailable")
				})
			} else if tc.key == "" {
				requireNoError(t, native.CoreV1().Secrets(config.Spec.ProxyServer.Namespace).Delete(context.Background(), tc.secretName, metav1.DeleteOptions{}))
			} else {
				secret, err := native.CoreV1().Secrets(config.Spec.ProxyServer.Namespace).Get(context.Background(), tc.secretName, metav1.GetOptions{})
				requireNoError(t, err)
				secret.Data[tc.key] = nil
				_, err = native.CoreV1().Secrets(config.Spec.ProxyServer.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{})
				requireNoError(t, err)
			}

			modified, err := r.deployProxyServer(config)
			assert.ErrorContains(t, err, tc.wantError)
			assert.False(t, modified)
			assert.Equal(t, before, getCertificateRolloutDeployment(t, r, config))
		})
	}
}

func newCertificateRolloutReconciler(t *testing.T) (*ManagedProxyConfigurationReconciler, *proxyv1alpha1.ManagedProxyConfiguration, *kubefake.Clientset) {
	t.Helper()
	config := newTestConfig(1)
	config.Name = "cluster-proxy"
	config.Generation = 1
	config.Spec.ProxyServer.Namespace = "proxy-system"
	config.Spec.ProxyServer.InClusterServiceName = "proxy-entrypoint"
	config.Spec.ProxyServer.Image = "example.invalid/cluster-proxy:unchanged"
	config.Spec.ProxyServer.Entrypoint = &proxyv1alpha1.ManagedProxyConfigurationProxyServerEntrypoint{
		Type:                proxyv1alpha1.EntryPointTypeLoadBalancerService,
		LoadBalancerService: &proxyv1alpha1.EntryPointLoadBalancerService{Name: "proxy-agent-entrypoint"},
		Port:                8091,
	}
	config.Spec.Authentication.Dump.Secrets = proxyv1alpha1.CertificateSigningSecrets{
		SigningProxyServerSecretName: "proxy-server",
		SigningAgentServerSecretName: "agent-server",
		SigningProxyClientSecretName: "proxy-client",
	}
	var objects []runtime.Object
	for _, name := range []string{"proxy-server", "agent-server"} {
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: config.Spec.ProxyServer.Namespace, Name: name},
			Data: map[string][]byte{
				corev1.TLSCertKey:       []byte(name + " certificate"),
				corev1.TLSPrivateKeyKey: []byte(name + " key"),
			},
		})
	}
	objects = append(objects, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: config.Spec.ProxyServer.Namespace, Name: "proxy-agent-entrypoint"},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{Hostname: "proxy.elb.ap-northeast-1.amazonaws.com"}},
		}},
	})
	native := kubefake.NewClientset(objects...)
	scheme := runtime.NewScheme()
	requireNoError(t, clientgoscheme.AddToScheme(scheme))
	signer, err := selfsigned.NewGeneratedSelfSigner()
	requireNoError(t, err)
	r := &ManagedProxyConfigurationReconciler{
		Client:          fake.NewClientBuilder().WithScheme(scheme).Build(),
		SelfSigner:      signer,
		CAPair:          signer.CA(),
		SecretGetter:    native.CoreV1(),
		ServiceGetter:   native.CoreV1(),
		EventRecorder:   events.NewInMemoryRecorder("certificate-rollout-test", clock.RealClock{}),
		imagePullPolicy: string(corev1.PullIfNotPresent),
	}
	return r, config, native
}

func getCertificateRolloutDeployment(t *testing.T, r *ManagedProxyConfigurationReconciler, config *proxyv1alpha1.ManagedProxyConfiguration) *appsv1.Deployment {
	t.Helper()
	deployment := &appsv1.Deployment{}
	requireNoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: config.Spec.ProxyServer.Namespace, Name: config.Name}, deployment))
	return deployment
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

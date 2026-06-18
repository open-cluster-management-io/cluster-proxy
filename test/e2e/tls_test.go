package e2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

// TLS Profile Test runs serially because it mutates the ocm-tls-profile ConfigMap,
// which restarts the cluster-proxy deployment and would break concurrent connectivity tests.
var _ = Describe("TLS Profile Test", Serial, Label("tls", "profile", "configuration"),
	func() {
		const tlsConfigMapName = sdktls.ConfigMapName

		var (
			originalConfigMapData map[string]string
			configMapExisted      bool
		)

		BeforeEach(func() {
			originalConfigMapData = nil
			configMapExisted = false

			By("Saving original TLS ConfigMap state")
			existing, err := hubKubeClient.CoreV1().ConfigMaps(hubInstallNamespace).Get(context.TODO(), tlsConfigMapName, metav1.GetOptions{})
			switch {
			case err == nil:
				configMapExisted = true
				originalConfigMapData = maps.Clone(existing.Data)
			case apierrors.IsNotFound(err):
				// ConfigMap does not exist before the test; nothing to save.
			default:
				Expect(err).ToNot(HaveOccurred(), "Failed to get TLS ConfigMap in BeforeEach")
			}
		})

		AfterEach(func() {
			By("Capturing deployment generation before ConfigMap cleanup")
			deploy, err := getProxyServerDeployment()
			Expect(err).ToNot(HaveOccurred(), "Failed to get deployment before cleanup")
			generationBeforeCleanup := deploy.Generation

			var expectedData map[string]string
			By("Restoring original TLS ConfigMap state")
			if configMapExisted {
				applyTLSProfile(tlsConfigMapName, originalConfigMapData)
				expectedData = originalConfigMapData
			} else {
				err := hubKubeClient.CoreV1().ConfigMaps(hubInstallNamespace).Delete(context.TODO(), tlsConfigMapName, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					Expect(err).ToNot(HaveOccurred(), "Failed to delete ConfigMap in cleanup")
				}
			}

			By("Waiting for deployment to be reconciled after ConfigMap cleanup")
			waitForProxyServerTLSProfile(generationBeforeCleanup, expectedTLSArgs(expectedData))

			By("Waiting for cluster-proxy API path to recover after ConfigMap cleanup")
			waitForClusterProxyKubeAPIAvailable()
		})

		It("should apply TLS 1.2 with cipher suites from ConfigMap", Label("tls", "flags", "tls12"), func() {
			By("Creating TLS ConfigMap with TLS 1.2 and cipher suites")
			deploy, err := getProxyServerDeployment()
			Expect(err).ToNot(HaveOccurred())
			generationBeforeUpdate := deploy.Generation
			tlsProfile := map[string]string{
				sdktls.ConfigMapKeyMinVersion:   "VersionTLS12",
				sdktls.ConfigMapKeyCipherSuites: "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			}
			applyTLSProfile(tlsConfigMapName, tlsProfile)

			By("Ensuring the proxy-server is configured with TLS 1.2 and cipher suites")
			waitForProxyServerTLSProfile(generationBeforeUpdate, expectedTLSArgs(tlsProfile))

			By("Ensuring the cluster-proxy API path remains available")
			waitForClusterProxyKubeAPIAvailable()
		})

		It("should apply TLS 1.3 with cipher suites from ConfigMap", Label("tls", "flags", "tls13"), func() {
			By("Creating TLS ConfigMap with TLS 1.3 and cipher suites")
			deploy, err := getProxyServerDeployment()
			Expect(err).ToNot(HaveOccurred())
			generationBeforeUpdate := deploy.Generation
			tlsProfile := map[string]string{
				sdktls.ConfigMapKeyMinVersion:   "VersionTLS13",
				sdktls.ConfigMapKeyCipherSuites: "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384",
			}
			applyTLSProfile(tlsConfigMapName, tlsProfile)

			By("Ensuring the proxy-server is configured with TLS 1.3 and cipher suites")
			waitForProxyServerTLSProfile(generationBeforeUpdate, expectedTLSArgs(tlsProfile))

			By("Ensuring the cluster-proxy API path remains available")
			waitForClusterProxyKubeAPIAvailable()
		})
	})

func getProxyServerDeployment() (*appsv1.Deployment, error) {
	deploy := &appsv1.Deployment{}
	err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: hubInstallNamespace,
		Name:      "cluster-proxy",
	}, deploy)
	return deploy, err
}

// applyTLSProfile creates or updates the ocm-tls-profile ConfigMap with the given data.
func applyTLSProfile(name string, data map[string]string) {
	configMaps := hubKubeClient.CoreV1().ConfigMaps(hubInstallNamespace)
	configMap, err := configMaps.Get(context.TODO(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(context.TODO(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hubInstallNamespace},
			Data:       data,
		}, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		return
	}
	Expect(err).ToNot(HaveOccurred())
	configMap.Data = data
	_, err = configMaps.Update(context.TODO(), configMap, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

type expectedProxyServerTLSArgs struct {
	present      []string
	absentPrefix []string
}

func expectedTLSArgs(data map[string]string) expectedProxyServerTLSArgs {
	minTLSVersion := sdktls.VersionToString(sdktls.GetDefaultTLSConfig().MinVersion)
	if v := data[sdktls.ConfigMapKeyMinVersion]; v != "" {
		minTLSVersion = v
	}

	expected := expectedProxyServerTLSArgs{
		present: []string{"--tls-min-version=" + minTLSVersion},
	}
	if cs := data[sdktls.ConfigMapKeyCipherSuites]; cs != "" {
		expected.present = append(expected.present, "--cipher-suites="+cs)
	} else {
		expected.absentPrefix = append(expected.absentPrefix, "--cipher-suites=")
	}
	return expected
}

func waitForProxyServerTLSProfile(previousGeneration int64, expected expectedProxyServerTLSArgs) {
	Eventually(func() error {
		deploy, err := getProxyServerDeployment()
		if err != nil {
			return err
		}
		if err := expectProxyServerArgs(deploy, expected); err != nil {
			return err
		}
		return expectProxyServerReady(deploy, previousGeneration)
	}).WithTimeout(4 * time.Minute).WithPolling(5 * time.Second).ShouldNot(HaveOccurred())
}

// expectProxyServerArgs verifies the cluster-proxy proxy-server container has
// the expected TLS args and no stale TLS args from the previous profile.
func expectProxyServerArgs(deploy *appsv1.Deployment, expected expectedProxyServerTLSArgs) error {
	container := proxyServerContainer(deploy)
	if container == nil {
		return fmt.Errorf("proxy-server container not found in deployment")
	}
	for _, arg := range expected.present {
		if !slices.Contains(container.Args, arg) {
			return fmt.Errorf("expected arg %q in %v", arg, container.Args)
		}
	}
	for _, prefix := range expected.absentPrefix {
		for _, arg := range container.Args {
			if strings.HasPrefix(arg, prefix) {
				return fmt.Errorf("unexpected arg with prefix %q in %v", prefix, container.Args)
			}
		}
	}
	return nil
}

func expectProxyServerReady(deploy *appsv1.Deployment, _ int64) error {
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return fmt.Errorf("deployment generation %d not yet observed, observed=%d", deploy.Generation, deploy.Status.ObservedGeneration)
	}

	desiredReplicas := ptr.Deref(deploy.Spec.Replicas, 1)
	if deploy.Status.Replicas != desiredReplicas ||
		deploy.Status.UpdatedReplicas != desiredReplicas ||
		deploy.Status.ReadyReplicas != desiredReplicas ||
		deploy.Status.AvailableReplicas != desiredReplicas ||
		deploy.Status.UnavailableReplicas != 0 {
		return fmt.Errorf("deployment rollout is incomplete: %v", deploy.Status)
	}
	return nil
}

func proxyServerContainer(deploy *appsv1.Deployment) *corev1.Container {
	for i := range deploy.Spec.Template.Spec.Containers {
		if deploy.Spec.Template.Spec.Containers[i].Name == "proxy-server" {
			return &deploy.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

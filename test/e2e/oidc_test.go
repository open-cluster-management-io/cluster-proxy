package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authenticationv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/utils/ptr"

	addonapiv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	"open-cluster-management.io/cluster-proxy/pkg/config"
)

const (
	// dexIssuer must match byte-for-byte the issuer in test/e2e/env/dex.yaml;
	// it is also passed to the service-proxy via the oidcIssuerURL customized
	// variable (go-oidc requires exact issuer equality).
	dexIssuer        = "https://dex.dex.svc.cluster.local:5556/dex"
	dexNamespace     = "dex"
	dexTLSSecretName = "dex-tls"
	dexClientID      = "cluster-proxy-e2e"
	dexClientSecret  = "cluster-proxy-e2e-secret"
	dexUserEmail     = "admin@example.com"
	dexUserPassword  = "password"

	oidcUsernamePrefix   = "oidc:"
	expectedOIDCUsername = oidcUsernamePrefix + dexUserEmail
	oidcCAConfigMapName  = "cluster-proxy-oidc-ca"
	oidcDeployConfigName = "oidc-deploy-config"
	oidcRoleBindingName  = "oidc-podrolebinding"
)

var _ = Describe("OIDC Authentication Test", Label("serviceproxy", "oidc"), Ordered, func() {
	var oidcProxyClient kubernetes.Interface

	BeforeAll(func(ctx SpecContext) {
		By("Read the Dex CA bundle")
		dexTLSSecret, err := hubKubeClient.CoreV1().Secrets(dexNamespace).Get(ctx, dexTLSSecretName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		dexCA := dexTLSSecret.Data["ca.crt"]
		Expect(dexCA).ToNot(BeEmpty())

		By("Create or refresh the OIDC CA ConfigMap in the spoke addon namespace")
		applyConfigMap(managedClusterInstallNamespace, oidcCAConfigMapName, map[string]string{"ca.crt": string(dexCA)})

		By("Cleanup existing AddOnDeploymentConfig if any")
		Expect(deleteAddOnDeploymentConfig(oidcDeployConfigName)).To(Succeed())
		waitProxyAgentDeploymentRolledOut()

		originalAddon, err := getManagedClusterAddon()
		Expect(err).ToNot(HaveOccurred())
		originalConfigs := originalAddon.Spec.Configs

		DeferCleanup(func() {
			By("Restore cluster-proxy addon config after test")
			Eventually(func() error {
				return setManagedClusterAddonConfigs(originalConfigs)
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Cleanup AddOnDeploymentConfig after test")
			Expect(deleteAddOnDeploymentConfig(oidcDeployConfigName)).To(Succeed())

			By("Wait for the oidc flags to be removed from the service-proxy container")
			waitServiceProxyOIDCArgs(false)
			waitManagedClusterAddonAvailable()

			By("Cleanup the OIDC CA ConfigMap")
			deleteConfigMap(managedClusterInstallNamespace, oidcCAConfigMapName)
		})

		By("Create an AddOnDeploymentConfig with the oidc customized variables")
		Eventually(func() error {
			return createAddOnDeploymentConfig(oidcDeployConfigName, addonapiv1beta1.AddOnDeploymentConfigSpec{
				AgentInstallNamespace: config.DefaultAddonInstallNamespace,
				CustomizedVariables: []addonapiv1beta1.CustomizedVariable{
					{Name: "oidcIssuerURL", Value: dexIssuer},
					{Name: "oidcClientID", Value: dexClientID},
					{Name: "oidcUsernameClaim", Value: "email"},
					{Name: "oidcUsernamePrefix", Value: oidcUsernamePrefix},
					{Name: "oidcCAConfigMap", Value: oidcCAConfigMapName},
					{Name: "oidcSigningAlgs", Value: "RS256"},
					{Name: "oidcRequiredClaimsJSON", Value: `{"email":"admin@example.com"}`},
				},
			})
		}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

		By("Add the config to cluster-proxy")
		Eventually(func() error {
			return setManagedClusterAddonConfigs([]addonapiv1beta1.AddOnConfig{
				addOnDeploymentConfigReference(oidcDeployConfigName),
			})
		}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

		By("Ensure the config is referenced")
		waitManagedClusterAddonConfigReferenced(oidcDeployConfigName)

		By("Wait for the oidc flags to appear in the service-proxy container")
		waitServiceProxyOIDCArgs(true)
		waitManagedClusterAddonAvailable()

		By("Obtain a real ID token from Dex via the password grant")
		dexPool, err := certutil.NewPoolFromBytes(dexCA)
		Expect(err).ToNot(HaveOccurred())
		dexClient := &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: dexPool, MinVersion: tls.VersionTLS12},
			},
		}
		var idToken string
		Eventually(func() error {
			var err error
			idToken, err = requestDexIDToken(ctx, dexClient)
			return err
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).ShouldNot(HaveOccurred())

		By("Build a cluster-proxy client that authenticates with the ID token")
		oidcProxyClient = clusterProxyClientWithToken(idToken)
	})

	It("should report the impersonated OIDC identity", func() {
		// the first request also triggers the service-proxy's lazy OIDC
		// provider discovery, hence the Eventually
		Eventually(func() error {
			review, err := oidcProxyClient.AuthenticationV1().SelfSubjectReviews().Create(
				context.Background(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
			if err != nil {
				return err
			}
			if review.Status.UserInfo.Username != expectedOIDCUsername {
				return fmt.Errorf("expected username %q, got %q", expectedOIDCUsername, review.Status.UserInfo.Username)
			}
			if !slices.Contains(review.Status.UserInfo.Groups, "system:authenticated") {
				return fmt.Errorf("expected system:authenticated in groups %v", review.Status.UserInfo.Groups)
			}
			return nil
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})

	It("should be forbidden without RBAC", func() {
		_, err := oidcProxyClient.CoreV1().Pods(hubInstallNamespace).List(context.Background(), metav1.ListOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "expected forbidden error, got: %v", err)
	})

	It("should be allowed with RBAC bound to the OIDC username", func() {
		By("Bind the suite's pod role to the oidc user")
		_, err := hubKubeClient.RbacV1().RoleBindings(hubInstallNamespace).Create(context.Background(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      oidcRoleBindingName,
				Namespace: hubInstallNamespace,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     podRoleName,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:     rbacv1.UserKind,
					APIGroup: "rbac.authorization.k8s.io",
					Name:     expectedOIDCUsername,
				},
			},
		}, metav1.CreateOptions{})
		if !apierrors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}
		DeferCleanup(func() {
			By("Cleanup the oidc user rolebinding")
			Expect(hubKubeClient.RbacV1().RoleBindings(hubInstallNamespace).Delete(
				context.Background(), oidcRoleBindingName, metav1.DeleteOptions{})).To(Succeed())
		})

		Eventually(func() error {
			_, err := oidcProxyClient.CoreV1().Pods(hubInstallNamespace).List(context.Background(), metav1.ListOptions{})
			return err
		}).WithTimeout(time.Minute).WithPolling(5 * time.Second).ShouldNot(HaveOccurred())
	})

	It("should reject a garbage token", func() {
		// JWT-shaped so the token reaches the OIDC verifier instead of being
		// rejected outright by the TokenReview paths
		garbageClient := clusterProxyClientWithToken("eyJhbGciOiJSUzI1NiJ9.garbage.sig")

		_, err := garbageClient.CoreV1().Pods(hubInstallNamespace).List(context.Background(), metav1.ListOptions{})
		Expect(apierrors.IsUnauthorized(err)).To(BeTrue(), "expected unauthorized error, got: %v", err)
	})
})

// clusterProxyClientWithToken returns a client that reaches the managed cluster
// through the cluster-proxy while authenticating with the given bearer token.
func clusterProxyClientWithToken(token string) kubernetes.Interface {
	cfg := rest.CopyConfig(clusterProxyCfg)
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""
	client, err := kubernetes.NewForConfig(cfg)
	Expect(err).ToNot(HaveOccurred())
	return client
}

// waitServiceProxyOIDCArgs waits until the proxy agent deployment has rolled
// out with the service-proxy container carrying (or not carrying) the oidc
// flags. The agent re-renders its manifests roughly every 5 minutes in the
// worst case, so the timeout must exceed that cadence.
func waitServiceProxyOIDCArgs(expectPresent bool) {
	Eventually(func() error {
		deploy, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}
		container := deploymentContainer(deploy, "service-proxy")
		if container == nil {
			return fmt.Errorf("service-proxy container not found in deployment")
		}
		present := slices.ContainsFunc(container.Args, func(arg string) bool {
			return strings.HasPrefix(arg, "--oidc-issuer-url=")
		})
		if present != expectPresent {
			return fmt.Errorf("expected oidc args present=%v in %v", expectPresent, container.Args)
		}
		return proxyAgentRolledOut(deploy, ptr.Deref(deploy.Spec.Replicas, 1))
	}).WithTimeout(7 * time.Minute).WithPolling(10 * time.Second).ShouldNot(HaveOccurred())
}

// requestDexIDToken performs a resource owner password grant against Dex and
// returns the resulting ID token.
func requestDexIDToken(ctx context.Context, client *http.Client) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("scope", "openid email profile")
	form.Set("username", dexUserEmail)
	form.Set("password", dexUserPassword)
	form.Set("client_id", dexClientID)
	form.Set("client_secret", dexClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dexIssuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dex token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResponse struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", err
	}
	if tokenResponse.IDToken == "" {
		return "", fmt.Errorf("dex token response did not contain an id_token: %s", string(body))
	}
	return tokenResponse.IDToken, nil
}

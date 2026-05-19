package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	fakeaddon "open-cluster-management.io/api/client/addon/clientset/versioned/fake"
)

func TestProvisionerSyncCreatesManagedKubeconfigSecret(t *testing.T) {
	now := time.Date(2026, 5, 19, 1, 2, 3, 0, time.UTC)
	sourceKubeconfig := testKubeconfig()
	hostingClient := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultSourceSecretName,
			Namespace: "cluster1",
		},
		Data: map[string][]byte{SecretKubeconfigKey: sourceKubeconfig},
	})
	managedClient := fakeManagedClient(t, "managed-token", now.Add(time.Hour))

	provisioner := NewProvisioner(Options{
		ClusterName:                    "cluster1",
		TargetNamespace:                "addon-ns",
		ManagedServiceAccountNamespace: "addon-ns",
		TokenExpiration:                time.Hour,
	}, hostingClient).WithManagedClientFactory(func(kubeconfig []byte) (kubernetes.Interface, error) {
		if string(kubeconfig) != string(sourceKubeconfig) {
			t.Fatalf("expected source kubeconfig to be used")
		}
		return managedClient, nil
	}).WithNow(func() time.Time { return now })

	if err := provisioner.Sync(context.Background()); err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}

	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), DefaultTargetSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get target secret: %v", err)
	}
	if secret.Annotations[annotationSourceKubeconfigHash] != kubeconfigHash(sourceKubeconfig) {
		t.Fatalf("source hash annotation was not set")
	}
	if secret.Annotations[annotationTokenExpirationTimestamp] != now.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("unexpected expiration annotation: %s", secret.Annotations[annotationTokenExpirationTimestamp])
	}

	generatedConfig, err := clientcmd.Load(secret.Data[SecretKubeconfigKey])
	if err != nil {
		t.Fatalf("failed to load generated kubeconfig: %v", err)
	}
	if generatedConfig.Clusters["managed"].Server != "https://managed.example.com:6443" {
		t.Fatalf("unexpected generated server: %s", generatedConfig.Clusters["managed"].Server)
	}
	if generatedConfig.AuthInfos["cluster-proxy"].Token != "managed-token" {
		t.Fatalf("unexpected generated token: %s", generatedConfig.AuthInfos["cluster-proxy"].Token)
	}
}

func TestProvisionerSyncRecordsEventAndCondition(t *testing.T) {
	now := time.Date(2026, 5, 19, 1, 2, 3, 0, time.UTC)
	sourceKubeconfig := testKubeconfig()
	hostingClient := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultSourceSecretName,
			Namespace: "cluster1",
		},
		Data: map[string][]byte{SecretKubeconfigKey: sourceKubeconfig},
	})
	addonClient := fakeaddon.NewSimpleClientset(&addonv1alpha1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			Name:       DefaultAddonName,
			Namespace:  "cluster1",
			Generation: 7,
		},
	})
	managedClient := fakeManagedClient(t, "managed-token", now.Add(time.Hour))

	provisioner := NewProvisioner(Options{
		ClusterName:                    "cluster1",
		TargetNamespace:                "addon-ns",
		ManagedServiceAccountNamespace: "addon-ns",
		TokenExpiration:                time.Hour,
	}, hostingClient).WithManagedClientFactory(func(kubeconfig []byte) (kubernetes.Interface, error) {
		return managedClient, nil
	}).WithAddonClient(addonClient).WithNow(func() time.Time { return now })

	if err := provisioner.Sync(context.Background()); err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}

	addon, err := addonClient.AddonV1alpha1().ManagedClusterAddOns("cluster1").Get(context.Background(), DefaultAddonName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get addon: %v", err)
	}
	condition := meta.FindStatusCondition(addon.Status.Conditions, ConditionManagedKubeconfigReady)
	if condition == nil {
		t.Fatalf("expected %s condition", ConditionManagedKubeconfigReady)
	}
	if condition.Status != metav1.ConditionTrue || condition.Reason != "ManagedKubeconfigCreated" || condition.ObservedGeneration != 7 {
		t.Fatalf("unexpected condition: %#v", condition)
	}
	events, err := hostingClient.CoreV1().Events("addon-ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events.Items) == 0 {
		t.Fatal("expected a Kubernetes event to be recorded")
	}
}

func TestProvisionerSyncFailurePatchesConditionFalse(t *testing.T) {
	now := time.Date(2026, 5, 19, 1, 2, 3, 0, time.UTC)
	hostingClient := fake.NewSimpleClientset()
	addonClient := fakeaddon.NewSimpleClientset(&addonv1alpha1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultAddonName,
			Namespace: "cluster1",
		},
	})

	provisioner := NewProvisioner(Options{
		ClusterName:     "cluster1",
		TargetNamespace: "addon-ns",
	}, hostingClient).WithAddonClient(addonClient).WithNow(func() time.Time { return now })

	if err := provisioner.Sync(context.Background()); err == nil {
		t.Fatal("expected sync error")
	}
	addon, err := addonClient.AddonV1alpha1().ManagedClusterAddOns("cluster1").Get(context.Background(), DefaultAddonName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get addon: %v", err)
	}
	condition := meta.FindStatusCondition(addon.Status.Conditions, ConditionManagedKubeconfigReady)
	if condition == nil {
		t.Fatalf("expected %s condition", ConditionManagedKubeconfigReady)
	}
	if condition.Status != metav1.ConditionFalse || condition.Reason != "SyncFailed" {
		t.Fatalf("unexpected condition: %#v", condition)
	}
}

func TestProvisionerSyncSkipsFreshSecret(t *testing.T) {
	now := time.Date(2026, 5, 19, 1, 2, 3, 0, time.UTC)
	sourceKubeconfig := testKubeconfig()
	hostingClient := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultSourceSecretName, Namespace: "cluster1"},
			Data:       map[string][]byte{SecretKubeconfigKey: sourceKubeconfig},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      DefaultTargetSecretName,
				Namespace: "addon-ns",
				Annotations: map[string]string{
					annotationSourceKubeconfigHash:     kubeconfigHash(sourceKubeconfig),
					annotationTokenExpirationTimestamp: now.Add(2 * time.Hour).Format(time.RFC3339),
				},
			},
			Data: map[string][]byte{SecretKubeconfigKey: []byte("existing")},
		},
	)

	called := false
	provisioner := NewProvisioner(Options{
		ClusterName:     "cluster1",
		TargetNamespace: "addon-ns",
		RefreshBefore:   time.Hour,
	}, hostingClient).WithManagedClientFactory(func(kubeconfig []byte) (kubernetes.Interface, error) {
		called = true
		return fake.NewSimpleClientset(), nil
	}).WithNow(func() time.Time { return now })

	if err := provisioner.Sync(context.Background()); err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}
	if called {
		t.Fatalf("managed client should not be created when target secret is fresh")
	}
}

func TestProvisionerSyncRefreshesBeforeExpiration(t *testing.T) {
	now := time.Date(2026, 5, 19, 1, 2, 3, 0, time.UTC)
	sourceKubeconfig := testKubeconfig()
	hostingClient := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultSourceSecretName, Namespace: "cluster1"},
			Data:       map[string][]byte{SecretKubeconfigKey: sourceKubeconfig},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      DefaultTargetSecretName,
				Namespace: "addon-ns",
				Annotations: map[string]string{
					annotationSourceKubeconfigHash:     kubeconfigHash(sourceKubeconfig),
					annotationTokenExpirationTimestamp: now.Add(30 * time.Minute).Format(time.RFC3339),
				},
			},
			Data: map[string][]byte{SecretKubeconfigKey: []byte("existing")},
		},
	)
	managedClient := fakeManagedClient(t, "refreshed-token", now.Add(time.Hour))
	provisioner := NewProvisioner(Options{
		ClusterName:     "cluster1",
		TargetNamespace: "addon-ns",
		RefreshBefore:   time.Hour,
		TokenExpiration: time.Hour,
	}, hostingClient).WithManagedClientFactory(func(kubeconfig []byte) (kubernetes.Interface, error) {
		return managedClient, nil
	}).WithNow(func() time.Time { return now })

	if err := provisioner.Sync(context.Background()); err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}

	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), DefaultTargetSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get target secret: %v", err)
	}
	generatedConfig, err := clientcmd.Load(secret.Data[SecretKubeconfigKey])
	if err != nil {
		t.Fatalf("failed to load generated kubeconfig: %v", err)
	}
	if generatedConfig.AuthInfos["cluster-proxy"].Token != "refreshed-token" {
		t.Fatalf("expected refreshed token, got %q", generatedConfig.AuthInfos["cluster-proxy"].Token)
	}
}

func TestProvisionerSyncRefreshesWhenSourceKubeconfigChanges(t *testing.T) {
	now := time.Date(2026, 5, 19, 1, 2, 3, 0, time.UTC)
	sourceKubeconfig := []byte(`apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://changed.example.com:6443
contexts:
- name: managed
  context:
    cluster: managed
    user: admin
current-context: managed
users:
- name: admin
  user:
    token: admin-token
`)
	hostingClient := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultSourceSecretName, Namespace: "cluster1"},
			Data:       map[string][]byte{SecretKubeconfigKey: sourceKubeconfig},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      DefaultTargetSecretName,
				Namespace: "addon-ns",
				Annotations: map[string]string{
					annotationSourceKubeconfigHash:     kubeconfigHash(testKubeconfig()),
					annotationTokenExpirationTimestamp: now.Add(2 * time.Hour).Format(time.RFC3339),
				},
			},
			Data: map[string][]byte{SecretKubeconfigKey: []byte("existing")},
		},
	)
	managedClient := fakeManagedClient(t, "changed-token", now.Add(time.Hour))
	provisioner := NewProvisioner(Options{
		ClusterName:     "cluster1",
		TargetNamespace: "addon-ns",
		RefreshBefore:   time.Hour,
		TokenExpiration: time.Hour,
	}, hostingClient).WithManagedClientFactory(func(kubeconfig []byte) (kubernetes.Interface, error) {
		if string(kubeconfig) != string(sourceKubeconfig) {
			t.Fatalf("expected changed source kubeconfig to be used")
		}
		return managedClient, nil
	}).WithNow(func() time.Time { return now })

	if err := provisioner.Sync(context.Background()); err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}

	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), DefaultTargetSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get target secret: %v", err)
	}
	generatedConfig, err := clientcmd.Load(secret.Data[SecretKubeconfigKey])
	if err != nil {
		t.Fatalf("failed to load generated kubeconfig: %v", err)
	}
	if generatedConfig.Clusters["managed"].Server != "https://changed.example.com:6443" {
		t.Fatalf("expected changed server, got %q", generatedConfig.Clusters["managed"].Server)
	}
	if generatedConfig.AuthInfos["cluster-proxy"].Token != "changed-token" {
		t.Fatalf("expected changed token, got %q", generatedConfig.AuthInfos["cluster-proxy"].Token)
	}
	if secret.Annotations[annotationSourceKubeconfigHash] != kubeconfigHash(sourceKubeconfig) {
		t.Fatalf("expected source hash annotation to be updated")
	}
}

func TestProvisionerCleanupDeletesTargetSecret(t *testing.T) {
	hostingClient := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultTargetSecretName, Namespace: "addon-ns"},
	})
	provisioner := NewProvisioner(Options{
		TargetNamespace: "addon-ns",
		Cleanup:         true,
	}, hostingClient)

	if err := provisioner.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected cleanup error: %v", err)
	}
	_, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), DefaultTargetSecretName, metav1.GetOptions{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected target secret to be deleted, got %v", err)
	}
}

func fakeManagedClient(t *testing.T, token string, expiration time.Time) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		createAction := action.(k8stesting.CreateAction)
		tokenRequest := createAction.GetObject().(*authenticationv1.TokenRequest)
		if tokenRequest.Spec.ExpirationSeconds == nil || *tokenRequest.Spec.ExpirationSeconds == 0 {
			t.Fatalf("expected TokenRequest expiration to be set")
		}
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{
				Token:               token,
				ExpirationTimestamp: metav1.NewTime(expiration),
			},
		}, nil
	})
	return client
}

func testKubeconfig() []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://managed.example.com:6443
    certificate-authority-data: Y2E=
contexts:
- name: managed
  context:
    cluster: managed
    user: admin
current-context: managed
users:
- name: admin
  user:
    token: admin-token
`)
}

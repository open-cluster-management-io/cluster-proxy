package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	addonmetrics "open-cluster-management.io/cluster-proxy/pkg/metrics"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

const (
	DefaultSourceSecretName            = "external-managed-kubeconfig"
	DefaultTargetSecretName            = "cluster-proxy-managed-kubeconfig"
	DefaultManagedServiceAccountName   = "cluster-proxy"
	DefaultAddonName                   = common.AddonName
	DefaultTokenExpiration             = 24 * time.Hour
	DefaultRefreshBefore               = time.Hour
	DefaultSyncInterval                = 5 * time.Minute
	DefaultHealthProbeBindAddress      = ":8000"
	SecretKubeconfigKey                = "kubeconfig"
	ConditionManagedKubeconfigReady    = "ManagedKubeconfigReady"
	annotationTokenExpirationTimestamp = "proxy.open-cluster-management.io/managed-kubeconfig-token-expiration"
	annotationSourceKubeconfigHash     = "proxy.open-cluster-management.io/source-kubeconfig-hash"
)

type ManagedClientFactory func(kubeconfig []byte) (kubernetes.Interface, error)

type Options struct {
	ClusterName                    string
	SourceNamespace                string
	SourceName                     string
	TargetNamespace                string
	TargetName                     string
	ManagedServiceAccountNamespace string
	ManagedServiceAccountName      string
	TokenExpiration                time.Duration
	RefreshBefore                  time.Duration
	SyncInterval                   time.Duration
	HubKubeconfig                  string
	AddonName                      string
	AddonNamespace                 string
	HealthProbeBindAddress         string
	Cleanup                        bool
}

func (o *Options) Complete() {
	if o.SourceNamespace == "" {
		o.SourceNamespace = o.ClusterName
	}
	if o.SourceName == "" {
		o.SourceName = DefaultSourceSecretName
	}
	if o.TargetNamespace == "" {
		o.TargetNamespace = os.Getenv("POD_NAMESPACE")
	}
	if o.TargetName == "" {
		o.TargetName = DefaultTargetSecretName
	}
	if o.ManagedServiceAccountNamespace == "" {
		o.ManagedServiceAccountNamespace = o.TargetNamespace
	}
	if o.ManagedServiceAccountName == "" {
		o.ManagedServiceAccountName = DefaultManagedServiceAccountName
	}
	if o.TokenExpiration == 0 {
		o.TokenExpiration = DefaultTokenExpiration
	}
	if o.RefreshBefore == 0 {
		o.RefreshBefore = DefaultRefreshBefore
	}
	if o.SyncInterval == 0 {
		o.SyncInterval = DefaultSyncInterval
	}
	if o.AddonName == "" {
		o.AddonName = common.AddonName
	}
	if o.AddonNamespace == "" {
		o.AddonNamespace = o.ClusterName
	}
	if o.HealthProbeBindAddress == "" {
		o.HealthProbeBindAddress = DefaultHealthProbeBindAddress
	}
}

func (o Options) Validate() error {
	if o.Cleanup {
		if o.TargetNamespace == "" {
			return fmt.Errorf("target namespace is required")
		}
		if o.TargetName == "" {
			return fmt.Errorf("target name is required")
		}
		return nil
	}
	if o.ClusterName == "" {
		return fmt.Errorf("cluster name is required")
	}
	if o.SourceNamespace == "" {
		return fmt.Errorf("source namespace is required")
	}
	if o.SourceName == "" {
		return fmt.Errorf("source name is required")
	}
	if o.TargetNamespace == "" {
		return fmt.Errorf("target namespace is required")
	}
	if o.TargetName == "" {
		return fmt.Errorf("target name is required")
	}
	if o.ManagedServiceAccountNamespace == "" {
		return fmt.Errorf("managed service account namespace is required")
	}
	if o.ManagedServiceAccountName == "" {
		return fmt.Errorf("managed service account name is required")
	}
	if o.TokenExpiration <= 0 {
		return fmt.Errorf("token expiration must be greater than zero")
	}
	if o.RefreshBefore < 0 {
		return fmt.Errorf("refresh before must not be negative")
	}
	if o.SyncInterval <= 0 {
		return fmt.Errorf("sync interval must be greater than zero")
	}
	return nil
}

type Provisioner struct {
	options             Options
	hostingClient       kubernetes.Interface
	managedClientFn     ManagedClientFactory
	addonClient         addonclient.Interface
	now                 func() time.Time
	lastTokenExpiration time.Time
}

func NewProvisioner(options Options, hostingClient kubernetes.Interface) *Provisioner {
	options.Complete()
	return &Provisioner{
		options:         options,
		hostingClient:   hostingClient,
		managedClientFn: newManagedClient,
		now:             time.Now,
	}
}

func (p *Provisioner) WithManagedClientFactory(factory ManagedClientFactory) *Provisioner {
	p.managedClientFn = factory
	return p
}

func (p *Provisioner) WithAddonClient(addonClient addonclient.Interface) *Provisioner {
	p.addonClient = addonClient
	return p
}

func (p *Provisioner) WithNow(now func() time.Time) *Provisioner {
	p.now = now
	return p
}

func (p *Provisioner) LastTokenExpiration() time.Time {
	return p.lastTokenExpiration
}

func (p *Provisioner) Run(ctx context.Context) error {
	go func() {
		if err := utils.ServeHealthProbes(p.options.HealthProbeBindAddress, nil); err != nil {
			klog.Fatal(err)
		}
	}()

	if p.options.Cleanup {
		return p.Cleanup(ctx)
	}
	if err := p.Sync(ctx); err != nil {
		klog.Errorf("managed kubeconfig sync failed: %v", err)
	}

	ticker := time.NewTicker(p.options.SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.Sync(ctx); err != nil {
				klog.Errorf("managed kubeconfig sync failed: %v", err)
			}
		}
	}
}

func (p *Provisioner) Cleanup(ctx context.Context) error {
	err := p.hostingClient.CoreV1().Secrets(p.options.TargetNamespace).Delete(ctx, p.options.TargetName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		p.recordEvent(ctx, corev1.EventTypeNormal, "ManagedKubeconfigCleanupSkipped",
			fmt.Sprintf("Generated managed kubeconfig secret %s/%s was already removed", p.options.TargetNamespace, p.options.TargetName))
		return nil
	}
	if err != nil {
		p.recordEvent(ctx, corev1.EventTypeWarning, "ManagedKubeconfigCleanupFailed", err.Error())
		return err
	}
	p.recordEvent(ctx, corev1.EventTypeNormal, "ManagedKubeconfigCleaned",
		fmt.Sprintf("Deleted generated managed kubeconfig secret %s/%s", p.options.TargetNamespace, p.options.TargetName))
	return nil
}

func (p *Provisioner) Sync(ctx context.Context) error {
	result, err := p.sync(ctx)
	if err != nil {
		addonmetrics.ObserveManagedKubeconfigRefresh("error")
		p.recordEvent(ctx, corev1.EventTypeWarning, "ManagedKubeconfigSyncFailed", err.Error())
		conditionErr := p.setManagedKubeconfigCondition(ctx, metav1.ConditionFalse, "SyncFailed", err.Error())
		if conditionErr != nil {
			klog.Errorf("failed to patch ManagedClusterAddOn condition after sync failure: %v", conditionErr)
		}
		return err
	}

	addonmetrics.ObserveManagedKubeconfigRefresh(result.metricResult)
	if !result.expiration.IsZero() {
		addonmetrics.SetManagedKubeconfigTokenExpiration(result.expiration, p.now())
	}
	p.recordEvent(ctx, corev1.EventTypeNormal, result.reason, result.message)
	if err := p.setManagedKubeconfigCondition(ctx, metav1.ConditionTrue, result.reason, result.message); err != nil {
		addonmetrics.ObserveManagedKubeconfigRefresh("error")
		p.recordEvent(ctx, corev1.EventTypeWarning, "ManagedKubeconfigConditionPatchFailed", err.Error())
		return err
	}
	return nil
}

type syncResult struct {
	metricResult string
	reason       string
	message      string
	expiration   time.Time
}

func (p *Provisioner) sync(ctx context.Context) (syncResult, error) {
	if err := p.options.Validate(); err != nil {
		return syncResult{}, err
	}

	source, err := p.hostingClient.CoreV1().Secrets(p.options.SourceNamespace).Get(ctx, p.options.SourceName, metav1.GetOptions{})
	if err != nil {
		return syncResult{}, err
	}
	sourceKubeconfig, ok := source.Data[SecretKubeconfigKey]
	if !ok || len(sourceKubeconfig) == 0 {
		return syncResult{}, fmt.Errorf("source secret %s/%s does not contain %q", p.options.SourceNamespace, p.options.SourceName, SecretKubeconfigKey)
	}

	sourceHash := kubeconfigHash(sourceKubeconfig)
	target, err := p.hostingClient.CoreV1().Secrets(p.options.TargetNamespace).Get(ctx, p.options.TargetName, metav1.GetOptions{})
	targetExists := err == nil
	if targetExists && !needsRefresh(target, sourceHash, p.now(), p.options.RefreshBefore) {
		klog.V(4).Infof("managed kubeconfig secret %s/%s is still fresh", p.options.TargetNamespace, p.options.TargetName)
		return syncResult{
			metricResult: "skipped",
			reason:       "ManagedKubeconfigFresh",
			message:      fmt.Sprintf("Generated managed kubeconfig secret %s/%s is still fresh", p.options.TargetNamespace, p.options.TargetName),
			expiration:   tokenExpirationFromSecret(target),
		}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return syncResult{}, err
	}

	managedClient, err := p.managedClientFn(sourceKubeconfig)
	if err != nil {
		return syncResult{}, err
	}

	expirationSeconds := int64(p.options.TokenExpiration.Seconds())
	tokenRequest, err := managedClient.CoreV1().ServiceAccounts(p.options.ManagedServiceAccountNamespace).CreateToken(
		ctx,
		p.options.ManagedServiceAccountName,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				ExpirationSeconds: &expirationSeconds,
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return syncResult{}, err
	}
	if tokenRequest.Status.Token == "" {
		return syncResult{}, fmt.Errorf("token request for serviceaccount %s/%s returned an empty token",
			p.options.ManagedServiceAccountNamespace, p.options.ManagedServiceAccountName)
	}

	expiration := tokenRequest.Status.ExpirationTimestamp.Time
	if expiration.IsZero() {
		expiration = p.now().Add(p.options.TokenExpiration)
	}
	managedKubeconfig, err := BuildManagedKubeconfig(sourceKubeconfig, tokenRequest.Status.Token)
	if err != nil {
		return syncResult{}, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.options.TargetName,
			Namespace: p.options.TargetNamespace,
			Annotations: map[string]string{
				annotationTokenExpirationTimestamp: expiration.UTC().Format(time.RFC3339),
				annotationSourceKubeconfigHash:     sourceHash,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			SecretKubeconfigKey: managedKubeconfig,
		},
	}

	if targetExists {
		secret.ResourceVersion = target.ResourceVersion
		_, err = p.hostingClient.CoreV1().Secrets(p.options.TargetNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	} else {
		_, err = p.hostingClient.CoreV1().Secrets(p.options.TargetNamespace).Create(ctx, secret, metav1.CreateOptions{})
	}
	if err != nil {
		return syncResult{}, err
	}

	p.lastTokenExpiration = expiration
	klog.Infof("managed kubeconfig secret %s/%s synced; token expires at %s",
		p.options.TargetNamespace, p.options.TargetName, expiration.UTC().Format(time.RFC3339))
	reason := "ManagedKubeconfigCreated"
	if targetExists {
		reason = "ManagedKubeconfigUpdated"
	}
	return syncResult{
		metricResult: "success",
		reason:       reason,
		message: fmt.Sprintf("Synced generated managed kubeconfig secret %s/%s; token expires at %s",
			p.options.TargetNamespace, p.options.TargetName, expiration.UTC().Format(time.RFC3339)),
		expiration: expiration,
	}, nil
}

func (p *Provisioner) recordEvent(ctx context.Context, eventType, reason, message string) {
	if p.options.TargetNamespace == "" || p.options.TargetName == "" {
		return
	}
	now := metav1.NewTime(p.now())
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    p.options.TargetNamespace,
			GenerateName: p.options.TargetName + ".",
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Secret",
			Namespace:  p.options.TargetNamespace,
			Name:       p.options.TargetName,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         corev1.EventSource{Component: "cluster-proxy-managed-kubeconfig-provisioner"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		EventTime:      metav1.MicroTime(now),
		Count:          1,
	}
	if _, err := p.hostingClient.CoreV1().Events(p.options.TargetNamespace).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		klog.Errorf("failed to record managed kubeconfig event %s: %v", reason, err)
	}
}

func (p *Provisioner) setManagedKubeconfigCondition(ctx context.Context, status metav1.ConditionStatus, reason, message string) error {
	if p.addonClient == nil || p.options.AddonNamespace == "" || p.options.AddonName == "" {
		return nil
	}

	addon, err := p.addonClient.AddonV1alpha1().ManagedClusterAddOns(p.options.AddonNamespace).Get(ctx, p.options.AddonName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	updated := addon.DeepCopy()
	meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
		Type:               ConditionManagedKubeconfigReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: addon.Generation,
	})
	_, err = p.addonClient.AddonV1alpha1().ManagedClusterAddOns(p.options.AddonNamespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	return err
}

func BuildManagedKubeconfig(sourceKubeconfig []byte, token string) ([]byte, error) {
	sourceConfig, err := clientcmd.Load(sourceKubeconfig)
	if err != nil {
		return nil, err
	}
	cluster, err := currentCluster(sourceConfig)
	if err != nil {
		return nil, err
	}

	clusterCopy := *cluster
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"managed": &clusterCopy,
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"cluster-proxy": {
				Token: token,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"managed": {
				Cluster:  "managed",
				AuthInfo: "cluster-proxy",
			},
		},
		CurrentContext: "managed",
	}

	return clientcmd.Write(config)
}

func currentCluster(config *clientcmdapi.Config) (*clientcmdapi.Cluster, error) {
	if config == nil {
		return nil, fmt.Errorf("kubeconfig is empty")
	}
	if config.CurrentContext != "" {
		if context, ok := config.Contexts[config.CurrentContext]; ok && context.Cluster != "" {
			if cluster, ok := config.Clusters[context.Cluster]; ok {
				return cluster, nil
			}
			return nil, fmt.Errorf("current context references missing cluster %q", context.Cluster)
		}
	}
	if len(config.Clusters) == 1 {
		for _, cluster := range config.Clusters {
			return cluster, nil
		}
	}
	return nil, fmt.Errorf("kubeconfig must have a current context or exactly one cluster")
}

func needsRefresh(secret *corev1.Secret, sourceHash string, now time.Time, refreshBefore time.Duration) bool {
	if secret == nil {
		return true
	}
	if len(secret.Data[SecretKubeconfigKey]) == 0 {
		return true
	}
	if secret.Annotations[annotationSourceKubeconfigHash] != sourceHash {
		return true
	}
	expirationRaw := secret.Annotations[annotationTokenExpirationTimestamp]
	if expirationRaw == "" {
		return true
	}
	expiration, err := time.Parse(time.RFC3339, expirationRaw)
	if err != nil {
		return true
	}
	return !now.Add(refreshBefore).Before(expiration)
}

func tokenExpirationFromSecret(secret *corev1.Secret) time.Time {
	if secret == nil {
		return time.Time{}
	}
	expirationRaw := secret.Annotations[annotationTokenExpirationTimestamp]
	if expirationRaw == "" {
		return time.Time{}
	}
	expiration, err := time.Parse(time.RFC3339, expirationRaw)
	if err != nil {
		return time.Time{}
	}
	return expiration
}

func kubeconfigHash(kubeconfig []byte) string {
	sum := sha256.Sum256(kubeconfig)
	return hex.EncodeToString(sum[:])
}

func newManagedClient(kubeconfig []byte) (kubernetes.Interface, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(rest.AddUserAgent(config, "cluster-proxy-managed-kubeconfig-provisioner"))
}

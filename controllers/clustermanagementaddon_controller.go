package controllers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strconv"
	"time"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	proxyclient "open-cluster-management.io/cluster-proxy/pkg/generated/clientset/versioned"
	proxylister "open-cluster-management.io/cluster-proxy/pkg/generated/listers/proxy/v1alpha1"

	"open-cluster-management.io/addon-framework/pkg/certrotation"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonlister "open-cluster-management.io/api/client/addon/listers/addon/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/addon/operator/authentication/selfsigned"
	"open-cluster-management.io/cluster-proxy/pkg/addon/operator/eventhandler"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var _ reconcile.Reconciler = &ClusterManagementAddonReconciler{}

var log = ctrl.Log.WithName("ClusterManagementAddonReconciler")

func RegisterClusterManagementAddonReconciler(
	mgr manager.Manager,
	selfSigner selfsigned.SelfSigner,
	nativeClient kubernetes.Interface,
	secretInformer informercorev1.SecretInformer) error {
	r := &ClusterManagementAddonReconciler{
		Client:     mgr.GetClient(),
		SelfSigner: selfSigner,
		CAPair:     selfSigner.CA(),
		newCertRotatorFunc: func(namespace, name string, sans ...string) selfsigned.CertRotation {
			return &certrotation.TargetRotation{
				Namespace:     namespace,
				Name:          name,
				Validity:      time.Hour * 24 * 180,
				HostNames:     sans,
				Lister:        secretInformer.Lister(),
				Client:        nativeClient.CoreV1(),
				EventRecorder: events.NewInMemoryRecorder("ClusterManagementAddonReconciler"),
			}
		},
		ServiceGetter:    nativeClient.CoreV1(),
		DeploymentGetter: nativeClient.AppsV1(),
	}
	return r.SetupWithManager(mgr)
}

type ClusterManagementAddonReconciler struct {
	client.Client
	SelfSigner       selfsigned.SelfSigner
	CAPair           *crypto.CA
	SecretLister     corev1listers.SecretLister
	SecretGetter     corev1client.SecretsGetter
	DeploymentGetter appsv1client.DeploymentsGetter
	ServiceGetter    corev1client.ServicesGetter
	EventRecorder    events.Recorder

	newCertRotatorFunc func(namespace, name string, sans ...string) selfsigned.CertRotation
	proxyConfigClient  proxyclient.Interface
	proxyConfigLister  proxylister.ManagedProxyConfigurationLister
	addonLister        addonlister.ManagedClusterAddOnNamespaceLister
	clusterAddonLister addonlister.ClusterManagementAddOnLister
}

func (c *ClusterManagementAddonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&addonv1alpha1.ClusterManagementAddOn{}).
		Watches(
			&source.Kind{
				Type: &proxyv1alpha1.ManagedProxyConfiguration{},
			},
			&eventhandler.ManagedProxyConfigurationHandler{
				Client: mgr.GetClient(),
			}).
		Complete(c)
}

func (c *ClusterManagementAddonReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log.Info("Start reconcile", "name", request.Name)

	// get the latest cluster-addon
	addon := &addonv1alpha1.ClusterManagementAddOn{}
	if err := c.Client.Get(ctx, request.NamespacedName, addon); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Cannot find cluster-addon", "name", request.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if len(addon.Spec.AddOnConfiguration.CRName) == 0 {
		log.Info("Skipping cluster-addon, no config coordinate", "name", request.Name)
		return reconcile.Result{}, nil
	}

	// get the related proxy configuration
	config := &proxyv1alpha1.ManagedProxyConfiguration{}
	if err := c.Client.Get(ctx, types.NamespacedName{
		Name: addon.Spec.AddOnConfiguration.CRName,
	}, config); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Cannot find proxy-configuration", "name", addon.Spec.AddOnConfiguration.CRName)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// ensure mandatory resources
	if err := c.ensureBasicResources(config); err != nil {
		return reconcile.Result{}, err
	}

	// ensure entrypoint
	entrypoint, err := c.ensureEntrypoint(config)
	if err != nil {
		return reconcile.Result{}, err
	}

	// ensure proxy-server cert rotation.
	// at an interval of 10 hrs which is the default resync period of controller-runtime's informer.
	if err := c.ensureRotation(config, entrypoint); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to rotate certificate")
	}

	// deploying central proxy server instances into the hub cluster.
	err = c.deployProxyServer(config)
	if err != nil {
		return reconcile.Result{}, err
	}

	// refreshing status
	if err := c.refreshStatus(config); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (c *ClusterManagementAddonReconciler) refreshStatus(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	currentState, err := c.getCurrentState(config)
	if err != nil {
		return err
	}
	status := proxyv1alpha1.ManagedProxyConfigurationStatus{}
	status.LastObservedGeneration = config.Generation
	status.Conditions = c.getConditions(currentState)
	mungedStatus := config.Status.DeepCopy()
	for i := range mungedStatus.Conditions {
		mungedStatus.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	if equality.Semantic.DeepEqual(&status, mungedStatus) {
		return nil
	}
	now := metav1.Now()
	editingConfig := config.DeepCopy()
	editingConfig.Status = status
	for i := range editingConfig.Status.Conditions {
		editingConfig.Status.Conditions[i].LastTransitionTime = now
	}
	return c.Client.Status().Update(context.TODO(), editingConfig)
}

func (c *ClusterManagementAddonReconciler) deployProxyServer(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	resources := []client.Object{
		newServiceAccount(config),
		newProxyService(config),
		newProxySecret(config, c.SelfSigner.CAData()),
		newProxyServerDeployment(config),
	}
	for _, resource := range resources {
		if err := c.ensure(resource); err != nil {
			return err
		}
	}
	return nil
}

func (c *ClusterManagementAddonReconciler) ensure(resource client.Object) error {
	if err := c.Client.Get(
		context.TODO(),
		types.NamespacedName{
			Namespace: resource.GetNamespace(),
			Name:      resource.GetName(),
		}, resource); err != nil {
		if apierrors.IsNotFound(err) {
			// if not found, then create
			if err := c.Client.Create(context.TODO(), resource); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return err
				}
			}
		}
		return err
	}
	if err := c.Client.Update(context.TODO(), resource); err != nil {
		if apierrors.IsConflict(err) {
			return c.ensure(resource)
		}
		return err
	}
	return nil
}

func (c *ClusterManagementAddonReconciler) getConditions(s *state) []metav1.Condition {
	deployedCondition := metav1.Condition{
		Type:    proxyv1alpha1.ConditionTypeProxyServerDeployed,
		Status:  metav1.ConditionFalse,
		Reason:  "NotYetDeployed",
		Message: "Replicas: " + strconv.Itoa(s.replicas),
	}
	if s.deployed {
		deployedCondition.Reason = "SuccessfullyDeployed"
		deployedCondition.Status = metav1.ConditionTrue
	}

	proxyServerCondition := metav1.Condition{
		Type:   proxyv1alpha1.ConditionTypeProxyServerSecretSigned,
		Status: metav1.ConditionFalse,
		Reason: "NotYetSigned",
	}
	if s.proxyServerCertExpireTime != nil {
		proxyServerCondition.Status = metav1.ConditionTrue
		proxyServerCondition.Reason = "SuccessfullySigned"
		proxyServerCondition.Message = "Expiry:" + s.proxyServerCertExpireTime.String()
	}

	agentServerCondition := metav1.Condition{
		Type:   proxyv1alpha1.ConditionTypeAgentServerSecretSigned,
		Status: metav1.ConditionFalse,
		Reason: "NotYetSigned",
	}
	if s.agentServerCertExpireTime != nil {
		agentServerCondition.Status = metav1.ConditionTrue
		agentServerCondition.Reason = "SuccessfullySigned"
		agentServerCondition.Message = "Expiry:" + s.agentServerCertExpireTime.String()
	}

	return []metav1.Condition{
		deployedCondition,
		proxyServerCondition,
		agentServerCondition,
	}
}

func (c *ClusterManagementAddonReconciler) ensureEntrypoint(config *proxyv1alpha1.ManagedProxyConfiguration) (string, error) {
	if config.Spec.ProxyServer.Entrypoint.Type == proxyv1alpha1.EntryPointTypeLoadBalancerService {
		proxyService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: config.Spec.ProxyServer.Namespace,
				Name:      config.Spec.ProxyServer.Entrypoint.LoadBalancerService.Name,
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					common.LabelKeyComponentName: common.ComponentNameProxyServer,
				},
				Type: corev1.ServiceTypeLoadBalancer,
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
		if err := c.Client.Create(context.TODO(), proxyService); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return "", errors.Wrapf(err, "failed to ensure entrypoint service for proxy-server")
			}
		}
	}

	switch config.Spec.ProxyServer.Entrypoint.Type {
	case proxyv1alpha1.EntryPointTypeLoadBalancerService:
		namespace := config.Spec.ProxyServer.Namespace
		name := config.Spec.ProxyServer.Entrypoint.LoadBalancerService.Name
		lbSvc, err := c.ServiceGetter.Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return "", errors.Wrapf(err, "failed to get service %q/%q", namespace, name)
		}
		if len(lbSvc.Status.LoadBalancer.Ingress) == 0 {
			return "", errors.New("external ip not yet provisioned")
		}
		return lbSvc.Status.LoadBalancer.Ingress[0].IP, nil
	}
	return "", fmt.Errorf("unsupported entrypoint type: %q", config.Spec.ProxyServer.Entrypoint.Type)
}

func (c *ClusterManagementAddonReconciler) ensureRotation(config *proxyv1alpha1.ManagedProxyConfiguration, entrypoint string) error {
	var hostNames []string
	if config.Spec.Authentication.Signer.SelfSigned != nil {
		hostNames = config.Spec.Authentication.Signer.SelfSigned.AdditionalSANs
	}
	sans := append(
		hostNames,
		entrypoint,
		config.Spec.ProxyServer.InClusterServiceName+"."+config.Spec.ProxyServer.Namespace,
		config.Spec.ProxyServer.InClusterServiceName+"."+config.Spec.ProxyServer.Namespace+".svc")

	tweakClientCertUsageFunc := func(cert *x509.Certificate) error {
		cert.ExtKeyUsage = []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		}
		return nil
	}

	// proxy server cert rotation
	proxyServerRotator := c.newCertRotatorFunc(
		config.Spec.ProxyServer.Namespace,
		config.Spec.Authentication.Dump.Secrets.SigningProxyServerSecretName,
		sans...)
	if err := proxyServerRotator.EnsureTargetCertKeyPair(c.CAPair, c.CAPair.Config.Certs); err != nil {
		return err
	}

	// agent sever cert rotation
	agentServerRotator := c.newCertRotatorFunc(
		config.Spec.ProxyServer.Namespace,
		config.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName,
		sans...)
	if err := agentServerRotator.EnsureTargetCertKeyPair(c.CAPair, c.CAPair.Config.Certs); err != nil {
		return err
	}

	// proxy client cert rotation
	proxyClientRotator := c.newCertRotatorFunc(
		config.Spec.ProxyServer.Namespace,
		config.Spec.Authentication.Dump.Secrets.SigningProxyClientSecretName,
		sans...)
	if err := proxyClientRotator.EnsureTargetCertKeyPair(c.CAPair, c.CAPair.Config.Certs, tweakClientCertUsageFunc); err != nil {
		return err
	}

	return nil
}

func (c *ClusterManagementAddonReconciler) ensureBasicResources(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	if err := c.ensureNamespace(config); err != nil {
		return err
	}
	return nil
}

func (c *ClusterManagementAddonReconciler) ensureNamespace(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	if err := c.Client.Get(context.TODO(), types.NamespacedName{
		Name: config.Spec.ProxyServer.Namespace,
	}, &corev1.Namespace{}); err != nil {
		if apierrors.IsNotFound(err) {
			if err := c.Client.Create(context.TODO(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: config.Spec.ProxyServer.Namespace,
				},
			}); err != nil {
				return errors.Wrapf(err, "failed creating namespace %q on absence", config.Spec.ProxyServer.Namespace)
			}
			return nil
		}
		return errors.Wrapf(err, "failed check namespace %q's presence", config.Spec.ProxyServer.Namespace)
	}
	return nil
}

func (c *ClusterManagementAddonReconciler) ensureProxyServerSecret(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	namespace := config.Spec.ProxyServer.Namespace
	name := config.Spec.Authentication.Dump.Secrets.SigningProxyServerSecretName
	return c.ensureSecret(namespace, name)
}

func (c *ClusterManagementAddonReconciler) ensureAgentServerSecret(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	namespace := config.Spec.ProxyServer.Namespace
	name := config.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName
	return c.ensureSecret(namespace, name)
}

func (c *ClusterManagementAddonReconciler) ensureProxyClientSecret(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	namespace := config.Spec.ProxyServer.Namespace
	name := config.Spec.Authentication.Dump.Secrets.SigningProxyClientSecretName
	return c.ensureSecret(namespace, name)
}

func (c *ClusterManagementAddonReconciler) ensureSecret(namespace, name string) error {
	secret, err := c.SecretLister.Secrets(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      name,
			},
		}
		_, err := c.SecretGetter.Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return errors.Wrapf(err, "failed creating secret's %q/%q", namespace, name)
	}
	if err != nil {
		return errors.Wrapf(err, "failed checking secret's %q/%q's presence", namespace, name)
	}
	return nil
}

type state struct {
	deployed                  bool
	replicas                  int
	proxyServerCertExpireTime *metav1.Time
	agentServerCertExpireTime *metav1.Time
}

func (c *ClusterManagementAddonReconciler) getCurrentState(config *proxyv1alpha1.ManagedProxyConfiguration) (*state, error) {
	namespace := config.Spec.ProxyServer.Namespace
	name := config.Name
	isDeployed := true
	scale, err := c.DeploymentGetter.Deployments(namespace).GetScale(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			isDeployed = false
		}
		return nil, err
	}
	s := &state{
		deployed: isDeployed,
		replicas: int(scale.Status.Replicas),
	}
	proxyServerSecret, err := c.SecretGetter.Secrets(namespace).
		Get(context.TODO(),
			config.Spec.Authentication.Dump.Secrets.SigningProxyServerSecretName,
			metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			isDeployed = false
		}
		return nil, err
	}
	s.proxyServerCertExpireTime = getPEMCertExpireTime(proxyServerSecret.Data[corev1.TLSCertKey])

	agentServerSecret, err := c.SecretGetter.Secrets(namespace).
		Get(context.TODO(),
			config.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName,
			metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			isDeployed = false
		}
		return nil, err
	}
	s.agentServerCertExpireTime = getPEMCertExpireTime(agentServerSecret.Data[corev1.TLSCertKey])

	return s, nil
}

func getPEMCertExpireTime(pemBytes []byte) *metav1.Time {
	b, _ := pem.Decode(pemBytes)
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		log.Error(err, "Failed parsing cert")
		return nil
	}
	return &metav1.Time{Time: cert.NotAfter}
}

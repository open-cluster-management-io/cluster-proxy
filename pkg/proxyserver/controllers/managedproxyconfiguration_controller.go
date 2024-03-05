package controllers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strconv"
	"strings"
	"time"

	"open-cluster-management.io/addon-framework/pkg/certrotation"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ reconcile.Reconciler = &ManagedProxyConfigurationReconciler{}

var log = ctrl.Log.WithName("ClusterManagementAddonReconciler")

func RegisterClusterManagementAddonReconciler(
	mgr manager.Manager,
	selfSigner selfsigned.SelfSigner,
	nativeClient kubernetes.Interface,
	secretInformer informercorev1.SecretInformer,
	supportsV1CSR bool,
) error {
	r := &ManagedProxyConfigurationReconciler{
		Client:     mgr.GetClient(),
		SelfSigner: selfSigner,
		CAPair:     selfSigner.CA(),
		newCertRotatorFunc: func(namespace, name string, sans ...string) selfsigned.CertRotation {
			return &certrotation.TargetRotation{
				Namespace: namespace,
				Name:      name,
				Validity:  time.Hour * 24 * 180,
				HostNames: sans,
				Lister:    secretInformer.Lister(),
				Client:    nativeClient.CoreV1(),
			}
		},
		SecretLister:     secretInformer.Lister(),
		SecretGetter:     nativeClient.CoreV1(),
		ServiceGetter:    nativeClient.CoreV1(),
		DeploymentGetter: nativeClient.AppsV1(),
		EventRecorder:    events.NewInMemoryRecorder("ClusterManagementAddonReconciler"),

		supportsV1CSR: supportsV1CSR,
	}
	return r.SetupWithManager(mgr)
}

type ManagedProxyConfigurationReconciler struct {
	client.Client
	SelfSigner       selfsigned.SelfSigner
	CAPair           *crypto.CA
	SecretLister     corev1listers.SecretLister
	SecretGetter     corev1client.SecretsGetter
	DeploymentGetter appsv1client.DeploymentsGetter
	ServiceGetter    corev1client.ServicesGetter
	EventRecorder    events.Recorder

	newCertRotatorFunc func(namespace, name string, sans ...string) selfsigned.CertRotation
	supportsV1CSR      bool
}

func (c *ManagedProxyConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// TODO should add a filter to only watch addon with cluster-proxy name
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.ManagedProxyConfiguration{}).
		Complete(c)
}

func (c *ManagedProxyConfigurationReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log.Info("Start reconcile", "name", request.Name)

	// get the related proxy configuration
	config := &proxyv1alpha1.ManagedProxyConfiguration{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: request.Name}, config); err != nil {
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
		return reconcile.Result{}, err
	}

	// deploying central proxy server instances into the hub cluster.
	isModified, err := c.deployProxyServer(config)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "fails to deploy proxy server")
	}
	if !isModified {
		klog.Infof("Proxy server resources are up-to-date")
	}

	// refreshing status
	if err := c.refreshStatus(isModified, config); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (c *ManagedProxyConfigurationReconciler) refreshStatus(isModified bool, config *proxyv1alpha1.ManagedProxyConfiguration) error {
	currentState, err := c.getCurrentState(isModified, config)
	if err != nil {
		return err
	}
	expectingStatus := proxyv1alpha1.ManagedProxyConfigurationStatus{}
	expectingStatus.LastObservedGeneration = config.Generation
	expectingStatus.Conditions = c.getConditions(currentState)
	currentStatus := config.Status.DeepCopy()
	for i := range currentStatus.Conditions {
		currentStatus.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	if !isModified && equality.Semantic.DeepEqual(&expectingStatus, currentStatus) {
		return nil
	}
	editingConfig := config.DeepCopy()
	for _, cond := range expectingStatus.Conditions {
		expectingCondition := cond
		meta.SetStatusCondition(&editingConfig.Status.Conditions, expectingCondition)
	}
	return c.Client.Status().Update(context.TODO(), editingConfig)
}

func (c *ManagedProxyConfigurationReconciler) deployProxyServer(config *proxyv1alpha1.ManagedProxyConfiguration) (bool, error) {
	resources := []client.Object{
		newServiceAccount(config),
		newProxyService(config),
		newProxySecret(config, c.SelfSigner.CAData()),
		newProxyServerDeployment(config),
		newProxyServerRole(config),
		newProxyServerRoleBinding(config),
	}
	anyCreated := false
	createdKinds := sets.NewString()
	anyUpdated := false
	updatedKinds := sets.NewString()
	for _, resource := range resources {
		gvks, _, err := c.Scheme().ObjectKinds(resource)
		if err != nil {
			return false, err
		}
		if len(gvks) != 1 {
			return false, fmt.Errorf("invalid gvks received: %v", gvks)
		}
		gvk := gvks[0]

		created, updated, err := c.ensure(config.Generation, gvk, resource)
		if err != nil {
			return false, err
		}
		if created {
			createdKinds.Insert(gvk.Kind)
		}
		if updated {
			updatedKinds.Insert(gvk.Kind)
		}
		anyCreated = anyCreated || created
		anyUpdated = anyUpdated || updated
	}
	if anyCreated {
		c.EventRecorder.ForComponent("ClusterManagementAddonReconciler").
			Eventf("ProxyServerCreated", "Resources are created: %v", createdKinds)
	}
	if anyUpdated {
		c.EventRecorder.ForComponent("ClusterManagementAddonReconciler").
			Eventf("ProxyServerUpdated", "Resources are updated: %v", updatedKinds)
	}
	return anyCreated || anyUpdated, nil
}

func (c *ManagedProxyConfigurationReconciler) ensure(incomingGeneration int64, gvk schema.GroupVersionKind, resource client.Object) (bool, bool, error) {
	// appending a label to all the applied resources so that they can always be
	// updated upon the configuration changes.
	annotations := resource.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[common.AnnotationKeyConfigurationGeneration] = strconv.Itoa(int(incomingGeneration))
	resource.SetAnnotations(annotations)

	created := false
	updated := false
	// create if it doesn't exist
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)
	if err := c.Client.Get(
		context.TODO(),
		types.NamespacedName{
			Namespace: resource.GetNamespace(),
			Name:      resource.GetName(),
		}, current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, false, errors.Wrapf(err,
				"failed to get resource kind: %s, namespace: %s, name %s",
				gvk.Kind,
				resource.GetNamespace(),
				resource.GetName(),
			)
		}
		// if not found, then create
		if err := c.Client.Create(context.TODO(), resource); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return false, false, errors.Wrapf(err,
					"failed to create resource kind: %s, namespace: %s, name %s",
					gvk.Kind,
					resource.GetNamespace(),
					resource.GetName(),
				)
			}
		} else {
			created = true
		}
	}

	var currentGeneration = 0
	if current.GetAnnotations() != nil && len(current.GetAnnotations()[common.AnnotationKeyConfigurationGeneration]) > 0 {
		var err error
		currentGeneration, err = strconv.Atoi(current.GetAnnotations()[common.AnnotationKeyConfigurationGeneration])
		if err != nil {
			return false, false, errors.Wrapf(err, "failed reading current generation for %v", gvk.Kind)
		}
	}
	// EXCEPTIONS
	// short-circuiting for service resources to avoid duplicated cluster-ip assignment
	if gvk.Group == "" && gvk.Kind == "Service" {
		return created, false, nil
	}

	// update if generation bumped
	if !created && int(incomingGeneration) > currentGeneration {
		resource.SetResourceVersion(current.GetResourceVersion())
		if err := c.Client.Update(context.TODO(), resource); err != nil {
			if apierrors.IsConflict(err) {
				return c.ensure(incomingGeneration, gvk, resource)
			}
			return false, false, errors.Wrapf(err,
				"failed to update resource kind: %s, namespace: %s, name %s",
				gvk.Kind,
				resource.GetNamespace(),
				resource.GetName(),
			)
		} else {
			updated = true
		}
	}
	return created, updated, nil
}

func (c *ManagedProxyConfigurationReconciler) getConditions(s *state) []metav1.Condition {
	deployedCondition := metav1.Condition{
		Type:    proxyv1alpha1.ConditionTypeProxyServerDeployed,
		Status:  metav1.ConditionFalse,
		Reason:  "NotYetDeployed",
		Message: "Replicas: " + strconv.Itoa(s.replicas),
	}
	if s.isCurrentlyDeployed {
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

func (c *ManagedProxyConfigurationReconciler) ensureEntrypoint(config *proxyv1alpha1.ManagedProxyConfiguration) (string, error) {
	if config.Spec.ProxyServer.Entrypoint.Type == proxyv1alpha1.EntryPointTypeLoadBalancerService {
		proxyService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   config.Spec.ProxyServer.Namespace,
				Name:        config.Spec.ProxyServer.Entrypoint.LoadBalancerService.Name,
				Annotations: getAnnotation(config.Spec.ProxyServer.Entrypoint.LoadBalancerService.Annotations),
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
	return "", nil
}

func (c *ManagedProxyConfigurationReconciler) ensureRotation(config *proxyv1alpha1.ManagedProxyConfiguration, entrypoint string) error {
	var hostNames []string
	if config.Spec.Authentication.Signer.SelfSigned != nil {
		hostNames = config.Spec.Authentication.Signer.SelfSigned.AdditionalSANs
	}
	sans := append(
		hostNames,
		"127.0.0.1",
		"localhost",
		entrypoint,
		config.Spec.ProxyServer.InClusterServiceName+"."+config.Spec.ProxyServer.Namespace,
		config.Spec.ProxyServer.InClusterServiceName+"."+config.Spec.ProxyServer.Namespace+".svc")
	if config.Spec.ProxyServer.Entrypoint != nil && config.Spec.ProxyServer.Entrypoint.Type == proxyv1alpha1.EntryPointTypeHostname {
		sans = append(sans, config.Spec.ProxyServer.Entrypoint.Hostname.Value)
	}

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
		return errors.Wrapf(err, "fails to rotate proxy server cert")
	}

	// agent sever cert rotation
	agentServerRotator := c.newCertRotatorFunc(
		config.Spec.ProxyServer.Namespace,
		config.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName,
		sans...)
	if err := agentServerRotator.EnsureTargetCertKeyPair(c.CAPair, c.CAPair.Config.Certs); err != nil {
		return errors.Wrapf(err, "fails to rotate proxy agent cert")
	}

	// proxy client cert rotation
	proxyClientRotator := c.newCertRotatorFunc(
		config.Spec.ProxyServer.Namespace,
		config.Spec.Authentication.Dump.Secrets.SigningProxyClientSecretName,
		sans...)
	if err := proxyClientRotator.EnsureTargetCertKeyPair(c.CAPair, c.CAPair.Config.Certs, tweakClientCertUsageFunc); err != nil {
		return errors.Wrapf(err, "fails to rotate proxy client cert")
	}

	if !c.supportsV1CSR {
		// agent client cert rotation
		// beta CSR running under Kubernetes release lower than 1.18 doesn't
		// support custom CSR signer. so we need to sign the secret in the hub
		// then apply it to the managed cluster via ManifestWork.
		proxyClientRotator := c.newCertRotatorFunc(
			config.Spec.ProxyServer.Namespace,
			common.AgentClientSecretName,
			sans...)
		if err := proxyClientRotator.EnsureTargetCertKeyPair(c.CAPair, c.CAPair.Config.Certs, tweakClientCertUsageFunc); err != nil {
			return errors.Wrapf(err, "fails to rotate proxy client cert")
		}
	}

	return nil
}

func (c *ManagedProxyConfigurationReconciler) ensureBasicResources(config *proxyv1alpha1.ManagedProxyConfiguration) error {
	if err := c.ensureNamespace(config); err != nil {
		return err
	}
	return nil
}

func (c *ManagedProxyConfigurationReconciler) ensureNamespace(config *proxyv1alpha1.ManagedProxyConfiguration) error {
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

type state struct {
	isCurrentlyDeployed       bool
	isRedeployed              bool
	isSigned                  bool
	replicas                  int
	proxyServerCertExpireTime *metav1.Time
	agentServerCertExpireTime *metav1.Time
}

func (c *ManagedProxyConfigurationReconciler) getCurrentState(isRedeployed bool, config *proxyv1alpha1.ManagedProxyConfiguration) (*state, error) {
	namespace := config.Spec.ProxyServer.Namespace
	name := config.Name
	isCurrentlyDeployed := true
	scale, err := c.DeploymentGetter.Deployments(namespace).GetScale(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			isCurrentlyDeployed = false
		} else {
			return nil, err
		}
	}
	isSigned := true
	proxyServerSecret, err := c.SecretGetter.Secrets(namespace).
		Get(context.TODO(),
			config.Spec.Authentication.Dump.Secrets.SigningProxyServerSecretName,
			metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			isSigned = false
		} else {
			return nil, err
		}
	}

	agentServerSecret, err := c.SecretGetter.Secrets(namespace).
		Get(context.TODO(),
			config.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName,
			metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			isSigned = false
		} else {
			return nil, err
		}
	}
	return &state{
		isCurrentlyDeployed:       isCurrentlyDeployed,
		isRedeployed:              isRedeployed,
		isSigned:                  isSigned,
		replicas:                  int(scale.Status.Replicas),
		proxyServerCertExpireTime: getPEMCertExpireTime(proxyServerSecret.Data[corev1.TLSCertKey]),
		agentServerCertExpireTime: getPEMCertExpireTime(agentServerSecret.Data[corev1.TLSCertKey]),
	}, nil
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

func getAnnotation(list []proxyv1alpha1.AnnotationVar) map[string]string {
	if len(list) == 0 {
		return nil
	}
	annotation := make(map[string]string, len(list))
	for _, v := range list {
		if errs := validation.IsQualifiedName(v.Key); len(errs) == 0 {
			klog.Warningf("Annotation key %s validate failed: %s, skip it!", v.Key, strings.Join(errs, ";"))
			continue
		}
		if errs := validation.IsValidLabelValue(v.Value); len(errs) > 0 {
			klog.Warningf("Annotation value %s validate failed: %s, skip it!", v.Key, strings.Join(errs, ";"))
			continue
		}
		annotation[v.Key] = v.Value
	}
	return annotation
}

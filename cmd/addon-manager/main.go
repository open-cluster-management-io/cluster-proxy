/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"os"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"open-cluster-management.io/addon-framework/pkg/addonmanager"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	"open-cluster-management.io/api/client/addon/clientset/versioned"
	"open-cluster-management.io/api/client/addon/informers/externalversions"
	"open-cluster-management.io/cluster-proxy/controllers"
	"open-cluster-management.io/cluster-proxy/pkg/addon/agent"
	"open-cluster-management.io/cluster-proxy/pkg/addon/operator/authentication/selfsigned"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/config"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(addonv1alpha1.AddToScheme(scheme))
	utilruntime.Must(proxyv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var signerSecretNamespace, signerSecretName string

	klog.SetOutput(os.Stdout)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":58080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":58081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&signerSecretNamespace, "signer-secret-namespace", "default",
		"The namespace of the secret to store the signer CA")
	flag.StringVar(&signerSecretName, "signer-secret-name", "cluster-proxy-signer",
		"The name of the secret to store the signer CA")
	flag.StringVar(&config.AgentImageName, "agent-image-name",
		config.AgentImageName,
		"The name of the addon agent's image")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if err := config.ValidateAgentImage(); err != nil {
		setupLog.Error(err, "failed to validate agent image name")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cluster-proxy-addon-manager",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	client, err := versioned.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to set up addon client")
		os.Exit(1)
	}
	nativeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to set up kubernetes native client")
		os.Exit(1)
	}

	informerFactory := externalversions.NewSharedInformerFactory(client, 0)
	nativeInformer := informers.NewSharedInformerFactoryWithOptions(nativeClient, 0)

	// loading self-signer
	selfSigner, err := selfsigned.NewSelfSignerFromSecretOrGenerate(
		nativeClient, signerSecretNamespace, signerSecretName)
	if err != nil {
		setupLog.Error(err, "failed loading self-signer")
		os.Exit(1)
	}

	if err = (&controllers.ClusterManagementAddonReconciler{
		Client:           mgr.GetClient(),
		SelfSigner:       selfSigner,
		CAPair:           selfSigner.CA(),
		SecretGetter:     nativeClient.CoreV1(),
		SecretLister:     nativeInformer.Core().V1().Secrets().Lister(),
		ServiceGetter:    nativeClient.CoreV1(),
		DeploymentGetter: nativeClient.AppsV1(),
		EventRecorder:    events.NewInMemoryRecorder("ClusterManagementAddonReconciler"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ManagedProxyConfiguration")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	addonManager, err := addonmanager.New(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	addonManager.AddAgent(agent.NewProxyAgent(
		mgr.GetClient(),
		nativeClient,
		selfSigner,
	))

	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	go informerFactory.Start(ctx.Done())
	go nativeInformer.Start(ctx.Done())
	go addonManager.Start(ctx)
	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

package controllers

import (
	"context"
	"os"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	proxyclient "open-cluster-management.io/cluster-proxy/pkg/generated/clientset/versioned"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func init() {
	logger := klogr.New()
	log.SetLogger(logger)

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

var (
	scheme = runtime.NewScheme()

	metricsAddr, probeAddr string
	enableLeaderElection   bool
)

func NewControllersCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "controllers",
		Short: "controllers",
		Run: func(cmd *cobra.Command, args []string) {
			err := runControllerManager()
			if err != nil {
				klog.Fatal(err, "unable to run controller manager")
			}
		},
	}

	addFlags(cmd)
	addFlagsForCertController(cmd)

	return cmd
}

func addFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	cmd.Flags().StringVar(&probeAddr, "health-probe-addr", ":8081", "The address the probe endpoint binds to.")
	cmd.Flags().BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
}

func runControllerManager() error {
	ctx, cancel := context.WithCancel(signals.SetupSignalHandler())
	defer cancel()

	// Setup clients, informers and listers.
	kubeConfig := config.GetConfigOrDie()

	// secret client and lister
	nativeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}
	secertClient := nativeClient.CoreV1()
	informerFactory := informers.NewSharedInformerFactory(nativeClient, 10*time.Minute)
	secertLister := informerFactory.Core().V1().Secrets().Lister()

	go informerFactory.Start(ctx.Done())

	// New controller manager
	mgr, err := manager.New(kubeConfig, manager.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: 9443,
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cluster-proxy-service-proxy",
	})
	if err != nil {
		klog.Error(err, "unable to set up overall controller manager")
		return err
	}

	// loading ManagedProxyConfiguration for owner reference
	proxyClient, err := proxyclient.NewForConfig(mgr.GetConfig())
	if err != nil {
		klog.Error(err, "unable to set up proxy client")
		os.Exit(1)
	}
	proxyConfig, err := proxyClient.ProxyV1alpha1().ManagedProxyConfigurations().Get(
		context.TODO(), "cluster-proxy", metav1.GetOptions{})
	if err != nil {
		klog.Error(err, "failed to get ManagedProxyConfiguration 'cluster-proxy'")
		os.Exit(1)
	}
	ownerRef := selfsigned.NewOwnerReferenceFromConfig(proxyConfig)

	// Register CertController
	err = registerCertController(certificatesNamespace, signerSecretName, signerSecretNamespace,
		secertLister, secertClient, ownerRef, mgr)
	if err != nil {
		klog.Error(err, "unable to set up cert-controller")
		return err
	}

	if err := mgr.Start(ctx); err != nil {
		klog.Error(err, "problem running manager")
		return err
	}
	return nil
}

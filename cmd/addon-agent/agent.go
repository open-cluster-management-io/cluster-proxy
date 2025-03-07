package agent

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	"open-cluster-management.io/addon-framework/pkg/lease"
	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// envKeyPodNamespace represents the environment variable key for the addon agent namespace.
const envKeyPodNamespace = "POD_NAMESPACE"

func NewAgent() *cobra.Command {
	agentOpts := NewAgentOptions()

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Start the managed service account addon agent",
		Run: func(cmd *cobra.Command, args []string) {
			if err := agentOpts.Run(); err != nil {
				klog.Fatal(err)
			}
		},
	}

	flags := cmd.Flags()
	agentOpts.AddFlags(flags)

	return cmd
}

type AgentOptions struct {
	HubKubeconfig          string
	ClusterName            string
	ProxyServerNamespace   string
	EnablePortForwardProxy bool
}

func NewAgentOptions() *AgentOptions {
	return &AgentOptions{}
}

func (o *AgentOptions) AddFlags(flags *pflag.FlagSet) {
	flag.StringVar(&o.HubKubeconfig, "hub-kubeconfig", "",
		"The kubeconfig to talk to hub cluster")
	flag.StringVar(&o.ClusterName, "cluster-name", "",
		"The name of the managed cluster")
	flag.StringVar(&o.ProxyServerNamespace, "proxy-server-namespace", "open-cluster-management-addon",
		"The namespace where proxy-server pod lives")
	flag.BoolVar(&o.EnablePortForwardProxy, "enable-port-forward-proxy", false,
		"If true, running a local server forwarding tunnel shakes to proxy-server pods")
}

func (o *AgentOptions) Run() error {
	logger := textlogger.NewLogger(textlogger.NewConfig())
	klog.SetOutput(os.Stdout)
	klog.InitFlags(flag.CommandLine)

	// pipe controller-runtime logs to klog
	ctrl.SetLogger(logger)

	cfg, err := clientcmd.BuildConfigFromFlags("", o.HubKubeconfig)
	if err != nil {
		panic(err)
	}
	cfg.UserAgent = "proxy-agent-addon-agent"

	spokeClient, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
	if err != nil {
		panic(fmt.Errorf("failed to create spoke client, err: %w", err))
	}
	addonAgentNamespace := os.Getenv("POD_NAMESPACE")
	if len(addonAgentNamespace) == 0 {
		panic(fmt.Sprintf("Pod namespace is empty, please set the ENV for %s", envKeyPodNamespace))
	}
	leaseUpdater := lease.NewLeaseUpdater(spokeClient, common.AddonName, addonAgentNamespace).
		WithHubLeaseConfig(cfg, o.ClusterName)

	ctx := context.Background()

	readiness := &atomic.Value{}
	readiness.Store(true)
	if o.EnablePortForwardProxy {
		readiness.Store(false)
		klog.Infof("Running local port-forward proxy")
		rr := util.NewRoundRobinLocalProxy(
			cfg,
			readiness,
			o.ProxyServerNamespace,
			common.LabelKeyComponentName+"="+common.ComponentNameProxyServer,
			8091,
		)
		_, err := rr.Listen(ctx)
		if err != nil {
			panic(err)
		}
	}

	// If the certificates is changed, we need to restart the agent to load the new certificates.
	cc, err := addonutils.NewConfigChecker("certificates check", "/etc/tls/tls.crt", "/etc/tls/tls.key")
	if err != nil {
		klog.Fatalf("failed create certificates checker: %v", err)
	}
	cc.SetReload(true)

	go serveHealthProbes(ctx.Done(), ":8888", map[string]healthz.Checker{
		"certificates": cc.Check,
		"port forward proxy readiness": func(_ *http.Request) error {
			if !readiness.Load().(bool) {
				return fmt.Errorf("not ready")
			}
			return nil
		},
	})

	klog.Infof("Starting lease updater")
	leaseUpdater.Start(ctx)
	<-ctx.Done()

	return nil
}

// serveHealthProbes starts a server to check healthz and readyz probes
func serveHealthProbes(stop <-chan struct{}, address string, healthCheckers map[string]healthz.Checker) {
	mux := http.NewServeMux()
	mux.Handle("/healthz", http.StripPrefix("/healthz", &healthz.Handler{Checks: healthCheckers}))

	server := http.Server{
		Handler: mux,
	}

	ln, err := net.Listen("tcp", address)
	if err != nil {
		klog.Errorf("error listening on %s: %v", address, err)
		return
	}

	klog.Infof("heath probes server is running...")
	// Run server
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			klog.Fatal(err)
		}
	}()

	// Shutdown the server when stop is closed
	<-stop
	if err := server.Shutdown(context.Background()); err != nil {
		klog.Fatal(err)
	}
}

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

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

var (
	hubKubeconfig               string
	clusterName                 string
	proxyServerNamespace        string
	enablePortForwardProxy      bool
	enableProxyAgentHealthCheck bool
)

// envKeyPodNamespace represents the environment variable key for the addon agent namespace.
const envKeyPodNamespace = "POD_NAMESPACE"

// proxyAgentHealthAddr is the address of the proxy-agent health server.
// The addon-agent and proxy-agent containers run in the same Pod and share the network namespace,
// so we can access the proxy-agent's health server via localhost.
const proxyAgentHealthAddr = "localhost:8093"

// checkProxyAgentReadiness returns a health check function that checks if the proxy-agent
// is connected to the proxy-server by querying the proxy-agent's /readyz endpoint.
// Since both containers share the same network namespace within the Pod, this function
// can reach the proxy-agent's health server at localhost:8093.
func checkProxyAgentReadiness() func() bool {
	client := &http.Client{Timeout: 5 * time.Second}
	return func() bool {
		resp, err := client.Get(fmt.Sprintf("http://%s/readyz", proxyAgentHealthAddr))
		if err != nil {
			klog.V(4).Infof("Failed to check proxy-agent readiness: %v", err)
			return false
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return true
		}
		klog.V(4).Infof("Proxy-agent not ready, status code: %d", resp.StatusCode)
		return false
	}
}

func main() {

	logger := textlogger.NewLogger(textlogger.NewConfig())
	klog.SetOutput(os.Stdout)
	klog.InitFlags(flag.CommandLine)
	flag.StringVar(&hubKubeconfig, "hub-kubeconfig", "",
		"The kubeconfig to talk to hub cluster")
	flag.StringVar(&clusterName, "cluster-name", "",
		"The name of the managed cluster")
	flag.StringVar(&proxyServerNamespace, "proxy-server-namespace", "open-cluster-management-addon",
		"The namespace where proxy-server pod lives")
	flag.BoolVar(&enablePortForwardProxy, "enable-port-forward-proxy", false,
		"If true, running a local server forwarding tunnel shakes to proxy-server pods")
	flag.BoolVar(&enableProxyAgentHealthCheck, "enable-proxy-agent-health-check", true,
		"If true, check proxy-agent connection status before updating lease")
	flag.Parse()

	// pipe controller-runtime logs to klog
	ctrl.SetLogger(logger)

	cfg, err := clientcmd.BuildConfigFromFlags("", hubKubeconfig)
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

	var leaseUpdater lease.LeaseUpdater
	if enableProxyAgentHealthCheck {
		klog.Infof("Proxy-agent health check enabled, lease will only update when proxy-agent is connected")
		leaseUpdater = lease.NewLeaseUpdater(
			spokeClient,
			common.AddonName,
			addonAgentNamespace,
			checkProxyAgentReadiness(),
		).WithHubLeaseConfig(cfg, clusterName)
	} else {
		leaseUpdater = lease.NewLeaseUpdater(
			spokeClient,
			common.AddonName,
			addonAgentNamespace,
		).WithHubLeaseConfig(cfg, clusterName)
	}

	ctx := context.Background()

	readiness := &atomic.Value{}
	readiness.Store(true)
	if enablePortForwardProxy {
		readiness.Store(false)
		klog.Infof("Running local port-forward proxy")
		rr := util.NewRoundRobinLocalProxy(
			cfg,
			readiness,
			proxyServerNamespace,
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

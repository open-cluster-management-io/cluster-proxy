package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"sync/atomic"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"open-cluster-management.io/cluster-proxy/pkg/addon/health"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/util"
)

var (
	hubKubeconfig          string
	clusterName            string
	proxyServerNamespace   string
	enablePortForwardProxy bool
)

func main() {

	klog.InitFlags(flag.CommandLine)
	flag.StringVar(&hubKubeconfig, "hub-kubeconfig", "",
		"The kubeconfig to talk to hub cluster")
	flag.StringVar(&clusterName, "cluster-name", "",
		"The name of the managed cluster")
	flag.StringVar(&proxyServerNamespace, "proxy-server-namespace", "open-cluster-management-addon",
		"The namespace where proxy-server pod lives")
	flag.BoolVar(&enablePortForwardProxy, "enable-port-forward-proxy", false,
		"If true, running a local server forwarding tunnel shakes to proxy-server pods")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags("", hubKubeconfig)
	if err != nil {
		panic(err)
	}
	cfg.UserAgent = "proxy-agent-addon-agent"

	leaseUpdater, err := health.NewAddonHealthUpdater(cfg, clusterName)
	if err != nil {
		panic(err)
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

	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", "8888"))
	if err != nil {
		klog.Fatalf("failed listening: %v", err)
	}
	go func() {
		klog.Infof("Starting local health check server")
		err := http.Serve(ln, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			if !readiness.Load().(bool) {
				rw.WriteHeader(http.StatusInternalServerError)
				rw.Write([]byte("not yet ready"))
				klog.Infof("not yet ready")
				return
			}
			rw.Write([]byte("ok"))
			return
		}))
		klog.Errorf("health check server aborted: %v", err)
	}()

	klog.Infof("Starting lease updater")
	leaseUpdater.Start(ctx)
	<-ctx.Done()
}

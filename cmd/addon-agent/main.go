package main

import (
	"context"
	"flag"

	"k8s.io/client-go/tools/clientcmd"
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

	if enablePortForwardProxy {
		rr := util.NewRoundRobinLocalProxy(
			cfg,
			proxyServerNamespace,
			common.LabelKeyComponentName+"="+common.ComponentNameProxyServer,
			8091,
		)
		_, err := rr.Listen()
		if err != nil {
			panic(err)
		}
	}

	ctx := context.Background()
	leaseUpdater.Start(ctx)
	<-ctx.Done()
}

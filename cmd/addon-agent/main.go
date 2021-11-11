package main

import (
	"context"
	"flag"

	"k8s.io/client-go/tools/clientcmd"
	"open-cluster-management.io/cluster-proxy/pkg/addon/health"
)

var (
	hubKubeconfig string
	clusterName   string
)

func main() {

	flag.StringVar(&hubKubeconfig, "hub-kubeconfig", "",
		"The kubeconfig to talk to hub cluster")
	flag.StringVar(&clusterName, "cluster-name", "",
		"The name of the managed cluster")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags("", hubKubeconfig)
	if err != nil {
		panic(err)
	}

	leaseUpdater, err := health.NewAddonHealthUpdater(cfg, clusterName)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	leaseUpdater.Start(ctx)
	<-ctx.Done()
}

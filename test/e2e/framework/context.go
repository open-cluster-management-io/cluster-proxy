package framework

import (
	"flag"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"
)

var e2eContext = &E2EContext{}

type E2EContext struct {
	HubKubeConfig string
	TestCluster   string
}

func ParseFlags() {
	registerFlags()
	flag.Parse()
	defaultFlags()
	validateFlags()
}

func registerFlags() {
	flag.StringVar(&e2eContext.HubKubeConfig,
		"hub-kubeconfig",
		os.Getenv("KUBECONFIG"),
		"Path to kubeconfig of the hub cluster.")
	flag.StringVar(&e2eContext.TestCluster,
		"test-cluster",
		"",
		"The target cluster to run the e2e suite.")
}

func defaultFlags() {
	if len(e2eContext.HubKubeConfig) == 0 {
		home := os.Getenv("HOME")
		if len(home) > 0 {
			e2eContext.HubKubeConfig = filepath.Join(home, ".kube", "config")
		}
	}
}

func validateFlags() {
	if len(e2eContext.HubKubeConfig) == 0 {
		klog.Fatalf("--hub-kubeconfig is required")
	}
	if len(e2eContext.TestCluster) == 0 {
		klog.Fatalf("--test-cluster is required")
	}
}

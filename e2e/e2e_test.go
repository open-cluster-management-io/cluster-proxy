package e2e

import (
	"os"
	"testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	"open-cluster-management.io/cluster-proxy/e2e/framework"
	// per-package e2e suite

	//_ "open-cluster-management.io/cluster-proxy/e2e/configuration"
	_ "open-cluster-management.io/cluster-proxy/e2e/install"
)

func TestMain(m *testing.M) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	framework.ParseFlags()
	os.Exit(m.Run())
}

func TestE2E(t *testing.T) {
	RunE2ETests(t)
}

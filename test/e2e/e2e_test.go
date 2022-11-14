package e2e

import (
	"os"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"open-cluster-management.io/cluster-proxy/test/e2e/framework"
	// per-package e2e suite

	_ "open-cluster-management.io/cluster-proxy/test/e2e/certificate"
	_ "open-cluster-management.io/cluster-proxy/test/e2e/connect"
	_ "open-cluster-management.io/cluster-proxy/test/e2e/install"
)

func TestMain(m *testing.M) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	framework.ParseFlags()
	os.Exit(m.Run())
}

func TestE2E(t *testing.T) {
	RunE2ETests(t)
}

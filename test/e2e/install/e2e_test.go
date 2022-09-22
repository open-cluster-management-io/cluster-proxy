package install

import (
	"os"
	"testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"open-cluster-management.io/cluster-proxy/e2e/framework"
)

func TestMain(m *testing.M) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	framework.ParseFlags()
	os.Exit(m.Run())
}

func RunE2ETests(t *testing.T) {
	ginkgo.RunSpecs(t, "ClusterProxy e2e suite -- install tests")
}

func TestE2E(t *testing.T) {
	RunE2ETests(t)
}

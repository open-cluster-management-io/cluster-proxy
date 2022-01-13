module open-cluster-management.io/cluster-proxy

go 1.16

require (
	github.com/onsi/ginkgo v1.16.4
	github.com/onsi/gomega v1.17.0
	github.com/openshift/library-go v0.0.0-20210916194400-ae21aab32431
	github.com/pkg/errors v0.9.1
	github.com/stretchr/testify v1.7.0
	google.golang.org/grpc v1.42.0
	k8s.io/api v0.22.1
	k8s.io/apimachinery v0.22.1
	k8s.io/client-go v0.22.1
	k8s.io/klog/v2 v2.9.0
	k8s.io/utils v0.0.0-20210722164352-7f3ee0f31471
	open-cluster-management.io/addon-framework v0.1.1-0.20220112091819-a7e8cdeae5a4
	open-cluster-management.io/api v0.5.1-0.20220112073018-2d280a97a052
	sigs.k8s.io/apiserver-network-proxy v0.0.27
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.27
	sigs.k8s.io/controller-runtime v0.9.5
)

replace sigs.k8s.io/apiserver-network-proxy/konnectivity-client => sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.27

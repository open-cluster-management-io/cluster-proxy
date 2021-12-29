module open-cluster-management.io/cluster-proxy

go 1.16

require (
	github.com/onsi/ginkgo v1.16.4
	github.com/onsi/gomega v1.14.0
	github.com/openshift/library-go v0.0.0-20210916194400-ae21aab32431
	github.com/pkg/errors v0.9.1
	github.com/stretchr/testify v1.7.0
	google.golang.org/grpc v1.38.0
	k8s.io/api v0.22.1
	k8s.io/apimachinery v0.22.1
	k8s.io/client-go v0.22.1
	k8s.io/klog/v2 v2.9.0
	k8s.io/utils v0.0.0-20210722164352-7f3ee0f31471
	open-cluster-management.io/addon-framework v0.1.1-0.20211223101009-d6b1a7adae93
	open-cluster-management.io/api v0.5.0
	sigs.k8s.io/apiserver-network-proxy v0.0.24
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.24
	sigs.k8s.io/controller-runtime v0.9.5
)

replace (
	k8s.io/api v0.21.1 => k8s.io/api v0.20.2
	k8s.io/apimachinery v0.21.1 => k8s.io/apimachinery v0.20.2
	k8s.io/client-go v0.21.1 => k8s.io/client-go v0.20.2
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client => sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.24
)

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

var managedClusterKubeconfig string
var managedClusterName string

var proxyServerHost string
var proxyServerPort string

// Assumes that the cluster-proxy is installed in the open-cluster-management-addon namespace.
// `proxyCACert` could be found in Secret `proxy-server-ca` in the `open-cluster-management-addon“ namespace.
var proxyCACertPath string

// Assumes that the cluster-proxy is installed in the open-cluster-management-addon namespace.
// `proxyCert` and `proxyKey` could be found in Secret `proxy-client` in the `open-cluster-management-addon“ namespace.
var proxyCertPath string
var proxyKeyPath string

func main() {
	flag.StringVar(&managedClusterKubeconfig, "kubeconfig", "", "the path to kubeconfig")
	flag.StringVar(&managedClusterName, "cluster", "", "the cluster name")
	flag.StringVar(&proxyServerHost, "host", "", "proxy server host")
	flag.StringVar(&proxyServerPort, "port", "", "proxy server port")
	flag.StringVar(&proxyCACertPath, "ca-cert", "", "the path to ca cert")
	flag.StringVar(&proxyCertPath, "cert", "", "the path to tls cert")
	flag.StringVar(&proxyKeyPath, "key", "", "the path to tls key")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags("", managedClusterKubeconfig)
	if err != nil {
		panic(err)
	}
	tlsCfg, err := util.GetClientTLSConfig(proxyCACertPath, proxyCertPath, proxyKeyPath, proxyServerHost, nil)
	if err != nil {
		panic(err)
	}
	dialerTunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
		context.TODO(),
		net.JoinHostPort(proxyServerHost, proxyServerPort),
		grpc.WithTransportCredentials(grpccredentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: time.Second * 5,
		}),
	)
	if err != nil {
		panic(err)
	}

	cfg.Host = managedClusterName
	// TODO: flexible client-side tls server name validation
	cfg.TLSClientConfig.Insecure = true
	cfg.TLSClientConfig.CAData = nil
	cfg.TLSClientConfig.CAFile = ""
	cfg.Dial = dialerTunnel.DialContext
	client := kubernetes.NewForConfigOrDie(cfg)

	ns, err := client.CoreV1().
		Namespaces().
		Get(context.TODO(), "default", metav1.GetOptions{})
	if err != nil {
		panic(err)
	}

	fmt.Printf("%v\n", ns)
}
```

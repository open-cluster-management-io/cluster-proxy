package main

import (
	"context"
	"flag"
	"fmt"
	"net"

	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

var kubeconfig string
var clusterName string
var proxyServerHost string
var proxyServerPort string
var proxyCACertPath string
var proxyCertPath string
var proxyKeyPath string

func main() {

	flag.StringVar(&kubeconfig, "kubeconfig", "", "the path to kubeconfig")
	flag.StringVar(&clusterName, "cluster", "", "the cluster name")
	flag.StringVar(&proxyServerHost, "host", "", "proxy server host")
	flag.StringVar(&proxyServerPort, "port", "", "proxy server port")
	flag.StringVar(&proxyCACertPath, "ca-cert", "", "the path to ca cert")
	flag.StringVar(&proxyCertPath, "cert", "", "the path to tls cert")
	flag.StringVar(&proxyKeyPath, "key", "", "the path to tls key")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}
	tlsCfg, err := util.GetClientTLSConfig(proxyCACertPath, proxyCertPath, proxyKeyPath, proxyServerHost, nil)
	if err != nil {
		panic(err)
	}
	tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
		context.TODO(),
		net.JoinHostPort(proxyServerHost, proxyServerPort),
		grpc.WithTransportCredentials(grpccredentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		panic(err)
	}
	cfg.Host = clusterName
	// TODO: flexible client-side tls server name validation
	cfg.TLSClientConfig.Insecure = true
	cfg.TLSClientConfig.CAData = nil
	cfg.TLSClientConfig.CAFile = ""
	cfg.Dial = tunnel.DialContext
	client := kubernetes.NewForConfigOrDie(cfg)
	ns, err := client.CoreV1().
		Namespaces().
		Get(context.TODO(), "default", metav1.GetOptions{})
	if err != nil {
		panic(err)
	}

	fmt.Printf("%v\n", ns)
}

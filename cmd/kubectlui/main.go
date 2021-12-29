package main

import (
	"context"
	"flag"
	"fmt"

	"k8s.io/klog/v2"
	"open-cluster-management.io/cluster-proxy/pkg/kubectlui"
)

var proxyServerHost string
var proxyServerPort string
var proxyCACertPath string
var proxyCertPath string
var proxyKeyPath string
var serverCert string
var serverKey string
var serverPort int

func main() {
	var err error
	flag.StringVar(&proxyServerHost, "host", "", "proxy server host")
	flag.StringVar(&proxyServerPort, "port", "", "proxy server port")
	flag.StringVar(&proxyCACertPath, "proxy-ca-cert", "", "the path to ca cert")
	flag.StringVar(&proxyCertPath, "proxy-cert", "", "the path to tls cert")
	flag.StringVar(&proxyKeyPath, "proxy-key", "", "the path to tls key")
	flag.StringVar(&serverCert, "server-cert", "", "the cert for server")
	flag.StringVar(&serverKey, "server-key", "", "the key for server")
	flag.IntVar(&serverPort, "server-port", 8080, "the port for server")
	flag.Parse()

	fmt.Println("proxy-server-ca-cert", proxyCACertPath)

	kui := kubectlui.NewKubectlUI(proxyServerHost, proxyServerPort,
		proxyCACertPath, proxyCertPath, proxyKeyPath,
		serverCert, serverKey, serverPort)
	err = kui.Start(context.Background())
	if err != nil {
		klog.Fatal(err)
	}
}

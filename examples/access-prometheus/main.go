package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/pkg/errors"
	prometheusapi "github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"
	clusterproxyclient "open-cluster-management.io/cluster-proxy/client"
	konnectivityclient "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

var kubeconfig string
var managedcluster string
var token string // The token is the token of `openshift-monitoring/prometheus-k8s` service account.

var proxyServerHost string
var proxyServerPort string

// Assumes that the cluster-proxy is installed in the multicluster-engine namespace.
// `proxyCACert` could be found in Secret `proxy-server-ca` in the `multicluster-engine` namespace.
var proxyCACertPath string

// Assumes that the cluster-proxy is installed in the multicluster-engine namespace.
// `proxyCert` and `proxyKey` could be found in Secret `proxy-client` in the `multicluster-engine` namespace.
var proxyCertPath string
var proxyKeyPath string

// You can also run the following command to get credientials:
/*
k get secret -n multicluster-engine proxy-server-ca -o jsonpath='{.data.ca\.crt}' | base64 -D > ./temp/ca.crt && \
k get secret -n multicluster-engine proxy-client -o jsonpath='{.data.tls\.crt}' | base64 -D > ./temp/tls.crt && \
k get secret -n multicluster-engine proxy-client -o jsonpath='{.data.tls\.key}' | base64 -D > ./temp/tls.key
*/

func main() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&managedcluster, "managed-cluster", "", "the name of the managed cluster")
	flag.StringVar(&token, "token", "", "the token of the managed cluster")
	flag.StringVar(&proxyServerHost, "host", "", "proxy server host")
	flag.StringVar(&proxyServerPort, "port", "", "proxy server port")
	flag.StringVar(&proxyCACertPath, "ca-cert", "", "the path to ca cert")
	flag.StringVar(&proxyCertPath, "cert", "", "the path to tls cert")
	flag.StringVar(&proxyKeyPath, "key", "", "the path to tls key")
	flag.Parse()

	// Step1: Get "proxy dialer" based on konnectivity client
	tlsCfg, err := util.GetClientTLSConfig(proxyCACertPath, proxyCertPath, proxyKeyPath, proxyServerHost, nil)
	if err != nil {
		panic(err)
	}
	proxyDialer, err := konnectivityclient.CreateSingleUseGrpcTunnel(
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

	// Step2: Get the "proxy Host" based on cluster-proxy client
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}
	proxyHost, err := clusterproxyclient.GetProxyHost(context.Background(), cfg, managedcluster, "openshift-monitoring", "prometheus-k8s")
	if err != nil {
		panic(errors.Wrap(err, "failed to get proxy host"))
	}

	// Step3: Using the token as the bearer token to access the prometheus
	roundTripper, err := transport.NewBearerAuthWithRefreshRoundTripper("", token, &http.Transport{
		DialContext:         proxyDialer.DialContext,
		TLSHandshakeTimeout: 2 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})
	if err != nil {
		panic(err)
	}

	// Step4: Replace the default dialer with the proxy dialer and replace the default host with the proxy host.
	pclient, err := prometheusapi.NewClient(prometheusapi.Config{
		Address:      "https://" + proxyHost + ":9091",
		RoundTripper: roundTripper,
	})
	if err != nil {
		panic(err)
	}
	papiclient := prometheusv1.NewAPI(pclient)

	result, _, err := papiclient.Query(context.Background(), "machine_cpu_sockets", time.Now())
	if err != nil {
		panic(errors.Wrap(err, "failed to query prometheus"))
	}
	fmt.Println(result)
}

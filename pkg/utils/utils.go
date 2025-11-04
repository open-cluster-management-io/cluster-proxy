package utils

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	HEADERSERVICECA   = "Service-Root-Ca"
	HEADERSERVICECERT = "Service-Client-Cert"
	HEADERSERVICEKEY  = "Service-Client-Key"

	// Cluster-Proxy custom headers for service proxy
	HeaderClusterProxyProto     = "Cluster-Proxy-Proto"
	HeaderClusterProxyNamespace = "Cluster-Proxy-Namespace"
	HeaderClusterProxyService   = "Cluster-Proxy-Service"
	HeaderClusterProxyPort      = "Cluster-Proxy-Port"
)

// TargetServiceConfig is a collection of data extrict from the request URL description the target service we can to access on the managed cluster.
// There are 2 usages of it:
// 1. used in function `ServiceProxyURL` to construct the target service URL.
// 2. used in function `UpdateRequest` to update the request object.
type TargetServiceConfig struct {
	Cluster   string
	Proto     string
	Service   string
	Namespace string
	Port      string
	Path      string
}

func UpdateRequest(t TargetServiceConfig, req *http.Request) *http.Request {
	// update request URL path
	req.URL.Path = t.Path

	// populate proto, namespace, service, and port to request headers
	req.Header.Set(HeaderClusterProxyProto, t.Proto)
	req.Header.Set(HeaderClusterProxyNamespace, t.Namespace)
	req.Header.Set(HeaderClusterProxyService, t.Service)
	req.Header.Set(HeaderClusterProxyPort, t.Port)

	return req
}

// GetTargetServiceConfig extrict the target service config from requestURL
// input: https://<route location cluster-proxy>/cluster1/api/v1/namespaces/default/services/<https:helloworld:8080>/proxy-service/ping?time-out=32s
// output: TargetServiceConfig{Cluster: cluster1, Proto: https, Service: helloworld, Namespace: default, Port: 8080, Path: /ping}
func GetTargetServiceConfig(requestURL string) (ts TargetServiceConfig, err error) {
	urlparams := strings.Split(requestURL, "/")
	if len(urlparams) < 9 {
		err = fmt.Errorf("requestURL format not correct, path less than 9: %s", requestURL)
		return
	}

	namespace := urlparams[5]

	proto, service, port, valid := utilnet.SplitSchemeNamePort(urlparams[7])
	if !valid {
		return TargetServiceConfig{}, fmt.Errorf("invalid service name %q", urlparams[7])
	}
	if proto == "" {
		proto = "https" // set a default to https
	}

	servicePath := strings.Join(urlparams[9:], "/")
	servicePath = strings.Split(servicePath, "?")[0] //we only need path here, the proxy pkg would add params back

	return TargetServiceConfig{
		Cluster:   urlparams[1],
		Proto:     proto,
		Service:   service,
		Namespace: namespace,
		Port:      port,
		Path:      servicePath,
	}, nil
}

// GetTargetServiceConfigForKubeAPIServer extrict the kube apiserver config from requestURL
// input: https://<route location cluster-proxy>/cluster1/api/pods?timeout=32s
// output: TargetServiceConfig{Cluster: cluster1, Proto: https, Service: kubernetes, Namespace: default, Port: 443, Path: api/pods}
func GetTargetServiceConfigForKubeAPIServer(requestURL string) (ts TargetServiceConfig, err error) {
	ts = TargetServiceConfig{
		Proto:     "https",
		Service:   "kubernetes",
		Namespace: "default",
		Port:      "443",
	}

	paths := strings.Split(requestURL, "/")
	if len(paths) <= 2 {
		err = fmt.Errorf("requestURL format not correct, path more than 2: %s", requestURL)
		return
	}
	kubeAPIPath := strings.Join(paths[2:], "/")      // api/pods?timeout=32s
	kubeAPIPath = strings.Split(kubeAPIPath, "?")[0] // api/pods note: we only need path here, the proxy pkg would add params back

	ts.Cluster = paths[1]
	ts.Path = kubeAPIPath
	return ts, nil
}

// GetTargetServiceURLFromRequest is used on the agent side, the service-proxy agent recived a request from the proxy-agent, and need to know the target service URL to do further proxy.
func GetTargetServiceURLFromRequest(req *http.Request) (*url.URL, error) {
	// get proto, namespace, service, and port from request headers
	proto := req.Header.Get(HeaderClusterProxyProto)
	namespace := req.Header.Get(HeaderClusterProxyNamespace)
	service := req.Header.Get(HeaderClusterProxyService)
	port := req.Header.Get(HeaderClusterProxyPort)

	// validate proto, namespace, service, and port
	if proto == "" || namespace == "" || service == "" || port == "" {
		return nil, fmt.Errorf("invalid request headers")
	}

	var targetServiceURL string
	// check if the request is meant to proxy to kube-apiserver
	if proto == "https" && service == "kubernetes" && namespace == "default" && port == "443" {
		targetServiceURL = "https://kubernetes.default.svc"
	} else {
		targetServiceURL = fmt.Sprintf("%s://%s.%s.svc:%s", proto, service, namespace, port)
	}

	url, err := url.Parse(targetServiceURL)
	if err != nil {
		return nil, err
	}

	return url, nil
}

const (
	ProxyTypeService = iota
	ProxyTypeKubeAPIServer
)

// GetProxyType determines whether a request meant to proxy to a regular service or the kube-apiserver of the managed cluster.
// An example of service: https://<route location cluster-proxy>/<managed_cluster_name>/api/v1/namespaces/<namespace_name>/services/<[https:]service_name[:port_name]>/proxy-service/<service_path>
// An example of kube-apiserver: https://<route location cluster-proxy>/<managed_cluster_name>/api/pods?timeout=32s
func GetProxyType(reqURI string) int {
	urlparams := strings.Split(reqURI, "/")
	if len(urlparams) > 9 && urlparams[8] == "proxy-service" {
		return ProxyTypeService
	}
	return ProxyTypeKubeAPIServer
}

// ServeHealthProbes serves health probes and configchecker.
func ServeHealthProbes(healthProbeBindAddress string, customChecks ...healthz.Checker) error {
	mux := http.NewServeMux()

	checks := map[string]healthz.Checker{
		"healthz-ping": healthz.Ping,
	}

	for i, check := range customChecks {
		checks[fmt.Sprintf("custom-healthz-checker-%d", i)] = check
	}

	mux.Handle("/healthz", http.StripPrefix("/healthz", &healthz.Handler{Checks: checks}))
	server := http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		Addr:              healthProbeBindAddress,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	klog.Infof("heath probes server is running...")
	return server.ListenAndServe()
}

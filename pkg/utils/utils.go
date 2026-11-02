package utils

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	utilnet "k8s.io/apimachinery/pkg/util/net"
)

const (
	HEADERSERVICECA   = "Service-Root-Ca"
	HEADERSERVICECERT = "Service-Client-Cert"
	HEADERSERVICEKEY  = "Service-Client-Key"

	KubeAPIServerHost = "kubernetes.default.svc"

	kubeAPIServerService   = "kubernetes"
	kubeAPIServerNamespace = "default"

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
	// Path is still percent-encoded, e.g. "files/a%2Fb".
	Path string
}

func UpdateRequest(t TargetServiceConfig, req *http.Request) *http.Request {
	// update request URL path
	setEscapedPath(req.URL, t.Path)

	// populate proto, namespace, service, and port to request headers
	req.Header.Set(HeaderClusterProxyProto, t.Proto)
	req.Header.Set(HeaderClusterProxyNamespace, t.Namespace)
	req.Header.Set(HeaderClusterProxyService, t.Service)
	req.Header.Set(HeaderClusterProxyPort, t.Port)

	return req
}

// setEscapedPath sets the path of u while keeping the encoding of escapedPath,
// so that the proxy does not escape it again (e.g. "%2F" -> "%252F").
func setEscapedPath(u *url.URL, escapedPath string) {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(escapedPath))
	for i := 0; i < len(escapedPath); i++ {
		c := escapedPath[i]
		if isRawPathByte(c) || (c == '%' && isEscape(escapedPath[i:])) {
			b.WriteByte(c)
			continue
		}
		// bytes such as '{' must be escaped, otherwise url.URL ignores RawPath
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0xF])
	}
	u.RawPath = b.String()
	u.Path, _ = url.PathUnescape(u.RawPath)
}

// isEscape reports whether s starts with an escape such as "%2F".
func isEscape(s string) bool {
	return len(s) >= 3 && isHex(s[1]) && isHex(s[2])
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// isRawPathByte reports whether c can be left unescaped in url.URL.RawPath.
func isRawPathByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		strings.IndexByte("-._~!$&'()*+,;=:@/[]", c) >= 0
}

// GetTargetServiceConfig extrict the target service config from requestURL
// input: https://<route location cluster-proxy>/cluster1/api/v1/namespaces/default/services/<https:helloworld:8080>/proxy-service/ping?time-out=32s
// output: TargetServiceConfig{Cluster: cluster1, Proto: https, Service: helloworld, Namespace: default, Port: 8080, Path: ping}
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

// GetTargetServiceURLFromRequest is used on the agent side, the service-proxy agent received a request from the proxy-agent, and need to know the target service URL to do further proxy.
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
	if strings.EqualFold(service, kubeAPIServerService) && strings.EqualFold(namespace, kubeAPIServerNamespace) {
		targetServiceURL = "https://" + KubeAPIServerHost
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

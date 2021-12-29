package kubectlui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"k8s.io/klog/v2"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

type KubectlUI struct {
	proxyServerHost string
	proxyServerPort string

	proxyCAPath   string
	proxyCertPath string
	proxyKeyPath  string

	serverCert string
	serverKey  string
	serverPort int
}

func NewKubectlUI(proxyServerHost, proxyServerPort,
	proxyCAPath, proxyCertPath, proxyKeyPath,
	serverCert, serverKey string, serverPort int) *KubectlUI {
	return &KubectlUI{
		proxyServerHost: proxyServerHost,
		proxyServerPort: proxyServerPort,
		proxyCAPath:     proxyCAPath,
		proxyCertPath:   proxyCertPath,
		proxyKeyPath:    proxyKeyPath,
		serverCert:      serverCert,
		serverKey:       serverKey,
		serverPort:      serverPort,
	}
}

func (k *KubectlUI) handler(wr http.ResponseWriter, req *http.Request) {
	if klog.V(4).Enabled() {
		dump, err := httputil.DumpRequest(req, true)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
		klog.V(4).Infof("request:\n%s", string(dump))
	}

	// parse clusterID from current requestURL
	clusterID, kubeAPIPath, err := parseRequestURL(req.RequestURI)
	if err != nil {
		http.Error(wr, err.Error(), http.StatusBadRequest)
		return
	}

	// TODO: check if the addonclient is exist
	// and this is why the code should be in the cluster-proxy repo
	// because crds are different
	// Here we should import repo of cluster-proxy as client

	// restruct new apiserverURL
	target := fmt.Sprintf("http://%s", clusterID)
	apiserverURL, err := url.Parse(target)
	if err != nil {
		http.Error(wr, err.Error(), http.StatusBadRequest)
		return
	}

	var proxyConn net.Conn
	defer func() {
		if proxyConn != nil {
			err = proxyConn.Close()
			if err != nil {
				klog.Errorf("connection closed: %v", err)
			}
		}
	}()

	proxyTLSCfg, err := util.GetClientTLSConfig(k.proxyCAPath, k.proxyCertPath, k.proxyKeyPath, k.proxyServerHost, nil)
	if err != nil {
		http.Error(wr, err.Error(), http.StatusInternalServerError)
		return
	}

	// TODO reuse connection
	// instantiate a gprc proxy dialer
	tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
		context.TODO(),
		net.JoinHostPort(k.proxyServerHost, k.proxyServerPort),
		grpc.WithTransportCredentials(grpccredentials.NewTLS(proxyTLSCfg)),
	)
	if err != nil {
		http.Error(wr, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(apiserverURL)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// golang http pkg automaticly upgrade http connection to http2 connection, but http2 can not upgrade to SPDY which used in "kubectl exec".
		// set ForceAttemptHTTP2 = false to prevent auto http2 upgration
		ForceAttemptHTTP2: false,
		DialContext:       tunnel.DialContext,
	}

	// Skip server-auth for kube-apiserver
	tlsClientConfig := transport.TLSClientConfig.Clone()
	tlsClientConfig.InsecureSkipVerify = true
	transport.TLSClientConfig = tlsClientConfig

	proxy.Transport = transport

	proxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, e error) {
		rw.Write([]byte(fmt.Sprintf("proxy to anp-proxy-server failed because %v", err)))
	}

	// update request URL path
	req.URL.Path = kubeAPIPath
	// update proto
	req.Proto = "http"
	klog.V(4).Infof("request scheme:%s; rawQuery:%s; path:%s", req.URL.Scheme, req.URL.RawQuery, req.URL.Path)

	proxy.ServeHTTP(wr, req)
}

func parseRequestURL(requestURL string) (clusterID string, kubeAPIPath string, err error) {
	paths := strings.Split(requestURL, "/")
	if len(paths) <= 2 {
		err = fmt.Errorf("requestURL format not correct, path more than 2: %s", requestURL)
		return
	}
	clusterID = paths[1]                             // <clusterID>
	kubeAPIPath = strings.Join(paths[2:], "/")       // api/pods?timeout=32s
	kubeAPIPath = strings.Split(kubeAPIPath, "?")[0] // api/pods note: we only need path here, the proxy pkg would add params back
	return
}

func (k *KubectlUI) Start(ctx context.Context) error {
	var err error

	klog.Infof("start https server on %d", k.serverPort)
	http.HandleFunc("/", k.handler)

	// for test sakes, here support
	err = http.ListenAndServe(fmt.Sprintf(":%d", k.serverPort), nil)
	if err != nil {
		klog.Fatalf("failed to start user proxy server: %v", err)
	}

	// err = http.ListenAndServeTLS(fmt.Sprintf(":%d", k.serverPort), k.serverCert, k.serverKey, nil)
	// if err != nil {
	// 	klog.Fatalf("failed to start user proxy server: %v", err)
	// }

	return nil
}

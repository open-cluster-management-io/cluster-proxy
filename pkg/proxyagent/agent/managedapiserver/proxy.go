package managedapiserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	addonmetrics "open-cluster-management.io/cluster-proxy/pkg/metrics"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

type Proxy struct {
	ManagedKubeconfig      string
	Listen                 string
	DialTimeout            time.Duration
	HealthProbeBindAddress string
}

func NewCommand() *cobra.Command {
	proxy := &Proxy{
		Listen:                 ":8443",
		DialTimeout:            30 * time.Second,
		HealthProbeBindAddress: ":8001",
	}

	cmd := &cobra.Command{
		Use:   "managed-apiserver-proxy",
		Short: "Relay raw TCP connections to the managed cluster apiserver",
		RunE: func(cmd *cobra.Command, args []string) error {
			return proxy.Run(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&proxy.ManagedKubeconfig, "managed-kubeconfig", proxy.ManagedKubeconfig, "The managed cluster kubeconfig")
	flags.StringVar(&proxy.Listen, "listen", proxy.Listen, "The TCP listen address")
	flags.DurationVar(&proxy.DialTimeout, "dial-timeout", proxy.DialTimeout, "Timeout for dialing the managed apiserver")
	flags.StringVar(&proxy.HealthProbeBindAddress, "health-probe-bind-address", proxy.HealthProbeBindAddress, "The address the health probe and metrics endpoint binds to")

	return cmd
}

func (p *Proxy) Run(ctx context.Context) error {
	if p.ManagedKubeconfig == "" {
		return fmt.Errorf("managed kubeconfig is required")
	}
	if p.Listen == "" {
		return fmt.Errorf("listen address is required")
	}
	if p.HealthProbeBindAddress == "" {
		p.HealthProbeBindAddress = ":8001"
	}

	config, err := clientcmd.BuildConfigFromFlags("", p.ManagedKubeconfig)
	if err != nil {
		return err
	}
	target, err := targetAddress(config.Host)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", p.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	go func() {
		if err := utils.ServeHealthProbes(p.HealthProbeBindAddress, nil); err != nil {
			klog.Fatal(err)
		}
	}()

	klog.Infof("managed apiserver proxy listening on %s and relaying to %s", p.Listen, target)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		addonmetrics.ObserveManagedAPIServerRelayConnectionStart()
		go p.handle(ctx, conn, target)
	}
}

func (p *Proxy) handle(ctx context.Context, downstream net.Conn, target string) {
	defer addonmetrics.ObserveManagedAPIServerRelayConnectionDone()
	defer downstream.Close()

	dialer := &net.Dialer{
		Timeout:   p.DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	upstream, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		addonmetrics.ObserveManagedAPIServerRelayDialError()
		klog.Errorf("failed to dial managed apiserver %s: %v", target, err)
		return
	}
	defer upstream.Close()

	errCh := make(chan error, 2)
	go copyAndClose(upstream, downstream, errCh)
	go copyAndClose(downstream, upstream, errCh)
	<-errCh
}

func copyAndClose(dst net.Conn, src net.Conn, errCh chan<- error) {
	_, err := io.Copy(dst, src)
	if tcp, ok := dst.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	errCh <- err
}

func targetAddress(host string) (string, error) {
	if host == "" {
		return "", fmt.Errorf("managed kubeconfig server is empty")
	}
	parsed, err := url.Parse(host)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("managed kubeconfig server %q does not include a host", host)
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return "", fmt.Errorf("managed kubeconfig server %q does not include a host", host)
	}
	if port := parsed.Port(); port != "" {
		return net.JoinHostPort(hostname, port), nil
	}
	switch parsed.Scheme {
	case "https":
		return net.JoinHostPort(hostname, "443"), nil
	case "http":
		return net.JoinHostPort(hostname, "80"), nil
	default:
		return "", fmt.Errorf("unsupported managed kubeconfig server scheme %q", parsed.Scheme)
	}
}

package metrics

import (
	"time"

	componentmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	managedKubeconfigTokenExpirationSeconds = componentmetrics.NewGauge(
		&componentmetrics.GaugeOpts{
			Name:           "cluster_proxy_managed_kubeconfig_token_expiration_seconds",
			Help:           "Seconds until the generated hosted-mode managed kubeconfig token expires.",
			StabilityLevel: componentmetrics.ALPHA,
		},
	)

	managedKubeconfigRefreshTotal = componentmetrics.NewCounterVec(
		&componentmetrics.CounterOpts{
			Name:           "cluster_proxy_managed_kubeconfig_refresh_total",
			Help:           "Total number of managed kubeconfig refresh attempts by result.",
			StabilityLevel: componentmetrics.ALPHA,
		},
		[]string{"result"},
	)

	managedAPIServerRelayConnectionsTotal = componentmetrics.NewCounter(
		&componentmetrics.CounterOpts{
			Name:           "cluster_proxy_managed_apiserver_relay_connections_total",
			Help:           "Total number of raw TCP connections accepted by the managed apiserver relay.",
			StabilityLevel: componentmetrics.ALPHA,
		},
	)

	managedAPIServerRelayConnectionsActive = componentmetrics.NewGauge(
		&componentmetrics.GaugeOpts{
			Name:           "cluster_proxy_managed_apiserver_relay_connections_active",
			Help:           "Current number of active raw TCP connections handled by the managed apiserver relay.",
			StabilityLevel: componentmetrics.ALPHA,
		},
	)

	managedAPIServerRelayDialErrorsTotal = componentmetrics.NewCounter(
		&componentmetrics.CounterOpts{
			Name:           "cluster_proxy_managed_apiserver_relay_dial_errors_total",
			Help:           "Total number of managed apiserver relay dial errors.",
			StabilityLevel: componentmetrics.ALPHA,
		},
	)

	serviceProxyRequestsTotal = componentmetrics.NewCounterVec(
		&componentmetrics.CounterOpts{
			Name:           "cluster_proxy_service_proxy_requests_total",
			Help:           "Total number of service-proxy requests by mode, target, and result.",
			StabilityLevel: componentmetrics.ALPHA,
		},
		[]string{"mode", "target", "result"},
	)

	serviceRelayRequestsTotal = componentmetrics.NewCounterVec(
		&componentmetrics.CounterOpts{
			Name:           "cluster_proxy_service_relay_requests_total",
			Help:           "Total number of service-relay requests by target scheme and result.",
			StabilityLevel: componentmetrics.ALPHA,
		},
		[]string{"scheme", "result"},
	)
)

func init() {
	legacyregistry.MustRegister(
		managedKubeconfigTokenExpirationSeconds,
		managedKubeconfigRefreshTotal,
		managedAPIServerRelayConnectionsTotal,
		managedAPIServerRelayConnectionsActive,
		managedAPIServerRelayDialErrorsTotal,
		serviceProxyRequestsTotal,
		serviceRelayRequestsTotal,
	)
}

func SetManagedKubeconfigTokenExpiration(expiration, now time.Time) {
	remaining := expiration.Sub(now).Seconds()
	if remaining < 0 {
		remaining = 0
	}
	managedKubeconfigTokenExpirationSeconds.Set(remaining)
}

func ObserveManagedKubeconfigRefresh(result string) {
	managedKubeconfigRefreshTotal.WithLabelValues(result).Inc()
}

func ObserveManagedAPIServerRelayConnectionStart() {
	managedAPIServerRelayConnectionsTotal.Inc()
	managedAPIServerRelayConnectionsActive.Inc()
}

func ObserveManagedAPIServerRelayConnectionDone() {
	managedAPIServerRelayConnectionsActive.Dec()
}

func ObserveManagedAPIServerRelayDialError() {
	managedAPIServerRelayDialErrorsTotal.Inc()
}

func ObserveServiceProxyRequest(mode, target, result string) {
	serviceProxyRequestsTotal.WithLabelValues(mode, target, result).Inc()
}

func ObserveServiceRelayRequest(scheme, result string) {
	serviceRelayRequestsTotal.WithLabelValues(scheme, result).Inc()
}

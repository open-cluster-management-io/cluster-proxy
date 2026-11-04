package util

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestLoadBalancerIngressAddress(t *testing.T) {
	testcases := []struct {
		name    string
		ingress []corev1.LoadBalancerIngress
		want    string
	}{
		{
			name: "not provisioned",
			want: "",
		},
		{
			name:    "ip",
			ingress: []corev1.LoadBalancerIngress{{IP: "1.2.3.4"}},
			want:    "1.2.3.4",
		},
		{
			name:    "hostname",
			ingress: []corev1.LoadBalancerIngress{{Hostname: "lb.example.com"}},
			want:    "lb.example.com",
		},
		{
			name:    "ip is preferred over hostname",
			ingress: []corev1.LoadBalancerIngress{{IP: "1.2.3.4", Hostname: "lb.example.com"}},
			want:    "1.2.3.4",
		},
		{
			name:    "empty entries are skipped",
			ingress: []corev1.LoadBalancerIngress{{}, {Hostname: "lb.example.com"}},
			want:    "lb.example.com",
		},
		{
			name:    "only empty entries",
			ingress: []corev1.LoadBalancerIngress{{}},
			want:    "",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &corev1.Service{}
			svc.Status.LoadBalancer.Ingress = tc.ingress
			if got := LoadBalancerIngressAddress(svc); got != tc.want {
				t.Errorf("LoadBalancerIngressAddress() = %q, want %q", got, tc.want)
			}
		})
	}
}

# Cluster Proxy

[![License](https://img.shields.io/:license-apache-blue.svg)](http://www.apache.org/licenses/LICENSE-2.0.html)


## What is Cluster Proxy?

Cluster Proxy is a pluggable addon working on OCM rebased on the extensibility
provided by [addon-framework](https://github.com/open-cluster-management-io/addon-framework) 
which automates the installation of [apiserver-network-proxy](https://github.com/kubernetes-sigs/apiserver-network-proxy)
on both hub cluster and managed clusters. The network proxy will be establishing
reverse proxy tunnels from the managed cluster to the hub cluster to make the 
clients from the hub network can access the services in the managed clusters'
network even if all the clusters are isolated in different VPCs.

Cluster Proxy consists of two components:

- __Addon-Manager__: Manages the installation of proxy-servers i.e. proxy ingress
  in the hub cluster.
  
- __Addon-Agent__: Manages the installation of proxy-agents for each managed 
  clusters.

The overall architecture is shown below:

![Arch](./hack/picture/arch.png)


## Getting started

### Prerequisite

- OCM registration (>= 0.5.0)

### Steps

#### Installing via Helm Chart

1. Adding helm repo:

```shell
$ helm repo add ocm https://open-cluster-management-helm-charts.oss-cn-beijing.aliyuncs.com/releases/
$ helm repo update
$ helm search repo ocm/cluster-proxy
NAME                       	CHART VERSION	APP VERSION	DESCRIPTION                   
ocm/cluster-proxy          	<..>       	    1.0.0      	A Helm chart for Cluster-Proxy
```

2. Install the helm chart:

```shell
$ helm install \
    -n open-cluster-management-cluster-proxy --create-namespace \
    cluster-proxy ocm/cluster-proxy 
$ kubectl -n open-cluster-management-cluster-proxy get pod
NAME                                           READY   STATUS        RESTARTS   AGE
cluster-proxy-5d8db7ddf4-265tm                 1/1     Running       0          12s
cluster-proxy-addon-manager-778f6d679f-9pndv   1/1     Running       0          33s
...
```

3. Install proxy agents for a specific managed cluster:

```shell
$ kubectl create -f - <<EOF
apiVersion: addon.open-cluster-management.io/v1alpha1
kind: ManagedClusterAddOn
metadata:
  name: cluster-proxy
  namespace: <your cluster name>
spec:
  installNamespace: "open-cluster-management-cluster-proxy"
EOF

$ kubectl get managedclusteraddon -A
NAMESPACE         NAME                     AVAILABLE   DEGRADED   PROGRESSING
<your cluster>    cluster-proxy            True                   
```

### Usage

By default, the proxy servers are running in GPRC mode so the proxy clients 
are expected to proxy through the tunnels by the [konnectivity-client](https://github.com/kubernetes-sigs/apiserver-network-proxy#clients).
Konnectivity is the underlying technique of Kubernetes' [egress-selector](https://kubernetes.io/docs/tasks/extend-kubernetes/setup-konnectivity/)
feature and an example of konnectivity client is visible [here](https://github.com/open-cluster-management-io/cluster-proxy/tree/main/examples/test-client).

Codewisely proxying to the managed cluster will be simply overriding the 
dialer of the kubernetes original client config object, e.g.:

```go
    // instantiate a gprc proxy dialer
    tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
        context.TODO(),
        <proxy service>,
        grpc.WithTransportCredentials(grpccredentials.NewTLS(proxyTLSCfg)),
    )
    cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
    if err != nil {
        panic(err)
    }
    // Override the default tcp dialer
    cfg.Dial = tunnel.DialContext 
```

## References

- Design: [https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/14-addon-cluster-proxy](https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/14-addon-cluster-proxy)
- Addon-Framework: [https://github.com/open-cluster-management-io/addon-framework](https://github.com/open-cluster-management-io/addon-framework)

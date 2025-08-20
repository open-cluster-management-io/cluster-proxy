# Cluster Proxy

[![License](https://img.shields.io/:license-apache-blue.svg)](http://www.apache.org/licenses/LICENSE-2.0.html)
[![Go](https://github.com/open-cluster-management-io/cluster-proxy/actions/workflows/go-presubmit.yml/badge.svg)](https://github.com/open-cluster-management-io/cluster-proxy/actions/workflows/go-presubmit.yml)


## What is Cluster Proxy?

Cluster Proxy is a pluggable addon working on OCM, based on the extensibility
provided by [addon-framework](https://github.com/open-cluster-management-io/addon-framework), 
which automates the installation of [apiserver-network-proxy](https://github.com/kubernetes-sigs/apiserver-network-proxy)
on both hub cluster and managed clusters. The network proxy establishes
reverse proxy tunnels from the managed cluster to the hub cluster to enable
clients from the hub network to access services in the managed clusters'
network even when all clusters are isolated in different VPCs.

Cluster Proxy consists of two components:

- __Addon-Manager__: Manages the installation of proxy-servers (i.e., proxy ingress)
  in the hub cluster.
  
- __Addon-Agent__: Manages the installation of proxy-agents for each managed 
  cluster.

The overall architecture is shown below:

![Arch](./hack/picture/arch.png)


## Getting started

### Prerequisite

- OCM registration (>= 0.5.0)

### Steps

#### Installing via Helm Chart

1. Adding helm repo:

```shell
$ helm repo add ocm https://openclustermanagement.blob.core.windows.net/releases/
$ helm repo update
$ helm search repo ocm/cluster-proxy
NAME                       	CHART VERSION	APP VERSION	DESCRIPTION                   
ocm/cluster-proxy          	<..>       	    1.0.0      	A Helm chart for Cluster-Proxy
```

2. Install the helm chart:

```shell
$ helm install \
    -n open-cluster-management-addon --create-namespace \
    cluster-proxy ocm/cluster-proxy 
$ kubectl -n open-cluster-management-cluster-proxy get pod
NAME                                           READY   STATUS        RESTARTS   AGE
cluster-proxy-5d8db7ddf4-265tm                 1/1     Running       0          12s
cluster-proxy-addon-manager-778f6d679f-9pndv   1/1     Running       0          33s
...
```

3. The addon will be automatically installed to your registered clusters, 
   verify the addon installation:

```shell
$ kubectl get managedclusteraddon -A | grep cluster-proxy
NAMESPACE         NAME                     AVAILABLE   DEGRADED   PROGRESSING
<your cluster>    cluster-proxy            True                   
```

### Usage

By default, the proxy servers are running in gRPC mode so the proxy clients 
are expected to proxy through the tunnels by the [konnectivity-client](https://github.com/kubernetes-sigs/apiserver-network-proxy#clients).
Konnectivity is the underlying technique of Kubernetes' [egress-selector](https://kubernetes.io/docs/tasks/extend-kubernetes/setup-konnectivity/)
feature and an example of konnectivity client is visible [here](https://github.com/open-cluster-management-io/cluster-proxy/tree/main/examples/test-client).

Code-wise, proxying to the managed cluster can be accomplished by simply overriding the 
dialer of the Kubernetes original client config object, e.g.:

```go
  // instantiate a gRPC proxy dialer
  tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
      context.TODO(),
      <proxy service>,
      grpc.WithTransportCredentials(grpccredentials.NewTLS(proxyTLSCfg)),
  )
  cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
  if err != nil {
  	return err
  }
  // The managed cluster's name.
  cfg.Host = clusterName
  // Override the default tcp dialer
  cfg.Dial = tunnel.DialContext 
```

### Performance

Here's the result of network bandwidth benchmarking via [goben](https://github.com/udhos/goben)
with and without Cluster-Proxy (i.e., Apiserver-Network-Proxy). The results show that proxying
through the tunnel involves approximately 50% performance loss, so it's recommended to avoid 
transferring data-intensive traffic over the proxy.


|  Bandwidth  |   Direct   | over Cluster-Proxy |
|-------------|------------|--------------------|
|  Read/Mbps  |  902 Mbps  |     461 Mbps       |
|  Write/Mbps |  889 Mbps  |     428 Mbps       |



## References

- Design: [https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/14-addon-cluster-proxy](https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/14-addon-cluster-proxy)
- Addon-Framework: [https://github.com/open-cluster-management-io/addon-framework](https://github.com/open-cluster-management-io/addon-framework)

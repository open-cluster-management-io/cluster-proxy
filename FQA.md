# FQA

<a name="custom-proxy-server-hostname"></a>

## 1. What is drawback of "PortForward" mode proxy ingress?

The "PortForward" will be starting a local proxy server in the addon agent
which is proxying the tunnel handshake and data via a port-forward long
connection. E.g. for a 3-replicas proxy server and 3-replicas proxy agent
environment, each agent will be maintaining 3 port-forward connection so
in all there's will be 3x3=9 long connections from each managed clusters.
The kube-apiserver has a limit in HTTP2 max concurrent streams prescribed
by `--http2-max-streams-per-connection` flag which is defaulted to 1000.
So under "PortForward" mode we need to take care of the number of inflight
long-running streams when we're managing more and more clusters.

## 2. What if my hub cluster doesn't support "LoadBalancer" type service?

By default, the cluster-proxy addon-manager will be automatically provisioning
a "LoadBalancer" typed service which is for listening tunnel handshakes from the
managed clusters. As a workaround, we can explicitly set an address url for the
proxy agents so that they know where to initiate the registration upon starting:

- For helm chart installation, adding a `--set-string 
  proxyServer.entrypointAddress=<the proxy server external hostname>` flag to
  prescribe the registration entry for the proxy agents.
- For opt-in to custom proxy server hostname for existing installation, editing
  the "ManagedProxyConfiguration" custom resource by:
  
> $ kubectl edit managedproxyconfiguration cluster-proxy

Then replace the entrypoint type from "LoadBalancerService" to "Hostname":

```yaml
spec:
  ...
  proxyServer:
    entrypoint:
      hostname:
        value: <your custom external hostname>
      type: Hostname
```

Note that the custom hostname will be automatically signed into proxy servers'
server-side X509 certificate upon changes and the hostname address shall be 
__accessible__ from each of the managed clusters.
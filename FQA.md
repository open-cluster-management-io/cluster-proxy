# FQA

<a name="custom-proxy-server-hostname"></a>

## 1. What if my hub cluster doesn't support "LoadBalancer" type service?

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
# Cluster Proxy Helm Chart

This Helm chart installs the Cluster Proxy addon for Open Cluster Management (OCM), which enables accessing services in isolated managed clusters through reverse proxy tunnels.

## Prerequisites

- Kubernetes cluster
- Helm 3.x
- Open Cluster Management (OCM) installed

## Installation

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace
```

## Configuration

### Values

| Parameter                               | Description                        | Default                                         |
| --------------------------------------- | ---------------------------------- | ----------------------------------------------- |
| `registry`                              | Image registry                     | `quay.io/open-cluster-management`               |
| `image`                                 | Image name                         | `cluster-proxy`                                 |
| `tag`                                   | Image tag                          | Chart version                                   |
| `replicas`                              | Number of replicas                 | `1`                                             |
| `spokeAddonNamespace`                   | Namespace for spoke addon          | `open-cluster-management-cluster-proxy`         |
| `proxyServerImage`                      | Proxy server image                 | `quay.io/open-cluster-management/cluster-proxy` |
| `proxyAgentImage`                       | Proxy agent image                  | `quay.io/open-cluster-management/cluster-proxy` |
| `proxyServer.entrypointLoadBalancer`    | Enable LoadBalancer for entrypoint | `false`                                         |
| `proxyServer.entrypointAddress`         | Custom entrypoint address          | `""`                                            |
| `proxyServer.port`                      | Proxy server port                  | `8091`                                          |
| `installByPlacement.placementName`      | Placement name for installation    | `""`                                            |
| `installByPlacement.placementNamespace` | Placement namespace                | `""`                                            |
| `enableServiceProxy`                    | Enable user server deployment      | `false`                                         |

### User Server Configuration

The user server provides an API endpoint for managing cluster proxy connections. To enable it:

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --set enableServiceProxy=true
```

#### Important Prerequisites for User Server

**Before enabling the user server, you MUST create the following secret in the installation namespace:**

**cluster-proxy-user-serving-cert** - TLS certificate for the user server

```yaml
apiVersion: v1
kind: Secret
type: kubernetes.io/tls
metadata:
  name: cluster-proxy-user-serving-cert
  namespace: <release-namespace>
data:
  tls.crt: <base64-encoded-certificate>
  tls.key: <base64-encoded-private-key>
```

**Automatically Created Secrets:**

The following secrets will be automatically created by the controller and do NOT need to be created manually:

- **proxy-server-ca** - CA certificate for the proxy server
- **proxy-client** - Client certificate for proxy authentication

**⚠️ Warning:** If the `cluster-proxy-user-serving-cert` secret is not present before installation, the user-server deployment will remain in **Pending** state and pods will fail to start.

To verify the secret is created:

```bash
kubectl get secret -n <release-namespace> cluster-proxy-user-serving-cert
```

## Examples

### Basic Installation

```bash
helm install cluster-proxy ./charts/cluster-proxy
```

### With User Server Enabled

```bash
# First, create the required secret
kubectl create secret tls cluster-proxy-user-serving-cert \
  --cert=path/to/tls.crt \
  --key=path/to/tls.key \
  -n open-cluster-management-addon

# Then install with user server enabled
# Note: proxy-server-ca and proxy-client secrets will be created automatically by the controller
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --set enableServiceProxy=true
```

### Custom Image and Replicas

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --set image=my-custom-proxy \
  --set tag=v1.0.0 \
  --set replicas=3
```

## Upgrading

```bash
helm upgrade cluster-proxy ./charts/cluster-proxy
```

## Uninstallation

```bash
helm uninstall cluster-proxy
```

## Troubleshooting

### User Server Pods Stuck in Pending

**Symptom:** After enabling `enableServiceProxy=true`, the deployment pods remain in Pending state.

**Solution:** Verify that the required secret exists in the namespace:

```bash
kubectl get secret -n <namespace> cluster-proxy-user-serving-cert
```

If the secret is missing, create it:

```bash
kubectl create secret tls cluster-proxy-user-serving-cert \
  --cert=path/to/tls.crt \
  --key=path/to/tls.key \
  -n <namespace>
```

Note: The `proxy-server-ca` and `proxy-client` secrets are created automatically by the controller and do not need manual creation.

### ImagePullBackOff Errors

**Solution:** Verify the image registry and credentials:

```bash
helm upgrade cluster-proxy ./charts/cluster-proxy \
  --set registry=<your-registry> \
  --set image=<your-image> \
  --set tag=<your-tag>
```

## More Information

For more details about the Cluster Proxy project, visit the [GitHub repository](https://github.com/open-cluster-management-io/cluster-proxy).

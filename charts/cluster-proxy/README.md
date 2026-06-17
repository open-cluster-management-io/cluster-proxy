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
| `userServer.enabled`                    | Auto-manage user-server cert       | `false`                                         |
| `userServer.additionalSANs`             | Extra SANs for the generated cert  | `[]`                                            |

### User Server Configuration

The user server provides an API endpoint for managing cluster proxy connections. To enable it:

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --set enableServiceProxy=true
```

#### User Server Serving Certificate

The user-server deployment mounts a TLS serving certificate from the `cluster-proxy-user-serving-cert` secret in the installation namespace. You can provision this secret in one of two ways.

**Option 1 (recommended): let the controller generate and rotate it**

Set `userServer.enabled=true` so the `ManagedProxyConfiguration` requests a user-server certificate. The controller then generates the `cluster-proxy-user-serving-cert` secret in the installation namespace and rotates it automatically, so no manual secret creation is required:

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --set enableServiceProxy=true \
  --set userServer.enabled=true
```

Add extra hostnames or IPs to the generated certificate with `userServer.additionalSANs`:

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --set enableServiceProxy=true \
  --set userServer.enabled=true \
  --set userServer.additionalSANs[0]=user-server.example.com
```

**Option 2: provide the certificate yourself**

If you leave `userServer.enabled=false`, you MUST create the `cluster-proxy-user-serving-cert` secret in the installation namespace before the user-server pods can start:

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

The following secrets are always created automatically by the controller and do NOT need to be created manually:

- **proxy-server-ca** - CA certificate for the proxy server
- **proxy-client** - Client certificate for proxy authentication

**⚠️ Warning:** When `userServer.enabled=false` and the `cluster-proxy-user-serving-cert` secret is not present, the user-server deployment will remain in **Pending** state and pods will fail to start. Either set `userServer.enabled=true` or create the secret manually.

To verify the secret exists (whether controller-generated or manually created):

```bash
kubectl get secret -n <release-namespace> cluster-proxy-user-serving-cert
```

## Examples

### Basic Installation

```bash
helm install cluster-proxy ./charts/cluster-proxy
```

### With User Server Enabled (controller-managed certificate)

```bash
# The controller generates and rotates cluster-proxy-user-serving-cert automatically.
# proxy-server-ca and proxy-client secrets are also created automatically by the controller.
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --set enableServiceProxy=true \
  --set userServer.enabled=true
```

### With User Server Enabled (self-provided certificate)

```bash
# Leave userServer.enabled at its default (false) and create the secret yourself first
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

**Symptom:** After enabling `enableServiceProxy=true`, the deployment pods remain in Pending state because the `cluster-proxy-user-serving-cert` secret is missing.

**Solution:** Verify that the secret exists in the namespace:

```bash
kubectl get secret -n <namespace> cluster-proxy-user-serving-cert
```

If the secret is missing, either let the controller manage it by enabling automatic rotation:

```bash
helm upgrade cluster-proxy ./charts/cluster-proxy \
  --set enableServiceProxy=true \
  --set userServer.enabled=true
```

or create it manually:

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

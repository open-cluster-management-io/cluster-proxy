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

| Parameter                               | Description                                                       | Default                                         |
| --------------------------------------- | ----------------------------------------------------------------- | ----------------------------------------------- |
| `registry`                              | Registry used with `image` and `tag`                              | `quay.io/open-cluster-management`               |
| `image`                                 | Cluster-proxy image name                                          | `cluster-proxy`                                 |
| `tag`                                   | Cluster-proxy image tag                                           | `v<chart version>`                              |
| `replicas`                              | Replicas for hub deployments                                      | `1`                                             |
| `spokeAddonNamespace`                   | Default managed cluster addon namespace                           | `open-cluster-management-cluster-proxy`         |
| `proxyServerImage`                      | Default apiserver-network-proxy server image                      | `quay.io/open-cluster-management/cluster-proxy` |
| `proxyAgentImage`                       | Default apiserver-network-proxy agent image                       | `quay.io/open-cluster-management/cluster-proxy` |
| `proxyServer.entrypointLoadBalancer`    | Expose the proxy entrypoint with a LoadBalancer Service           | `false`                                         |
| `proxyServer.entrypointAddress`         | External proxy entrypoint hostname                                | `""`                                            |
| `proxyServer.port`                      | Proxy entrypoint port                                             | `8091`                                          |
| `proxyServer.imagePullPolicy`           | Proxy server and agent image pull policy                          | `IfNotPresent`                                  |
| `installByPlacement.placementName`      | Placement used to select managed clusters                         | `cluster-proxy-placement` when empty             |
| `installByPlacement.placementNamespace` | Namespace containing the Placement                               | Release namespace when empty                    |
| `enableKubeApiProxy`                    | Enable Kubernetes API proxy support                               | `true`                                          |
| `enableServiceProxy`                    | Deploy the hub user-server and managed cluster service-proxy      | `false`                                         |
| `enableImpersonation`                   | Grant hub permissions required for service-proxy impersonation    | `true`                                          |
| `featureGates.clusterProfile`           | Enable ClusterProfile integration                                | `false`                                         |
| `userServer.enabled`                    | Generate and rotate the user-server serving certificate          | `false`                                         |
| `userServer.additionalSANs`             | Extra SANs for the generated user-server certificate             | `[]`                                            |
| `networkPolicies.enabled`               | Create opt-in NetworkPolicies for hub and managed workloads       | `false`                                         |

### Service Proxy and User Server Configuration

The user-server accepts HTTP requests over HTTPS on the hub and sends them
through the cluster-proxy tunnel. The service-proxy sidecar on each managed
cluster authenticates Kubernetes API requests and forwards them to the target
Service. Enable both components with:

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace \
  --set enableServiceProxy=true \
  --set enableImpersonation=true \
  --set userServer.enabled=true
```

`enableImpersonation` defaults to true. Keep it enabled when using hub tokens
or external OIDC tokens. With impersonation enabled, managed-cluster-issued
tokens continue to use the managed cluster TokenReview path.

#### User Server Serving Certificate

The user-server deployment mounts a TLS serving certificate from the `cluster-proxy-user-serving-cert` secret in the installation namespace. You can provision this secret in one of two ways.

**Option 1 (recommended): let the controller generate and rotate it**

Set `userServer.enabled=true` so the `ManagedProxyConfiguration` requests a user-server certificate. The controller then generates the `cluster-proxy-user-serving-cert` secret in the installation namespace and rotates it automatically, so no manual secret creation is required:

```bash
helm upgrade --install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace \
  --set enableServiceProxy=true \
  --set userServer.enabled=true
```

Add extra hostnames or IPs to the generated certificate with `userServer.additionalSANs`:

```bash
helm upgrade --install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace \
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
  namespace: open-cluster-management-addon
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
kubectl get secret -n open-cluster-management-addon cluster-proxy-user-serving-cert
```

#### External OIDC Authentication

OIDC is configured per managed cluster through an AddOnDeploymentConfig, not
through top-level Helm values. First enable service-proxy and choose a
user-server certificate option:

```bash
helm upgrade --install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace \
  --set enableServiceProxy=true \
  --set enableImpersonation=true \
  --set userServer.enabled=true
```

Then follow the
[service-proxy authentication and impersonation guide](../../pkg/serviceproxy/readme.md#oidc-token-authentication)
to configure the issuer, optional private CA, ManagedClusterAddOn reference,
and managed cluster RBAC.

## Examples

### Basic Installation

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace
```

### With User Server Enabled (controller-managed certificate)

```bash
# The controller generates and rotates cluster-proxy-user-serving-cert automatically.
# proxy-server-ca and proxy-client secrets are also created automatically by the controller.
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace \
  --set enableServiceProxy=true \
  --set userServer.enabled=true
```

### With User Server Enabled (self-provided certificate)

```bash
# Leave userServer.enabled at its default (false) and create the secret yourself first
kubectl create namespace open-cluster-management-addon \
  --dry-run=client \
  --output yaml \
  | kubectl apply -f -

kubectl create secret tls cluster-proxy-user-serving-cert \
  --cert=path/to/tls.crt \
  --key=path/to/tls.key \
  -n open-cluster-management-addon

# Then install with user server enabled
# Note: proxy-server-ca and proxy-client secrets will be created automatically by the controller
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace \
  --set enableServiceProxy=true
```

### Custom Image and Replicas

```bash
helm install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --create-namespace \
  --set image=my-custom-proxy \
  --set tag=v1.0.0 \
  --set replicas=3
```

## Upgrading

```bash
helm upgrade cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon
```

## Uninstallation

```bash
helm uninstall cluster-proxy \
  --namespace open-cluster-management-addon
```

## Troubleshooting

### User Server Pods Stuck in Pending

**Symptom:** After enabling `enableServiceProxy=true`, the deployment pods remain in Pending state because the `cluster-proxy-user-serving-cert` secret is missing.

**Solution:** Verify that the secret exists in the namespace:

```bash
kubectl get secret -n open-cluster-management-addon cluster-proxy-user-serving-cert
```

If the secret is missing, either let the controller manage it by enabling automatic rotation:

```bash
helm upgrade cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --set enableServiceProxy=true \
  --set userServer.enabled=true
```

or create it manually:

```bash
kubectl create secret tls cluster-proxy-user-serving-cert \
  --cert=path/to/tls.crt \
  --key=path/to/tls.key \
  -n open-cluster-management-addon
```

Note: The `proxy-server-ca` and `proxy-client` secrets are created automatically by the controller and do not need manual creation.

### ImagePullBackOff Errors

**Solution:** Verify the image registry and credentials:

```bash
helm upgrade cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --set registry=registry.example.com/team \
  --set image=cluster-proxy \
  --set tag=v1.0.0
```

## More Information

For more details about the Cluster Proxy project, visit the [GitHub repository](https://github.com/open-cluster-management-io/cluster-proxy).

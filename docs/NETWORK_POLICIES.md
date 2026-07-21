# Network Policies

Cluster Proxy can apply allow-list [NetworkPolicies](https://kubernetes.io/docs/concepts/services-networking/network-policies/) for its hub and managed workloads. The feature is **opt-in** and defaults to off so existing installs are unchanged.

## Toggle

| Surface | Field | What it controls |
|---------|-------|------------------|
| Helm chart `charts/cluster-proxy` | `networkPolicies.enabled` | Renders NetworkPolicies for **addon-manager** and **user-server** (when `enableServiceProxy=true`); writes `ManagedProxyConfiguration.spec.networkPolicies.enabled` |
| `ManagedProxyConfiguration` | `spec.networkPolicies.enabled` | Controller applies/removes NetworkPolicy for **proxy-server**; addon chart applies/removes NetworkPolicy for **proxy-agent** on managed clusters |

When `enabled` is `false` or unset, **managed** NetworkPolicies are removed (or never created):

- **proxy-server** — deleted by the controller
- **proxy-agent** — dropped from the addon chart / ManifestWork on the next reconcile
- **addon-manager** / **user-server** — removed on Helm upgrade when the chart no longer renders them

Policies applied **manually** (for example `kubectl apply` of the manager/user templates) are **not** removed by disabling the feature; delete those explicitly.

## Policies

Peers are **port-based** (empty `from` / `to`) so the same manifests work across Kubernetes distributions. That matches the OCM registration-operator NetworkPolicy style.

### Hub (install namespace, typically `open-cluster-management-addon`)

| Workload | Selector | Ingress | Egress |
|----------|----------|---------|--------|
| ANP proxy-server | `proxy.open-cluster-management.io/component-name=proxy-server` | TCP **8090**, **8091** | DNS 53/5353; API **443**/**6443** |
| addon-manager | `component=cluster-proxy-manager` | none (deny all) | DNS; API **443**/**6443** |
| user-server | `component=cluster-proxy-addon-user` | TCP **9092**, **8090** | DNS; API **443**/**6443**; ANP **8090** |

External clients typically reach an ingress endpoint on **:443**, which then connects to backends **8091** (ANP) / **9092** (user). The NetworkPolicy allows those backend ports without hard-coding ingress peer selectors.

### Managed (`spokeAddonNamespace`)

| Workload | Selector | Ingress | Egress |
|----------|----------|---------|--------|
| proxy-agent | `proxy.open-cluster-management.io/component-name=proxy-agent` | TCP **8000**, **7443** (service-proxy health / TLS) | DNS; hub/spoke API **443**/**6443**; ANP entrypoint (`.Values.serviceEntryPointPort`, default **8091**) |

## Ports: hard-coded vs configurable

Most ports in these NetworkPolicies match values that the workloads themselves hard-code today. That is intentional for this opt-in allow-list.

| Port(s) | Used by | Configurable? |
|---------|---------|---------------|
| **8090**, **8091** | Hub ANP Service / proxy-server NP | CRD documents `spec.deploy.ports` (defaults 8090/8091), but the controller currently **hard-codes** those ports on the Service. Policies match that deployed behavior. |
| **entrypoint.port** (default **8091**) | Agent → hub ANP (`--proxy-server-port`) | **Yes** — from `ManagedProxyConfiguration.spec.proxyServer.entrypoint.port`, passed as `.Values.serviceEntryPointPort`. The agent NetworkPolicy templates this value. |
| **9092**, **8090** | user-server serve / ANP client | Hard-coded in the hub chart Deployment/Service args |
| **8000**, **7443** | service-proxy health / TLS | Hard-coded in the addon-agent chart |
| **53**, **5353**, **443**, **6443** | DNS / Kubernetes API | Platform conventions (same style as OCM NetworkPolicies) |

Hard-coding is OK while the binaries and charts use fixed ports. If `spec.deploy.ports` or chart values become live knobs later, the NetworkPolicies should follow those values.

## `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`

On managed clusters, `AddOnDeploymentConfig.spec.proxyConfig` can set `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` on the **proxy-agent** container (via chart `proxyConfig`). Those URLs are cluster-specific (host and port are not fixed).

The managed agent NetworkPolicy does **not** parse proxy env vars. If a forward proxy is required and its TCP port is not already allowed (443, 6443, or `serviceEntryPointPort`), egress to that proxy is blocked when NetworkPolicies are enabled.

### Extending the agent policy for a forward proxy

Kubernetes unions allow rules across NetworkPolicies that select the same pod. Leave the managed policy in place and apply a second policy in the agent namespace (same selectors):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cluster-proxy-proxy-agent-http-proxy
  namespace: open-cluster-management-cluster-proxy  # agent install namespace
spec:
  podSelector:
    matchLabels:
      open-cluster-management.io/addon: cluster-proxy
      proxy.open-cluster-management.io/component-name: proxy-agent
  policyTypes:
  - Egress
  egress:
  - ports:
    - protocol: TCP
      port: 3128  # port from HTTP_PROXY / HTTPS_PROXY URL
    # Optional: restrict to the proxy peer
    # to:
    # - ipBlock:
    #     cidr: 10.0.0.10/32
```

Steps:

1. Read the proxy URL from AddOnDeploymentConfig (`spec.proxyConfig.httpProxy` / `httpsProxy`).
2. Allow that host’s TCP port (often 3128, 8080, or 8888 — not fixed).
3. Prefer a `to:` peer (`ipBlock` or namespace/pod selectors) when known; port-only empty `to` works but is broader.
4. Keep hub/API/entrypoint allows if `NO_PROXY` still sends some traffic directly.

This companion policy is **manual**: disabling `networkPolicies.enabled` does not delete it. The hub chart does not set these proxy env vars on addon-manager or user-server.

## Enable

### Helm

```bash
helm upgrade --install cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --set networkPolicies.enabled=true
```

### Patch an existing ManagedProxyConfiguration

```bash
kubectl patch managedproxyconfiguration cluster-proxy --type=merge -p \
  '{"spec":{"networkPolicies":{"enabled":true}}}'
```

Also ensure the hub chart NetworkPolicies are present. Prefer Helm upgrade with `--set networkPolicies.enabled=true` so manager/user policies stay under release management. The ManagedProxyConfiguration patch alone updates **proxy-server** and **proxy-agent** policies.

If you apply the manager/user templates manually instead, those objects are outside Helm/controller ownership. Disabling the feature later will not delete them — remove them with `kubectl delete` (or equivalent) yourself.

### Disable

```bash
kubectl patch managedproxyconfiguration cluster-proxy --type=merge -p \
  '{"spec":{"networkPolicies":{"enabled":false}}}'
```

```bash
helm upgrade cluster-proxy ./charts/cluster-proxy \
  --namespace open-cluster-management-addon \
  --set networkPolicies.enabled=false
```

That removes managed policies only (controller + Helm/addon chart as above). Manually created hub NetworkPolicies must still be deleted explicitly.

## Backward compatibility

### Shipping with the feature disabled (default)

Adding `spec.networkPolicies` is backward compatible when left unset or `enabled: false`:

| Combination | Behavior |
|-------------|----------|
| No change to install/CR | No NetworkPolicies are created — same runtime behavior as before this feature |
| New CRD + old controller | Field is stored; ignored until the controller is upgraded |
| Old CRD + new controller | Setting `networkPolicies.enabled: true` may be pruned by the apiserver until the CRD is updated |
| Helm always renders `networkPolicies.enabled: true\|false` into the CR | Harmless with the new CRD; dropped with an old CRD until upgraded |

**Upgrade order:** install or update the **CRD before or with** the new controller/chart.

### Enabling is not a transparent upgrade

Setting `enabled: true` is a **deliberate behavior change**, not a no-op. NetworkPolicies start restricting traffic. Paths covered by the allow-lists above (ANP **8090**/**8091**, user-server **9092**, DNS, API **443**/**6443**) should keep working; traffic outside those rules can break.

Examples of what can fail after enable:

- Egress to non-API ports (custom sidecars, package pulls, other tooling)
- Forward proxy ports from `HTTP_PROXY` / `HTTPS_PROXY` (see above) unless you add a companion NetworkPolicy
- Ingress to **addon-manager** (deny-all) — e.g. metrics scrapers hitting the pod directly
- Clusters where the Kubernetes API is not on **443** or **6443**
- Custom overlay traffic not covered by these policies

Validate on a non-production hub and spoke before enabling broadly. Disable by setting `enabled: false` (managed policies are removed; delete any manually applied hub policies yourself) if something regresses.

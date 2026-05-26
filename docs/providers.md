# Providers

Three providers ship in the binary. Each has its own auth chain, minimum
permission set, and per-resource implementation status.

| Provider     | Phase | Implementation status                                  |
| ------------ | ----- | ------------------------------------------------------ |
| Cloudflare   | 2     | Zones + DNS records implemented; 11 resource types stubbed |
| OCI          | 3     | Compartments + regions + Compute + Load Balancers implemented; 15 resource types stubbed |
| Kubernetes   | 4     | **Universal** — dynamic-client + discovery lists every built-in resource type and every CRD with no per-resource code |

---

## Cloudflare

### Auth

API-token only. No legacy email + global API key path.

```bash
export CLOUDFLARE_API_TOKEN=cf-token-here
auditor audit --provider cloudflare
```

### Minimum token scopes

Create a custom token at https://dash.cloudflare.com/profile/api-tokens
with the following permissions (account-level):

| Permission          | Access |
| ------------------- | ------ |
| `Zone`              | Read   |
| `Zone.DNS`          | Read   |

Add additional `Read` permissions as more resource types come online
(R2, KV, Workers, etc. — see resource matrix below).

### Resource matrix

| Resource type                        | Asset type                  | Status   |
| ------------------------------------ | --------------------------- | -------- |
| Zones                                | `cloudflare.zone`           | shipped  |
| DNS records                          | `cloudflare.dns_record`     | shipped  |
| R2 buckets                           | `cloudflare.r2_bucket`      | stub     |
| KV namespaces                        | `cloudflare.kv_namespace`   | stub     |
| Workers scripts                      | `cloudflare.worker_script`  | stub     |
| Workers routes (per zone)            | `cloudflare.worker_route`   | stub     |
| Pages projects                       | `cloudflare.pages_project`  | stub     |
| D1 databases                         | `cloudflare.d1_database`    | stub     |
| Access apps                          | `cloudflare.access_app`     | stub     |
| Tunnels                              | `cloudflare.tunnel`         | stub     |
| Load Balancers (per zone)            | `cloudflare.load_balancer`  | stub     |
| Rulesets (account + per zone)        | `cloudflare.ruleset`        | stub     |
| Page Rules (per zone)                | `cloudflare.page_rule`      | stub     |
| Certificates                         | `cloudflare.certificate`    | stub     |

Stubs are wired into the collector orchestrator but emit zero assets
until the per-resource function is filled in
(`internal/providers/cloudflare/stubs.go`).

### SDK notes

Uses `github.com/cloudflare/cloudflare-go/v4` (the current generated
SDK). v2 — which the original plan specified — has been superseded.
The v4 API uses `cloudflare.F(value)` to wrap required parameters and
the `AutoPager` iterator pattern.

---

## OCI

### Auth chain

The provider tries each method in order; the first that yields a working
tenancy OCID wins.

1. **Instance principal** — only attempted when the IMDS endpoint
   (`http://169.254.169.254/opc/v2/instance/`) responds within 250 ms.
   Off-OCI machines (laptops, GitHub-runners) skip this without delay.
2. **Resource principal** — only attempted when the
   `OCI_RESOURCE_PRINCIPAL_VERSION` env var is set (OCI Functions, OKE
   workload identity).
3. **Config file** — `~/.oci/config`, profile from `--oci-profile` or
   `DEFAULT`.
4. **Env vars** — `OCI_TENANCY`, `OCI_USER`, `OCI_REGION`,
   `OCI_FINGERPRINT`, `OCI_KEY_PATH` (path to the private key PEM,
   not inline content).

### Minimum policy

Replace `Inventory-Auditors` with whatever group your auditor identity
belongs to:

```
Allow group Inventory-Auditors to inspect compartments in tenancy
Allow group Inventory-Auditors to read instance-family in tenancy
Allow group Inventory-Auditors to read load-balancers in tenancy
```

For the full provider (all 17 resources once implemented), the
read-everything shortcut is:

```
Allow group Inventory-Auditors to read all-resources in tenancy
```

### Regions

Default: home region of the configured tenancy. To scan all regions
the tenancy is subscribed to:

```bash
auditor audit --provider oci --oci-regions all
# or pick specific regions:
auditor audit --provider oci --oci-regions us-ashburn-1,us-phoenix-1
```

### Compartment recursion

This is the OCI gotcha most home-grown inventory tools miss. The
provider lists every accessible compartment in the tenancy tree via the
SDK's `CompartmentIdInSubtree=true` flag, then fans out per-compartment
collectors. The tenancy root itself is included as a synthetic
compartment so resources living at the root aren't skipped.

### Resource matrix

| Resource type            | Asset type                       | Status   |
| ------------------------ | -------------------------------- | -------- |
| Compartments             | `oci.compartment`                | shipped  |
| Compute instances        | `oci.compute.instance`           | shipped  |
| Classic Load Balancers   | `oci.load_balancer`              | shipped  |
| Block volumes            | `oci.block_volume`               | stub     |
| Boot volumes             | `oci.boot_volume`                | stub     |
| VCNs                     | `oci.vcn`                        | stub     |
| Subnets                  | `oci.subnet`                     | stub     |
| Object Storage buckets   | `oci.object_storage.bucket`      | stub     |
| Autonomous Databases     | `oci.autonomous_db`              | stub     |
| DB Systems               | `oci.db_system`                  | stub     |
| Functions                | `oci.function`                   | stub     |
| Container Instances      | `oci.container_instance`         | stub     |
| OKE clusters             | `oci.oke_cluster`                | stub     |
| Vaults                   | `oci.vault`                      | stub     |
| IAM Policies             | `oci.iam.policy`                 | stub     |
| IAM Users                | `oci.iam.user`                   | stub     |
| IAM Groups               | `oci.iam.group`                  | stub     |
| IAM Dynamic Groups       | `oci.iam.dynamic_group`          | stub     |

---

## Kubernetes

### Auth

1. **In-cluster** — when `KUBERNETES_SERVICE_HOST` is set (i.e., we're
   a pod). Uses the pod's mounted ServiceAccount token automatically.
2. **kubeconfig** — `$KUBECONFIG` env, then `~/.kube/config`. Pick a
   context with `--kube-context`.

### Minimum permissions

Because the provider uses dynamic discovery (`ServerPreferredResources`
→ `dynamicClient.Resource(gvr).List`), it can't enumerate a narrower
verb/resource matrix in advance — CRDs arrive after install and we'd
miss them. The read-only-everywhere ClusterRole that the Helm chart
provisions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cloud-asset-auditor
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["get", "list"]
  - nonResourceURLs: ["/healthz", "/version", "/api", "/apis", "/apis/*"]
    verbs: ["get"]
```

If your threat model requires a narrower role, bind the ServiceAccount
to whatever you allow — the provider tolerates `Forbidden` responses
per-GVR (warning, not failure) so a narrower role just produces a
smaller inventory.

### Namespace filtering

| Flag                                  | Behavior                                                              |
| ------------------------------------- | --------------------------------------------------------------------- |
| `--kube-namespace <ns>`               | Scope every namespaced list to one namespace                          |
| `--kube-exclude-namespaces a,b,c`     | Drop these namespaces from the cluster-wide list (default: `kube-system,kube-public,kube-node-lease`) |

### What gets listed

**Everything the API server reports as preferred via discovery, minus:**

- Subresources (`pods/status`, `deployments/scale`, etc.)
- Anything whose verb list doesn't include `list`
- Resources the ServiceAccount can't access (`Forbidden` → warning, not failure)

That includes every built-in resource (`v1.Pod`, `apps/v1.Deployment`,
`networking.k8s.io/v1.Ingress`, …) **and every CRD** in the cluster
(`example.com/v1.Widget`, `cert-manager.io/v1.Certificate`, …) with
**zero per-resource code**.

### Asset type format

`<group>/<version>.<kind>`. Core resources have an empty group, so the
format collapses to `<version>.<kind>`:

| Resource                        | `Asset.Type`                          |
| ------------------------------- | ------------------------------------- |
| Pod (core)                      | `v1.Pod`                              |
| Deployment (apps)               | `apps/v1.Deployment`                  |
| Ingress (networking)            | `networking.k8s.io/v1.Ingress`        |
| Hypothetical CRD                | `example.com/v1.Widget`               |

### Aggregated-API caveat

`ServerPreferredResources` can return a partial result with a
`*discovery.ErrGroupDiscoveryFailed` error when an aggregated API
server's backing service is down (e.g., a stale metrics-server). The
provider treats that as a warning and continues with whatever did
discover.

---

## Adding new providers

See [extending.md](./extending.md).

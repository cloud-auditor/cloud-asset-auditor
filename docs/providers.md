# Providers

Three providers ship in the binary. Each has its own auth chain, minimum
permission set, and per-resource implementation status.

| Provider     | Phase | Implementation status                                  |
| ------------ | ----- | ------------------------------------------------------ |
| Cloudflare   | 2     | **Complete** — accounts, zones, DNS, R2, KV, Workers, D1, Pages, Access, Tunnels, certificates, Rulesets, Page Rules, Load Balancers |
| OCI          | 3     | **Complete** — compartments + regions + all resource types implemented (compute, networking, storage, object storage, databases, functions, container instances, OKE, vaults, IAM) |
| Kubernetes   | 4     | **Universal** — dynamic-client + discovery lists every built-in resource type and every CRD with no per-resource code |

There is also a credential-free [**demo provider**](#demo-data) for evaluating
the tool and producing documentation.

---

## Demo data

`--demo` swaps the real providers for a built-in synthetic inventory of a
fictional company, **Northwind** — roughly 590 assets across Cloudflare, OCI,
Kubernetes (two clusters), GCP, Tailscale, and NetBird. It needs no
credentials, no network, and no config:

```bash
auditor serve --demo            # web UI at http://localhost:8080
auditor audit --demo -o json    # the whole inventory on stdout
auditor topology --demo -o html > topology.html
auditor reach --demo --exposed  # what a public DNS name can reach
```

`--demo` is a **persistent** flag, so it works on every subcommand, and it is
viper-bound: `AUDITOR_DEMO=1` or a `demo: true` config-file key do the same
thing.

### What it is for

Three things, in order of importance:

1. **Evaluation.** Someone assessing the tool can see a populated Assets table,
   Dashboard, and Topology graph before deciding whether to point it at a real
   tenancy.
2. **Documentation and screenshots.** Every screenshot in this repo can be
   regenerated from a fixture instead of redacted from a customer's inventory.
3. **Topology coverage.** The fixture is shaped backwards from the resolvers:
   it produces at least one edge of **every** `core.EdgeKind`, which no real
   test tenancy reliably does. `TestFixture_ExercisesEveryEdgeKind` asserts it.

### Behaviour worth knowing

- **Provider selection.** With `--demo` and no `--provider`, the run resolves
  to *just* `demo` — a demo must not quietly fire every real provider at your
  live credentials. An explicit `--provider` still wins, so
  `--demo --provider demo,kubernetes` scans your cluster alongside the fixture.
- **Registration.** The provider is registered only when `--demo` is passed, so
  `auditor providers` does not list `demo` on a normal run and fabricated
  assets can never appear in a real audit by accident.
- **Pacing.** A full run takes about 3 seconds, deliberately: the UI streams
  rows as they arrive and a demo that finished instantly would show none of
  that. `AUDITOR_DEMO_DURATION` overrides the budget — `AUDITOR_DEMO_DURATION=0`
  disables pacing entirely (what you want for scripted exports),
  `AUDITOR_DEMO_DURATION=20s` slows it down for a screen recording.
- **It reports errors on purpose.** The demo emits three non-fatal provider
  errors mid-stream so the partial-failure UI has something to render. That
  means `auditor audit --demo` **exits 2**, the normal partial-failure code.
  Not a bug — a demo where everything always succeeds hides a whole surface.
- **Determinism.** Fixed identifiers, a fixed epoch, no `time.Now`, no unseeded
  randomness. Two demo snapshots diff to nothing, so
  `auditor diff a.json b.json` is meaningful against demo data.

### What it does not model

The GCP forwarding rule publishes a public IP that two Cloudflare A records
point at, but no resolver indexes GCP addresses yet (`internal/topology/index.go`
handles Cloudflare, OCI LBs, the mesh providers, and Kubernetes payloads). The
fixture keeps the realistic data and the missing edge rather than papering over
the gap — if a GCP address resolver ever lands, the demo already exercises it.

Fixture source: [`internal/providers/demo/fixtures.go`](../internal/providers/demo/fixtures.go).
Changing an address or a label there can silently delete an edge, which is why
the edge-kind assertion exists.

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

| Permission                     | Access |
| ------------------------------ | ------ |
| `Account.Account Settings`     | Read   |
| `Zone`                         | Read   |
| `Zone.DNS`                     | Read   |
| `Account.Workers R2 Storage`   | Read   |
| `Account.Workers KV Storage`   | Read   |
| `Account.Workers Scripts`      | Read   |
| `Account.D1`                   | Read   |
| `Account.Cloudflare Pages`     | Read   |
| `Account.Access: Apps and Policies` | Read |
| `Account.Cloudflare Tunnel`    | Read   |
| `Account.mTLS Certificates`    | Read   |
| `Zone.SSL and Certificates`    | Read   |
| `Account.Account Rulesets` / `Zone.Zone WAF` | Read |
| `Zone.Page Rules`              | Read   |
| `Zone.Load Balancers`          | Read   |

A token missing a scope degrades gracefully: that collector reports one
error and every other resource type still lands (exit code 2 = partial).

### Why am I only getting DNS records?

The single most common Cloudflare surprise. A token created from the
**"Edit zone DNS"** preset (or any token scoped to just `Zone` + `Zone.DNS`)
produces *exactly* zones + DNS records and nothing else:

- **Account-scoped resources vanish silently.** With no `Account.Account
  Settings:Read` scope, `GET /accounts` returns success with an **empty** list
  — not a 403 — so R2, KV, Workers, D1, Pages, Access, Tunnels, mTLS
  certificates, and account rulesets all find zero accounts to enumerate and
  emit nothing. The auditor now flags this explicitly:

  ```
  cloudflare: the API token can see 0 accounts — all account-scoped resources
  (R2, KV, Workers, …) were skipped. This usually means the token is missing
  the 'Account.Account Settings:Read' scope …
  ```

- **Other zone-scoped resources 403.** Page Rules, Load Balancers, zone
  rulesets, and certificates return `403 Forbidden` (codes `10000` /
  `9109`); each error is annotated with a hint to add the matching Read scope.

The fix is to broaden the token to the scope table above (or use a token from
the **"Read all resources"** template). The token itself is valid — it's the
scopes that are missing.

### Resource matrix

| Resource type                        | Asset type                       | Scope          |
| ------------------------------------ | -------------------------------- | -------------- |
| Accounts                             | `cloudflare.account`             | token          |
| Zones                                | `cloudflare.zone`                | token          |
| DNS records                          | `cloudflare.dns_record`          | per zone       |
| R2 buckets                           | `cloudflare.r2_bucket`           | per account    |
| KV namespaces                        | `cloudflare.kv_namespace`        | per account    |
| Workers scripts                      | `cloudflare.worker_script`       | per account    |
| Pages projects                       | `cloudflare.pages_project`       | per account    |
| D1 databases                         | `cloudflare.d1_database`         | per account    |
| Access apps                          | `cloudflare.access_app`          | per account    |
| Tunnels (cloudflared)                | `cloudflare.tunnel`              | per account    |
| Certificate packs (edge)             | `cloudflare.certificate_pack`    | per zone       |
| Custom certificates                  | `cloudflare.custom_certificate`  | per zone       |
| mTLS certificates                    | `cloudflare.mtls_certificate`    | per account    |
| Rulesets (`scope` tag = account/zone)| `cloudflare.ruleset`             | account + zone |
| Page Rules                           | `cloudflare.page_rule`           | per zone       |
| Load Balancers                       | `cloudflare.load_balancer`       | per zone       |

Every resource type is implemented — there is no `stubs.go` anymore. Each
collector lives in its own file under `internal/providers/cloudflare/`
(e.g. `r2.go`, `rulesets.go`) following the `dns.go` template. Zone-scoped
assets carry `zone_id` / `zone_name` tags, which is what the topology
`wafBinding` resolver joins on.

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

Default: **every region the tenancy is subscribed to**. If the subscription
lookup fails (e.g. a missing identity permission), the audit falls back to the
tenancy's home region rather than aborting. To narrow the scan to specific
regions:

```bash
auditor audit --provider oci --oci-regions us-ashburn-1,us-phoenix-1
# "all" is the explicit form of the default:
auditor audit --provider oci --oci-regions all
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
| Block volumes            | `oci.block_volume`               | shipped  |
| Boot volumes             | `oci.boot_volume`                | shipped  |
| VCNs                     | `oci.vcn`                        | shipped  |
| Subnets                  | `oci.subnet`                     | shipped  |
| Object Storage buckets   | `oci.object_storage.bucket`      | shipped  |
| Autonomous Databases     | `oci.autonomous_database`        | shipped  |
| DB Systems               | `oci.db_system`                  | shipped  |
| Functions applications   | `oci.functions.application`      | shipped  |
| Functions                | `oci.functions.function`         | shipped  |
| Container Instances      | `oci.container_instance`         | shipped  |
| OKE clusters             | `oci.oke.cluster`                | shipped  |
| Vaults                   | `oci.vault`                      | shipped  |
| IAM Policies             | `oci.iam.policy`                 | shipped  |
| IAM Users                | `oci.iam.user`                   | shipped  |
| IAM Groups               | `oci.iam.group`                  | shipped  |
| IAM Dynamic Groups       | `oci.iam.dynamic_group`          | shipped  |

---

## Kubernetes

### Auth

1. **In-cluster** — when `KUBERNETES_SERVICE_HOST` is set (i.e., we're
   a pod). Uses the pod's mounted ServiceAccount token automatically.
2. **kubeconfig** — `$KUBECONFIG` env, then `~/.kube/config`. Pick a
   context with `--kube-context`.

### Scanning several clusters at once

`--kube-contexts` takes a comma-separated list of kubeconfig contexts (or the
literal `all` for every context) and inventories each in one audit, merging the
results. It overrides `--kube-context`. Each cluster is scanned in parallel via
its own client; per-cluster failures are non-fatal and tagged with the offending
context name (e.g. `context "prod-eu": ...`). Every asset keeps its origin
cluster in `account_id`, so you can split by it (`--sheet-by account` for XLSX,
or the Account facet in the web UI).

```bash
# Two named clusters
auditor audit --provider kubernetes --kube-contexts prod-us,prod-eu

# Every context in ~/.kube/config
auditor audit --provider kubernetes --kube-contexts all
```

In the web UI, the Assets tab shows a **Kube contexts** picker (populated from
`GET /api/v1/kube/contexts`) when the server's kubeconfig has more than one
context; leaving it unset uses the server's current-context. Running in-cluster
collapses to the single mounted ServiceAccount — there's nothing to choose.

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

## GCP

[Google Cloud](https://cloud.google.com) is inventoried via the **Cloud Asset
Inventory API** (`searchAllResources`): one call returns every resource type —
compute, storage, GKE, networking, IAM, … — across a project, folder, or
organization. There's no per-service matrix to maintain, the same universality
the Kubernetes provider gets from discovery.

### Auth

**Application Default Credentials** (ADC), the standard Google chain:
`GOOGLE_APPLICATION_CREDENTIALS` (a service-account key file) → `gcloud auth
application-default login` user credentials → GCE/GKE metadata server (workload
identity). There's no auth flag — whatever ADC resolves is used.

### Scope (what to inventory)

GCP is *enabled* by a scope resolved from the environment (the way Cloudflare is
enabled by its token):

- `GOOGLE_CLOUD_PROJECT=my-project` → scope `projects/my-project`
- `GCP_SCOPE=organizations/123456` (or `folders/789`) → org/folder-wide — every
  project underneath, in one pass

`--gcp-project` / `--gcp-scope` override the env scope (e.g. to widen a project
default to a whole organization). With no scope, GCP is skipped.

```bash
export GOOGLE_CLOUD_PROJECT=my-project
auditor audit --provider gcp -o json
# Whole organization in one audit:
auditor audit --provider gcp --gcp-scope organizations/123456 -o csv
```

### Required setup

1. Enable the API: `gcloud services enable cloudasset.googleapis.com`
2. Grant the caller `roles/cloudasset.viewer` on the project / folder / org.

### Quota project (user credentials only)

When authenticating with `gcloud` *user* ADC, Google APIs require a billing/quota
project or the call 403s with "API requires a quota project". The provider sets
it automatically from a `projects/<id>` scope. For a **folder/org** scope with
user credentials, set `GOOGLE_CLOUD_QUOTA_PROJECT=<project>` (or run `gcloud auth
application-default set-quota-project <project>`). Service-account credentials
carry their own project and need none of this.

### What you get

Each resource becomes one asset: the full resource name is the `id`, `assetType`
(e.g. `compute.googleapis.com/Instance`) the `type`, `location` the `region`,
`project` the `account_id`, `state` the `status`, and `labels` (plus network
tags, description, …) the tags. `--include-raw` attaches the full search result.

### SDK notes

No vendored SDK — a hand-rolled REST client (`client.go`) against
`cloudasset.googleapis.com`, with `golang.org/x/oauth2/google` for ADC token
sourcing. The google-cloud-go asset client pulls gRPC and a large dependency
tree for what over REST is a single paginated GET; the thin client keeps the
CGO-free binary lean.

---

## NetBird

[NetBird](https://netbird.io) is a WireGuard-based mesh VPN / zero-trust
network. The provider inventories a NetBird account through its REST
Management API.

### Auth

Token only — a **Personal Access Token** (prefixed `nbp_`), created under
*Settings → Personal Access Tokens* (or a service-user token) in the NetBird
dashboard. Export it as `NETBIRD_API_TOKEN`. It rides in the
`Authorization: Token <PAT>` header and is never logged. (The `Bearer <JWT>`
scheme NetBird also supports is for IdP-issued tokens, not PATs, and isn't
used here.)

```bash
export NETBIRD_API_TOKEN=nbp_xxxxxxxxxxxxxxxxxxxxxxxxxxxx
auditor audit --provider netbird -o json
```

### Self-hosted

By default the provider talks to the NetBird cloud API at
`https://api.netbird.io`. For a self-hosted instance, point it at your
Management service (same `/api` paths, same Token auth) via either:

```bash
export NETBIRD_MANAGEMENT_URL=https://netbird.example.com
# or, per-invocation:
auditor audit --provider netbird --netbird-management-url https://netbird.example.com
```

### Resource matrix

Each list endpoint returns a bare JSON array (no pagination); collectors fan
out under `--max-concurrency`. Cloud rate limit is 120 req/min.

| Asset type               | Endpoint                  | Notes |
| ------------------------ | ------------------------- | ----- |
| `netbird.peer`           | `GET /api/peers`          | Status = connected/disconnected; overlay `ip`/`ipv6`, public `connection_ip`, and `dns_label`/`hostname` are tagged (and fed to the topology graph) |
| `netbird.group`          | `GET /api/groups`         | |
| `netbird.policy`         | `GET /api/policies`       | Status = enabled/disabled; rule detail in `raw` |
| `netbird.route`          | `GET /api/routes`         | `network` CIDR + `domains` tagged; name = `network_id` (the HA route group) |
| `netbird.network`        | `GET /api/networks`       | The "Networks" feature (routers/resources/policies) |
| `netbird.nameserver`     | `GET /api/dns/nameservers`| Nameserver group → distribution peer groups + match domains |
| `netbird.setup_key`      | `GET /api/setup-keys`     | The secret key value is **never** mapped (omitted from the struct, so it can't reach `raw`); status is the lifecycle state |
| `netbird.user`           | `GET /api/users`          | The `password` field (create-only) is likewise never mapped |
| `netbird.posture_check`  | `GET /api/posture-checks` | Tags list which sub-checks are present |
| `netbird.account`        | `GET /api/accounts`       | Also resolves the account id stamped on every asset's `account_id` |

### Topology

A NetBird peer joins the cross-provider topology graph the same way the other
heuristic IP/hostname joins work: its `connection_ip` / `ip` / `ipv6` are
indexed by IP and its `dns_label` / `hostname` by hostname, so a Cloudflare DNS
record (or OCI LB) pointing at a peer's address yields a `dns` edge to the peer.
A route's `network` is a CIDR and `peer`/`groups` are id references — useful for
a future containment resolver.

### SDK notes

Unlike the other providers, NetBird has **no vendored SDK** — the read surface
is a hand-rolled stdlib `net/http` client (`internal/providers/netbird/client.go`).
The official `netbirdio/netbird` Go module pulls the entire management server +
gRPC stack, which would bloat the static binary for the sake of a handful of
GETs. The API is simple REST + token auth, so a thin client is the better trade.

---

## Tailscale

Inventories a [Tailscale](https://tailscale.com) tailnet (WireGuard mesh /
zero-trust) through the public v2 REST API.

### Auth

Set **`TAILSCALE_API_KEY`** to an API access token (`tskey-api-…`, from
[Settings → Keys](https://login.tailscale.com/admin/settings/keys)) or an OAuth
client secret — the API accepts both on the same header. The provider is
skipped with a warning when the variable is unset, exactly like Cloudflare's
token.

```bash
export TAILSCALE_API_KEY=tskey-api-xxxxxxxxxxxx
auditor audit --provider tailscale -o json
```

The token is sent as `Authorization: Bearer`. (NetBird's PAT uses the `Token`
scheme instead — the two mesh providers differ here.)

### Which tailnet

By default the provider uses `-`, the API's "the token's own tailnet"
sentinel, which also becomes each asset's `account_id`. Override it when a
token can reach several tailnets:

```bash
export TAILSCALE_TAILNET=example.com          # or --tailscale-tailnet
```

Self-hosted or Headscale-compatible control planes:

```bash
export TAILSCALE_API_BASE_URL=https://headscale.internal   # or --tailscale-api-url
```

### Required scopes

A read-only token is enough. With OAuth clients, grant `devices:core:read`,
`users:read`, `auth_keys:read`, `dns:read`, and `policy_file:read`. Missing a
scope degrades gracefully — that collector reports a non-fatal error and the
rest still collect.

### Resource matrix

| Asset type            | Endpoint                          | Notes |
| --------------------- | --------------------------------- | ----- |
| `tailscale.device`    | `GET /devices?fields=all`         | `fields=all` is required for `advertisedRoutes` / `enabledRoutes` — they're absent from the default field set, and they're what identifies a subnet router. IPv4 and IPv6 are split into `ip` / `ipv6` tags |
| `tailscale.user`      | `GET /users`                      | Role, status, device count |
| `tailscale.key`       | `GET /keys?all=true`              | `all=true` widens past machine auth keys to OAuth clients and federated identities — credentials into the tailnet too. The secret material is **never** mapped |
| `tailscale.dns`       | `GET /dns/{nameservers,searchpaths,preferences}` | Folded into one asset (a tailnet has exactly one of each). A partial read still emits, with the failures reported alongside |
| `tailscale.acl`       | `GET /acl`                        | Policy-file summary (rule/group/host counts); the full document is in `raw` |
| `tailscale.acl_rule`  | `GET /acl`                        | One asset per `acls` / `grants` / `ssh` rule — this is what the traffic-flow graph hangs edges on |
| `tailscale.acl_group` | `GET /acl`                        | `groups` entries and their members |
| `tailscale.acl_tag`   | `GET /acl`                        | `tagOwners` entries |
| `tailscale.acl_host`  | `GET /acl`                        | `hosts` aliases (name → IP/CIDR), indexed by IP for topology joins |

A tailnet with no custom policy returns **404** from `/acl`. That's "nothing
configured", not a failure, and is not reported as an error.

### Secrets

Node keys (`machineKey`, `nodeKey`) and auth-key material (`key`) have no
fields in the Go structs at all, so they cannot reach `Asset.Raw` even with
`--include-raw`. The omission is structural rather than a filter that could be
forgotten.

### Topology

Device addresses (`ip`, `ipv6`) are indexed by IP and `dns_name` / `hostname`
by hostname, so a Cloudflare DNS record or load balancer pointing at a device
yields an edge to it. More interestingly, `tailscale.acl_rule` assets drive the
**traffic-flow** resolver: `src` and `dst` selectors resolve to devices (via
their ACL tags), policy groups, users, and host aliases, producing
`traffic-allow` / `traffic-deny` edges through the rule. See
[configuration.md](./configuration.md#traffic-flow-edges).

### SDK notes

Like NetBird, no vendored SDK — a hand-rolled stdlib client
(`internal/providers/tailscale/client.go`). The official `tailscale.com` module
pulls the whole tsnet/wgengine stack, which would dwarf the read surface we
actually use.

---

## Adding new providers

See [extending.md](./extending.md).

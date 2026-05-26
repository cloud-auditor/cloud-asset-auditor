# cloud-asset-auditor (Helm chart)

Deploys [cloud-asset-auditor](https://github.com/cloud-auditor/cloud-asset-auditor)
to Kubernetes in one of two shapes:

| `mode`       | Shape                       | Use when…                                                                 |
| ------------ | --------------------------- | -------------------------------------------------------------------------- |
| `cronjob`    | `batch/v1.CronJob`          | You want periodic snapshots written to logs or a PVC (operations default). |
| `deployment` | `apps/v1.Deployment` + Service (+ optional Ingress) | You want a browser-accessible UI for ad-hoc audits. |

## Quickstart

```bash
kubectl create namespace auditor

# 1. Provide credentials. Edit and apply the example, or generate via
#    your secret manager — never commit real values.
kubectl -n auditor apply -f examples/secret.yaml

# 2. CronJob mode (default).
helm install auditor . -n auditor \
  -f examples/values-cronjob.yaml

#    OR Deployment mode.
helm install auditor . -n auditor \
  -f examples/values-deployment.yaml
```

## Values

See [`values.yaml`](./values.yaml) for the full set; each field is
documented inline. The most-touched knobs:

| Key                                 | Default                                            | Meaning                                                |
| ----------------------------------- | -------------------------------------------------- | ------------------------------------------------------ |
| `mode`                              | `cronjob`                                          | `cronjob` or `deployment`                              |
| `image.repository`                  | `ghcr.io/cloud-auditor/cloud-asset-auditor`        | OCI image source                                       |
| `image.tag`                         | `""` (Chart.appVersion)                            | Override to pin a specific build                       |
| `providers`                         | `[kubernetes]`                                     | Provider names to scan; `[]` means all registered      |
| `credentials.existingSecret`        | `""`                                               | **Required** for any non-K8s provider                  |
| `credentials.ociKeySecret`          | `""`                                               | Secret containing `oci_api_key.pem` (mounted as file)  |
| `cronjob.schedule`                  | `0 */6 * * *`                                      | Cron expression (UTC)                                  |
| `cronjob.pvc.enabled`               | `false`                                            | Persist audit output to a PVC                          |
| `deployment.serve.auth`             | `none`                                             | `none` \| `basic` \| `token`                           |
| `deployment.ingress.enabled`        | `false`                                            | Toggle the Ingress resource                            |
| `rbac.create`                       | `true`                                             | ClusterRole `[get, list]` on `*` for in-cluster audits |

## Credentials

The chart never inlines credentials. Provide an existing Secret via
`credentials.existingSecret` whose key/value pairs become env vars on the
auditor pod (`envFrom: secretRef:`).

See [`examples/secret.yaml`](./examples/secret.yaml) for the full set of
recognized keys: `CLOUDFLARE_API_TOKEN`, `OCI_TENANCY`, `OCI_USER`,
`OCI_FINGERPRINT`, `OCI_REGION`, `AUDITOR_BASIC_USER`, `AUDITOR_BASIC_PASS`,
`AUDITOR_API_TOKEN`.

The OCI SDK's env-var auth path needs a file path for the private key
(`OCI_KEY_PATH`), not inline PEM content. Put the key in a *second* Secret
under the key `oci_api_key.pem` and reference it via
`credentials.ociKeySecret`. The chart mounts it at
`/etc/oci/oci_api_key.pem` and sets `OCI_KEY_PATH` automatically.

## Permission surface (RBAC)

With `rbac.create: true` (the default) the chart provisions:

```yaml
kind: ClusterRole
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["get", "list"]
  - nonResourceURLs: ["/healthz", "/version", "/api", "/apis", "/apis/*"]
    verbs: ["get"]
```

This is **read-only everywhere** — necessary for the Kubernetes provider's
dynamic-discovery approach (it can't enumerate a narrower verb/resource
matrix in advance because CRDs are inventoried automatically). If your
threat model requires a narrower scope, set `rbac.create: false` and bind
the chart's ServiceAccount to your own role; the provider tolerates
Forbidden responses per-resource and will continue with a warning.

## Verification

```bash
helm lint .
helm template auditor . --debug
helm template auditor . -f examples/values-deployment.yaml --debug
```

Both renderings should produce valid manifests that pass
`kubectl --dry-run=client apply -f -`.

## Uninstall

```bash
helm uninstall auditor -n auditor
# PVC (if used) is preserved by default — delete it manually if you want.
kubectl -n auditor delete pvc -l app.kubernetes.io/instance=auditor
```

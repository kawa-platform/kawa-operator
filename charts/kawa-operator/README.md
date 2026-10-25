# kawa-operator

Installs the **kawa operator**, which manages a kawa gateway through `VirtualTopic`, `Client`, `Role` and `Group`
resources. The user documentation is `docs/operator.md` in kawa-docs.

Each operator release publishes this chart with the same version as the operator image:

```bash
helm install kawa-operator oci://ghcr.io/kawa-platform/charts/kawa-operator --version <version> \
  -n kawa -f operator-values.yaml
```

## Values

| Value | Meaning |
|-------|---------|
| `gateway.adminUrl` | The gateway's admin API (required). |
| `gateway.auth.existingSecret` | Secret, key `client-secret`, with the operator client's secret (required). Mounted as a file. |
| `gateway.tls.caSecret` | Secret, key `ca.crt`, for an https admin API with a private CA. |
| `namespaceSelector` | Label selector of the namespaces to serve. Empty serves all. |
| `resyncInterval` | How often synced topics are compared with the gateway again (default `5m`). |
| `metrics.enabled` | Serve controller-runtime metrics over HTTPS on `:8443` (default `false`). |

The operator's client id on the gateway is always `kawa-operator`.

## What the chart adds

- The CRDs and the operator's ClusterRole rules, which `make manifests` generates into `files/` from the operator's
  code; never edit them by hand. The CRDs carry `helm.sh/resource-policy: keep`, so uninstalling the chart leaves
  them and every custom resource.
- Aggregated ClusterRoles that give the built-in `view`, `edit` and `admin` roles access to the four kinds.
- Leader election. The lease is named after the release, so several releases
  can share a namespace.
- With `metrics.enabled`: the Service `<release>-metrics`, the RBAC the endpoint needs to check scrapers' tokens,
  and the ClusterRole `<release>-metrics-reader` to bind to a scraper's ServiceAccount.

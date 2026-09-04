# Metrics Documentation

This document describes the Prometheus metrics exposed by the HyperFleet
operator, including their meaning, labels, and example queries for common
investigations. It follows the HyperFleet metrics standard, matching the naming
and label conventions used by the other components (API, Sentinel, Adapters).

## Metrics Endpoint

Metrics are exposed at:
- **Endpoint**: `/metrics`
- **Port**: 9090 (default, configurable via `--metrics-bind-address`; set `0` to disable)
- **Protocol**: plain HTTP (`--metrics-secure=false` by default)
- **Format**: OpenMetrics/Prometheus text format

The operator's custom collectors register into controller-runtime's registry, so
they are served on the **same** `/metrics` endpoint as the built-in
`controller_runtime_*` metrics — there is no second metrics server.

All operator-defined series share the `hyperfleet_operator_` prefix (Prometheus
`Namespace` `hyperfleet` + `Subsystem` `operator`) and carry the standard
`component` and `version` const labels.

## Application Metrics

### Build Info

#### `hyperfleet_operator_build_info`

**Type:** Gauge (always 1)

**Description:** Build information for the HyperFleet operator component. The
value is always `1`; the identity is carried in the labels.

**Labels:**

| Label | Description | Example Values |
|-------|-------------|----------------|
| `component` | Component name (const) | `operator` |
| `version` | Application version (const) | `v1.2.3`, `dev` |
| `commit` | Git commit SHA (short) | `abc1234` |
| `go_version` | Go runtime version | `go1.26.0` |

**Example output:**

```text
hyperfleet_operator_build_info{component="operator",version="v1.2.3",commit="abc1234",go_version="go1.26.0"} 1
```

> Version and commit are injected at build time via `-ldflags -X` (see
> `internal/version`). Under a plain `go build`/`make run` they fall back to the
> module version and VCS revision from the binary's build info, so the metric is
> still populated (`version="dev"` when unavailable).

### Process Liveness

#### `hyperfleet_operator_up`

**Type:** Gauge

**Description:** `1` while the operator process is running. Set once at startup.
Distinct from the Prometheus scrape-generated `up` series.

**Labels:** `component`, `version` (const)

**Example output:**

```text
hyperfleet_operator_up{component="operator",version="v1.2.3"} 1
```

### Reconcile Metrics

These metrics track the `HyperFleetConfig` reconcile loop.

#### `hyperfleet_operator_reconcile_duration_seconds`

**Type:** Histogram

**Description:** Wall-clock duration of a full reconcile, recorded regardless of
outcome.

**Labels:** `component`, `version` (const)

**Buckets:** `0.005s`, `0.01s`, `0.025s`, `0.05s`, `0.1s`, `0.25s`, `0.5s`, `1s`, `2.5s`, `5s`, `10s`

**Derived metrics:**
- `hyperfleet_operator_reconcile_duration_seconds_sum`
- `hyperfleet_operator_reconcile_duration_seconds_count`
- `hyperfleet_operator_reconcile_duration_seconds_bucket`

#### `hyperfleet_operator_reconcile_errors_total`

**Type:** Counter

**Description:** Total number of reconcile errors, labeled by the stage that
failed, so error rate can be broken down by cause.

**Labels:**

| Label | Description | Example Values |
|-------|-------------|----------------|
| `component` | Component name (const) | `operator` |
| `version` | Application version (const) | `v1.2.3` |
| `reason` | Reconcile stage that failed | `get`, `discovery`, `secrets`, `bundle`, `render`, `apply` |

**Example output:**

```text
hyperfleet_operator_reconcile_errors_total{component="operator",version="v1.2.3",reason="apply"} 3
```

### Operand Metrics

These metrics track the operands the operator manages (one per component in the
resolved bundle, e.g. `api`).

#### `hyperfleet_operator_operand_ready`

**Type:** Gauge

**Description:** Readiness of each operand workload: `1` when the Deployment
reports `Available=True`, `0` otherwise. Published from the freshly applied state
each reconcile.

**Labels:**

| Label | Description | Example Values |
|-------|-------------|----------------|
| `component` | Component name (const) | `operator` |
| `version` | Application version (const) | `v1.2.3` |
| `operand` | Operand component name | `api` |

**Example output:**

```text
hyperfleet_operator_operand_ready{component="operator",version="v1.2.3",operand="api"} 1
```

#### `hyperfleet_operator_operand_rollouts_total`

**Type:** Counter

**Description:** Total operand workload rollouts, labeled by operand and the
trigger that caused the rollout. Detected before applying, by comparing the live
pod template against the desired one via a template-hash annotation.

**Labels:**

| Label | Description | Example Values |
|-------|-------------|----------------|
| `component` | Component name (const) | `operator` |
| `version` | Application version (const) | `v1.2.3` |
| `operand` | Operand component name | `api` |
| `trigger` | What caused the rollout | `create`, `image`, `config` |

**Trigger values:**
- `create` — the operand's workload did not exist and was created.
- `image` — a rollout caused by a container image change.
- `config` — a rollout caused by any other pod-template change (env, resources, ...).

**Example output:**

```text
hyperfleet_operator_operand_rollouts_total{component="operator",version="v1.2.3",operand="api",trigger="create"} 1
hyperfleet_operator_operand_rollouts_total{component="operator",version="v1.2.3",operand="api",trigger="image"} 4
```

### Applied Config

#### `hyperfleet_operator_applied_config_info`

**Type:** Gauge (info-style, always 1)

**Description:** Info metric whose `hash` label is the digest of the currently
applied configuration: the `HyperFleetConfig` spec plus every component's
config-rollout hash (rendered config content and referenced-Secret
resourceVersions). Covering only the spec would miss a Secret rotation or a
resolved value that never lands in the CR (e.g. OIDC JWKS discovery), either of
which changes what is actually applied to an operand without changing the spec
itself. Exactly **one** series exists at a time: the collector is reset before
each set, so a change replaces the previous series rather than accumulating
cardinality.

**Labels:**

| Label | Description | Example Values |
|-------|-------------|----------------|
| `component` | Component name (const) | `operator` |
| `version` | Application version (const) | `v1.2.3` |
| `hash` | 12-char SHA-256 digest of the applied spec and component config hashes | `9f2a1c4b7d3e` |

**Example output:**

```text
hyperfleet_operator_applied_config_info{component="operator",version="v1.2.3",hash="9f2a1c4b7d3e"} 1
```

## Controller-Runtime Metrics

Because the operator's collectors share controller-runtime's registry, the
standard controller-runtime metrics are exposed on the same endpoint, including:

| Metric | Type | Description |
|--------|------|-------------|
| `controller_runtime_reconcile_total` | Counter | Total reconciles per controller, by result (`success`, `error`, `requeue`). |
| `controller_runtime_reconcile_errors_total` | Counter | Total reconcile errors per controller. |
| `controller_runtime_reconcile_time_seconds` | Histogram | Reconcile latency per controller. |
| `controller_runtime_active_workers` | Gauge | Number of active reconcile workers per controller. |
| `workqueue_depth` | Gauge | Current depth of the reconcile work queue. |

## Go Runtime and Process Metrics

The Prometheus Go client library automatically exposes process and runtime
metrics on the same endpoint, e.g. `process_cpu_seconds_total`,
`process_resident_memory_bytes`, `process_start_time_seconds`, `go_goroutines`,
and the `go_memstats_*` family. Use `process_start_time_seconds` (rather than a
custom build-time label) to answer "when did this binary start".

## Example PromQL Queries

### Reconcile Health

```promql
# Reconcile rate (per second)
rate(hyperfleet_operator_reconcile_duration_seconds_count[5m])

# Reconcile error rate by failing stage
sum by (reason) (rate(hyperfleet_operator_reconcile_errors_total[5m]))

# P99 reconcile latency
histogram_quantile(0.99,
  sum(rate(hyperfleet_operator_reconcile_duration_seconds_bucket[5m])) by (le))

# Average reconcile duration
rate(hyperfleet_operator_reconcile_duration_seconds_sum[10m]) /
rate(hyperfleet_operator_reconcile_duration_seconds_count[10m])
```

### Operand Health

```promql
# Operands that are not ready
hyperfleet_operator_operand_ready == 0

# Rollout rate by operand and trigger
sum by (operand, trigger) (rate(hyperfleet_operator_operand_rollouts_total[15m]))

# Image-triggered rollouts in the last hour
increase(hyperfleet_operator_operand_rollouts_total{trigger="image"}[1h])
```

### Fleet State

```promql
# Currently applied config digest (the single active series)
hyperfleet_operator_applied_config_info

# Operator build identity
hyperfleet_operator_build_info

# Is the operator up?
hyperfleet_operator_up
```

## Prometheus Operator Integration

The operator creates its own `ServiceMonitor` (`controller-manager-metrics-monitor`)
at runtime so Prometheus scrapes the `:9090` endpoint over plain HTTP. The
bootstrap is conditional: it runs only when the cluster serves the Prometheus
Operator API (`monitoring.coreos.com/v1`), and is skipped — with a log line — when
that CRD is absent. Metrics remain available on `:9090` either way. See the
`internal/servicemonitor` package.

The `ServiceMonitor` is intentionally **not** shipped in the OLM bundle. OLM
applies a bundle's arbitrary manifests but does not install the CRDs they depend
on, so bundling it would fail the InstallPlan — and block the entire operator
install — on any cluster without the Prometheus Operator CRD. Because HyperFleet
targets generic Kubernetes (not only OpenShift, where that CRD is guaranteed), the
runtime bootstrap above degrades gracefully instead.

A cluster that installs the Prometheus Operator *after* the operator started picks
the `ServiceMonitor` up on the operator's next restart. For GitOps, the equivalent
static manifest remains available at `config/prometheus/monitor.yaml` and can be
applied directly (requires the `ServiceMonitor` CRD).

## Related Documentation

- [README](../README.md#observability-endpoints) — observability endpoints quick reference
- [HyperFleet metrics standard](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/standards/metrics.md)
- [HyperFleet health-endpoints standard](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/standards/health-endpoints.md)

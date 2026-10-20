# HyperFleet Operator

The HyperFleet Operator packages and operates HyperFleet through OLM. Its
partner-facing API is a single cluster-scoped `HyperFleetConfig` custom
resource; the workloads and supporting objects produced from that resource are
operator-owned implementation details.

## Getting Started

- **Contributing or developing locally:** follow the
  [developer guide](docs/developer-guide.md) for prerequisites, local validation,
  and end-to-end testing. Start with `make install-hooks` to set up the git
  hooks (secret scanning, commitlint, linting, file hygiene).
- **Installing through OLM:** follow the
  [OLM bundle, catalog, and development installation guide](docs/olm.md).
- **Installing on a disconnected OpenShift cluster:** follow the
  [disconnected installation guide](docs/disconnected-install.md).

## Documentation

### Developing and extending the operator

- [Developer guide](docs/developer-guide.md): repository layout, local workflow,
  unit and end-to-end tests, generated files, and troubleshooting.
- [`HyperFleetConfig` reference](docs/hyperfleetconfig-reference.md): complete
  specification, referenced Secret contracts, status conditions, and known
  implementation limits.
- [Component pattern](docs/component-pattern.md): the contract and checklist for
  adding a shared or bundle-specific component.

### Installing and operating the operator

- [OLM bundle, catalog, and development installation](docs/olm.md)
- [Disconnected OpenShift installation](docs/disconnected-install.md)
- [Metrics reference](docs/metrics.md)

The disconnected workflow mirrors the published catalog. The catalog selects
the OLM bundle and its related images.

## Observability

The manager exposes these endpoints by default:

- Liveness: `http://localhost:8080/healthz`
- Readiness: `http://localhost:8080/readyz`
- Metrics: `http://localhost:9090/metrics`

Metrics use the `hyperfleet_operator_*` namespace. See the
[metrics reference](docs/metrics.md) for the complete catalogue, labels, and
example PromQL queries.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at <http://www.apache.org/licenses/LICENSE-2.0>.

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.

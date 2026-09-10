# hyperfleet-operator

A Kubernetes operator for HyperFleet cluster lifecycle management.

## Description

hyperfleet-operator packages and delivers HyperFleet as a standard Kubernetes operator, installed and managed through OLM. It exposes a single cluster-scoped custom resource, `HyperFleetConfig`, as the entire partner-facing surface: install, configure, and observe HyperFleet through that one CR and its status conditions, with everything else the operator manages kept internal.

### Getting Started

For comprehensive installation and build instructions including:
- OLM installation - via catalog or bundle
- Non-OLM installation - image build and deployment

See [docs/olm.md](docs/olm.md#developer-installation)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.


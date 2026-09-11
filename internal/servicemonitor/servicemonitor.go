/*
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
*/

// Package servicemonitor bootstraps the operator's own Prometheus ServiceMonitor
// at runtime, but only when the Prometheus Operator API is present on the cluster.
//
// The ServiceMonitor (monitoring.coreos.com/v1) is deliberately NOT shipped in the
// OLM bundle: OLM applies a bundle's arbitrary manifests via an InstallPlan but does
// not install the CRD they depend on, so bundling the ServiceMonitor would fail the
// InstallPlan — and block the entire operator install — on any cluster without the
// Prometheus Operator CRD. Since HyperFleet targets generic Kubernetes (not only
// OpenShift, where that CRD is guaranteed), the operator instead creates the
// ServiceMonitor itself when, and only when, the API is available, and degrades
// gracefully (metrics still served on :9090) when it is not.
package servicemonitor

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// The operator creates and maintains its own ServiceMonitor in its own namespace
// when the Prometheus Operator API is present. Namespaced (not a ClusterRole rule):
// the ServiceMonitor only ever lives in the operator's namespace, matching the
// least-privilege scoping used for the secrets grant. The namespace literal must
// match config/default/kustomization.yaml's namespace transformer.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch,namespace=hyperfleet-system

const (
	// serviceMonitorName is the operator's own ServiceMonitor. It matches the static
	// manifest historically shipped in config/prometheus/monitor.yaml so GitOps users
	// who apply that overlay and clusters relying on this runtime path converge on the
	// same object.
	serviceMonitorName = "controller-manager-metrics-monitor"

	// appName is the operator's app.kubernetes.io/name label value and its
	// server-side-apply field-manager identity.
	appName = "hyperfleet-operator"
	// metricsServiceName is the operator's metrics Service, as named after
	// config/default's namePrefix (hyperfleet-operator-) is applied to
	// config/default/metrics_service.yaml, matching the name used in OLM
	// bundle manifests. The ServiceMonitor is set to be owned by this Service,
	// so it is garbage-collected along with the rest of the operator's install
	// instead of being left behind as an orphan.
	metricsServiceName = "hyperfleet-operator-controller-manager-metrics-service"
	// controlPlane is the control-plane label value shared by the operator's
	// Deployment, its metrics Service and this ServiceMonitor.
	controlPlane = "controller-manager"

	smGroup   = "monitoring.coreos.com"
	smVersion = "v1"
	smKind    = "ServiceMonitor"
)

// Bootstrapper is a manager Runnable that ensures the operator's ServiceMonitor
// exists. It runs once, after the manager wins leader election, so only the active
// instance writes the object.
type Bootstrapper struct {
	// Config is the rest.Config used to probe API availability and apply the object.
	Config *rest.Config
	// Namespace is the operator's own namespace, where the ServiceMonitor is created.
	Namespace string
}

// NeedLeaderElection makes the bootstrap run only on the leader, so a multi-replica
// rollout does not race to create the same object.
func (b *Bootstrapper) NeedLeaderElection() bool { return true }

// Start ensures the ServiceMonitor exists when the Prometheus Operator API is
// available. It is best-effort: every failure is logged and swallowed so metrics
// bootstrap never crashes the manager or blocks reconciliation. A cluster that
// installs the Prometheus Operator after the operator started picks the
// ServiceMonitor up on the operator's next restart.
func (b *Bootstrapper) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("servicemonitor")

	available, err := serviceMonitorAvailable(b.Config)
	if err != nil {
		// Discovery failed (e.g. a transient API server error). Skip rather than
		// crash: metrics are still exposed on :9090 and a restart retries.
		log.Error(err, "could not determine ServiceMonitor API availability; skipping ServiceMonitor bootstrap")
		return nil
	}
	if !available {
		log.Info("Prometheus Operator API (monitoring.coreos.com/v1 ServiceMonitor) not present; " +
			"skipping ServiceMonitor creation. Install the Prometheus Operator and restart the operator to enable " +
			"scraping, or apply config/prometheus manually.")
		return nil
	}

	cl, err := client.New(b.Config, client.Options{})
	if err != nil {
		log.Error(err, "could not build client for ServiceMonitor bootstrap")
		return nil
	}

	// Own the ServiceMonitor by the metrics Service it scrapes, so it is
	// garbage-collected when the operator's install is removed instead of being
	// left behind as an orphan (it lives in the operator's own namespace, so a
	// same-namespace owner reference is valid).
	svc := &corev1.Service{}
	if err := cl.Get(ctx, client.ObjectKey{Name: metricsServiceName, Namespace: b.Namespace}, svc); err != nil {
		log.Error(err, "could not read metrics Service; skipping ServiceMonitor bootstrap",
			"name", metricsServiceName, "namespace", b.Namespace)
		return nil
	}

	sm := buildServiceMonitor(b.Namespace, svc)
	applyConfig := client.ApplyConfigurationFromUnstructured(sm)
	if err := cl.Apply(ctx, applyConfig, client.FieldOwner(appName), client.ForceOwnership); err != nil {
		log.Error(err, "failed to apply operator ServiceMonitor",
			"name", serviceMonitorName, "namespace", b.Namespace)
		return nil
	}
	log.Info("ensured operator ServiceMonitor", "name", serviceMonitorName, "namespace", b.Namespace)
	return nil
}

// serviceMonitorAvailable reports whether the cluster serves the ServiceMonitor
// API. A cluster without the Prometheus Operator returns NotFound for the group
// version, which is a clean "not available" rather than an error.
func serviceMonitorAvailable(cfg *rest.Config) (bool, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("create discovery client: %w", err)
	}
	groupVersion := smGroup + "/" + smVersion
	list, err := dc.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("list server resources for %s: %w", groupVersion, err)
	}
	return hasServiceMonitorKind(list), nil
}

// hasServiceMonitorKind reports whether the API resource list advertises the
// ServiceMonitor kind. Split out from serviceMonitorAvailable so the matching
// logic is unit-testable without a live discovery client.
func hasServiceMonitorKind(list *metav1.APIResourceList) bool {
	if list == nil {
		return false
	}
	for _, r := range list.APIResources {
		if r.Kind == smKind {
			return true
		}
	}
	return false
}

// buildServiceMonitor renders the operator's ServiceMonitor as an unstructured
// object. Unstructured avoids taking a compile-time dependency on the Prometheus
// Operator API module for a single fixed object. The selector must match the
// labels the operator's metrics Service carries (config/default/metrics_service.yaml)
// or Prometheus scrapes nothing. It is owned by that same Service, so deleting
// the operator's install (and its metrics Service with it) garbage-collects the
// ServiceMonitor too, instead of leaving it behind as an orphan.
//
// The endpoint's scheme is hardcoded to "http", matching the HyperFleet metrics
// standard's plain-HTTP default (--metrics-secure=false). Bootstrapper does not
// know the operator's --metrics-secure setting, so if it is run with
// --metrics-secure=true this ServiceMonitor will scrape an HTTPS+authn/authz
// endpoint over plain HTTP and fail. See config/prometheus/monitor.yaml.
func buildServiceMonitor(namespace string, owner *corev1.Service) *unstructured.Unstructured {
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(schema.GroupVersionKind{Group: smGroup, Version: smVersion, Kind: smKind})
	sm.SetName(serviceMonitorName)
	sm.SetNamespace(namespace)
	sm.SetLabels(map[string]string{
		"control-plane":                controlPlane,
		"app.kubernetes.io/name":       appName,
		"app.kubernetes.io/managed-by": appName,
	})
	sm.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Service",
		Name:       owner.Name,
		UID:        owner.UID,
	}})
	sm.Object["spec"] = map[string]any{
		"selector": map[string]any{
			"matchLabels": map[string]any{
				"control-plane":          controlPlane,
				"app.kubernetes.io/name": appName,
			},
		},
		"endpoints": []any{
			map[string]any{
				"path":     "/metrics",
				"port":     "metrics", // matches the metrics Service port name
				"scheme":   "http",
				"interval": "30s",
			},
		},
	}
	return sm
}

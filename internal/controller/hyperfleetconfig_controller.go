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

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/apply"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/bundle"
)

// HyperFleetConfigReconciler reconciles a HyperFleetConfig object. It is the
// single bundle controller: it resolves spec.bundle to a component set and
// reconciles each component's operands via server-side apply.
type HyperFleetConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorNamespace is the namespace the operator runs in and where all
	// operands are created. Sourced from OPERATOR_NAMESPACE (downward API) in main.go.
	OperatorNamespace string
	// APIImage is the container image for the API operand. Sourced from
	// RELATED_IMAGE_HYPERFLEET_API in main.go; empty falls back to the API
	// component's compiled-in default.
	APIImage string
	// HTTPClient performs OIDC discovery requests. Nil falls back to a lazily-built,
	// reused hardened default client (see discoverJWKSURL/discoveryClientOnce);
	// tests inject one pointed at an httptest server, bypassing that default
	// entirely.
	HTTPClient *http.Client

	// discoveryClientOnce/discoveryClient lazily construct the default discovery
	// client exactly once per reconciler instance, so repeated reconciles reuse
	// one http.Transport (and its connection pool) instead of leaking a fresh one
	// per call — a fresh net.Dialer-backed Transport has IdleConnTimeout's zero
	// value (no limit), so discarding it after one request never closes whatever
	// connection it opened.
	discoveryClientOnce sync.Once
	discoveryClient     *http.Client

	// discoveryCacheMu guards discoveryCache.
	discoveryCacheMu sync.Mutex
	// discoveryCache holds the last successfully discovered jwks_uri per issuer,
	// so a transient IdP outage degrades to "keep using the last-known key
	// endpoint" rather than blocking every reconcile — including ones triggered
	// by an unrelated Secret rotation — on that issuer being reachable. See
	// resolveJWKSURL.
	discoveryCache map[string]string
}

// +kubebuilder:rbac:groups=hyperfleet.redhat.com,resources=hyperfleetconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hyperfleet.redhat.com,resources=hyperfleetconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hyperfleet.redhat.com,resources=hyperfleetconfigs/finalizers,verbs=update
// The operator reconciles operands with server-side apply (create/update/patch)
// and relies on owner-reference garbage collection (run by kube-controller-manager,
// not this operator) for cleanup, so it needs no delete permission on operands.
// get;list;watch back the Owns() informer caches.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete,namespace=hyperfleet-system
// Secrets are referenced (not owned): the operator reads partner-provided
// database/TLS/JWKS Secrets to compute the config-rollout hash and watches them
// so a rotation re-triggers reconcile. get;list;watch only — the operator never
// writes them. Namespaced (not a ClusterRole rule): every referenced Secret
// lives in the operator's own namespace (see SecretReference), matching the
// cache scoping in cmd/main.go. The namespace literal here must match that
// default and config/default/kustomization.yaml's namespace transformer, which
// also rewrites this Role's own metadata.namespace at build time.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=hyperfleet-system

// Reconcile drives the cluster toward the desired state for the HyperFleetConfig
// singleton. It is level-based and idempotent: it renders each component's
// desired operands from the CR and server-side-applies them, so running it twice
// with no spec change writes nothing, and out-of-band drift self-heals (the
// Owns() watches re-invoke this loop).
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *HyperFleetConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	log := logf.FromContext(ctx)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		// The CR is gone: its operands carry controller owner references, so the
		// built-in garbage collector removes them. No finalizer is required. There
		// is also nothing to write status to, so this path bypasses the deferred
		// status patch below entirely.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// componentConditions, componentsCollected and missingSecrets feed
	// aggregateStatus in the deferred status patch below. They are populated as
	// reconciliation proceeds so that a mid-loop failure (reterr set, named
	// return) still status-patches with whatever was learned before the failure
	// — e.g. a missing Secret discovered before a later component's apply fails
	// still surfaces as Degraded. componentsCollected is set true only once the
	// per-component loop below completes without error; aggregateStatus uses it
	// to avoid publishing a fabricated Available/Progressing value when the
	// real state was never actually checked this reconcile.
	var componentConditions []metav1.Condition
	var componentsCollected bool
	var missingSecrets []string
	defer func() {
		if reterr != nil {
			log.Error(reterr, "reconcile failed")
		}
		if aggregateStatus(cr, componentConditions, componentsCollected, missingSecrets, reterr) {
			if err := r.Status().Update(ctx, cr); err != nil {
				reterr = errors.Join(reterr, fmt.Errorf("update status: %w", err))
			}
		}
	}()

	// Resolve the JWKS URL. When auth is on and the CR pins neither a JWKS URL nor
	// a JWKS Secret, this performs OIDC discovery — a network read, so it lives
	// here rather than in the pure renderer. Empty otherwise.
	jwksURL, err := r.resolveJWKSURL(ctx, cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve JWKS URL: %w", err)
	}

	// Read each referenced Secret's resourceVersion for the rollout hash. A
	// rotation bumps resourceVersion without changing the rendered config.yaml or
	// the pod spec, so without hashing it the pods would keep running with stale
	// credentials/certs. A missing Secret is not fatal here — it is hashed as
	// absent so the pods roll once the Secret appears — but it does drive the
	// Degraded condition via missingSecrets below.
	secretData, err := r.referencedSecretData(ctx, cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read referenced secrets: %w", err)
	}
	for _, e := range secretData {
		if !e.present {
			missingSecrets = append(missingSecrets, e.id)
		}
	}

	components, err := bundle.Resolve(cr.Spec.Bundle, bundle.Config{
		APIImage:        r.APIImage,
		Namespace:       r.OperatorNamespace,
		ResolvedJWKSURL: jwksURL,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve components: %w", err)
	}

	// Preallocated to the largest plausible size: each component contributes
	// exactly one Available and one Progressing condition today.
	componentConditions = make([]metav1.Condition, 0, len(components)*2)

	for _, component := range components {
		objs, err := component.Render(ctx, cr)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("render component %q: %w", component.Name(), err)
		}
		// Stamp the config-hash on the component's Deployment pod template so a
		// config or secret-value change rolls the pods. For components without a
		// ConfigMap+Deployment pair (none today besides the API) this is a no-op.
		stampConfigHash(objs, secretData)
		if err := apply.Objects(ctx, r.Client, cr, r.Scheme, objs); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply component %q: %w", component.Name(), err)
		}
		// apply.Objects patches each object in place via server-side apply, so objs
		// now carries whatever status the API server currently has for it (e.g. the
		// Deployment controller's last-written replica counts) — Conditions reads
		// that back rather than performing its own Get.
		conds, err := component.Conditions(ctx, cr, objs)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get conditions for component %q: %w", component.Name(), err)
		}
		componentConditions = append(componentConditions, conds...)
	}
	componentsCollected = true

	// TODO(HYPERFLEET-1512): enforce that referenced Secrets exist in
	// r.OperatorNamespace beyond driving the Degraded condition above — e.g.
	// rejecting or fast-failing before Render/Apply run at all.

	log.Info("reconciled HyperFleetConfig",
		"bundle", cr.Spec.Bundle, "components", len(components), "namespace", r.OperatorNamespace)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. It watches the
// HyperFleetConfig and every operand type it owns, so an out-of-band change to
// any operand re-invokes Reconcile and the desired state is re-applied. It also
// watches Secrets (which it references but does not own) so a rotation of a
// referenced database/TLS/JWKS Secret re-triggers reconcile and rolls the pods.
func (r *HyperFleetConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hyperfleetv1alpha1.HyperFleetConfig{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToConfig),
			// Only wake on actual content changes: ResourceVersionChangedPredicate
			// filters out resyncs and no-op updates, so a periodic relist does not
			// spuriously re-reconcile.
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Named("hyperfleetconfig").
		Complete(r)
}

// mapSecretToConfig enqueues the singleton HyperFleetConfig when a Secret in the
// operator namespace changes. It does not filter to the specific referenced
// Secret names: the reference set lives in the CR spec, and re-reconciling on any
// Secret in the operator's own namespace is cheap (there is a single CR) and
// keeps this mapping free of spec-reading. Reconcile then reads only the Secrets
// the current spec references. Secrets outside the operator namespace are ignored
// (referenced Secrets must live there — see SecretReference).
func (r *HyperFleetConfigReconciler) mapSecretToConfig(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: hyperfleetv1alpha1.SingletonName}},
	}
}

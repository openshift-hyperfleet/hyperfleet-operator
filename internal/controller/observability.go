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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// templateHashAnnotation records the digest of the pod template the operator last
// applied to an operand Deployment. It is stored in the Deployment's metadata (not
// its pod template), so writing it never itself triggers a rollout; it exists only
// so the next reconcile can tell whether the desired pod template changed and thus
// whether an apply will roll the workload. HYPERFLEET-1408 layers spec-derived
// content into the template; this annotation already accounts for it.
const templateHashAnnotation = "hyperfleet.redhat.com/template-hash"

// hashConfig returns a short, stable digest of the applied spec. json.Marshal of a
// Go struct is field-ordered and deterministic, so equal specs hash equally across
// reconciles and process restarts.
func hashConfig(spec hyperfleetv1alpha1.HyperFleetConfigSpec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		// A spec that cannot be marshaled is not something the caller can act on;
		// fall back to a sentinel so the metric still publishes a single series.
		return "unmarshalable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// hashPodTemplate returns a short, stable digest of a Deployment's pod template.
func hashPodTemplate(dep *appsv1.Deployment) string {
	b, err := json.Marshal(dep.Spec.Template)
	if err != nil {
		return "unmarshalable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// stampTemplateHash records the desired pod-template digest on the Deployment so
// the apply persists it for the next reconcile's rollout comparison.
func stampTemplateHash(dep *appsv1.Deployment, hash string) {
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[templateHashAnnotation] = hash
}

// recordRollouts inspects each Deployment a component wants to apply and, when the
// apply will roll the workload, increments the rollout counter with the trigger.
// It must run BEFORE apply, while the live object still holds the previous state.
// It also stamps the desired template hash onto the object so the apply persists
// it. Metrics are best-effort: a read error here is logged and skipped, never
// surfaced as a reconcile failure.
func (r *HyperFleetConfigReconciler) recordRollouts(ctx context.Context, component string, objs []client.Object) {
	log := logf.FromContext(ctx)
	for _, o := range objs {
		dep, ok := o.(*appsv1.Deployment)
		if !ok {
			continue
		}
		desired := hashPodTemplate(dep)
		stampTemplateHash(dep, desired)

		live := &appsv1.Deployment{}
		err := r.Get(ctx, client.ObjectKeyFromObject(dep), live)
		switch {
		case apierrors.IsNotFound(err):
			metrics.IncOperandRollout(component, metrics.TriggerCreate)
		case err != nil:
			log.V(1).Info("skipping rollout metric: could not read live operand",
				"component", component, "deployment", dep.Name, "error", err.Error())
		default:
			prev := live.Annotations[templateHashAnnotation]
			// prev == "" means we have never stamped this Deployment (e.g. first
			// reconcile after upgrading to this operator version): adopt the hash
			// silently rather than count a rollout we cannot attribute.
			if prev != "" && prev != desired {
				metrics.IncOperandRollout(component, rolloutTrigger(live, dep))
			}
		}
	}
}

// rolloutTrigger classifies why a rollout is happening: an image change if any
// container image differs, otherwise a config/template change.
func rolloutTrigger(live, desired *appsv1.Deployment) string {
	if !sameContainerImages(live, desired) {
		return metrics.TriggerImage
	}
	return metrics.TriggerConfig
}

// sameContainerImages reports whether both Deployments have the same container
// images in the same order.
func sameContainerImages(a, b *appsv1.Deployment) bool {
	ac, bc := a.Spec.Template.Spec.Containers, b.Spec.Template.Spec.Containers
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if ac[i].Image != bc[i].Image {
			return false
		}
	}
	return true
}

// recordReadiness reads the live status of each of a component's Deployments after
// apply and publishes the operand readiness gauge. A Deployment is ready when it
// reports the Available condition True. Best-effort: read errors are logged and the
// gauge is left untouched rather than failing the reconcile.
func (r *HyperFleetConfigReconciler) recordReadiness(ctx context.Context, component string, objs []client.Object) {
	log := logf.FromContext(ctx)
	for _, o := range objs {
		dep, ok := o.(*appsv1.Deployment)
		if !ok {
			continue
		}
		live := &appsv1.Deployment{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(dep), live); err != nil {
			log.V(1).Info("skipping readiness metric: could not read live operand",
				"component", component, "deployment", dep.Name, "error", err.Error())
			continue
		}
		metrics.SetOperandReady(component, deploymentAvailable(live))
	}
}

// deploymentAvailable reports whether a Deployment carries the Available=True
// condition.
func deploymentAvailable(dep *appsv1.Deployment) bool {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == "True"
		}
	}
	return false
}

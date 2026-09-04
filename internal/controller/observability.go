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
	"io"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

// hashConfig returns a short, stable digest of the fully-applied state: the CR
// spec plus every rendered component's config-rollout digest (see
// computeConfigHash, returned by stampConfigHash). Spec alone is not enough — a
// referenced Secret rotation, or a resolved value that never lands in the CR
// (e.g. resolveJWKSURL's OIDC discovery), can change what is actually applied to
// an operand without the spec itself changing, and this metric must reflect
// that too. json.Marshal of a Go struct is field-ordered and deterministic, so
// equal inputs hash equally across reconciles and process restarts.
func hashConfig(spec hyperfleetv1alpha1.HyperFleetConfigSpec, componentConfigHashes []string) string {
	b, err := json.Marshal(spec)
	if err != nil {
		// A spec that cannot be marshaled is not something the caller can act on;
		// fall back to a sentinel rather than fail the metric outright.
		b = []byte("unmarshalable")
	}
	h := sha256.New()
	_, _ = h.Write(b)
	for _, ch := range componentConfigHashes {
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, ch)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
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

// rolloutEvent is a rollout detected by detectRollouts but not yet reported to
// the rollout counter. Splitting detection from reporting lets the caller defer
// metrics.IncOperandRollout until apply has actually succeeded, so a failed
// apply — retried on the next reconcile — is not counted as a rollout that
// never happened.
type rolloutEvent struct {
	component string
	trigger   string
}

// detectRollouts inspects each Deployment a component wants to apply and reports
// which ones the apply will roll, and why. It must run BEFORE apply, while the
// live object still holds the previous state. It also stamps the desired
// template hash onto the object so the apply persists it. Callers must pass the
// returned events to commitRollouts only after apply succeeds — see that
// function's doc comment. Detection itself is best-effort: a read error here is
// logged and skipped, never surfaced as a reconcile failure.
func (r *HyperFleetConfigReconciler) detectRollouts(ctx context.Context, component string, objs []client.Object) []rolloutEvent {
	log := logf.FromContext(ctx)
	var events []rolloutEvent
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
			events = append(events, rolloutEvent{component: component, trigger: metrics.TriggerCreate})
		case err != nil:
			log.V(1).Info("skipping rollout metric: could not read live operand",
				"component", component, "deployment", dep.Name, "error", err.Error())
		default:
			prev := live.Annotations[templateHashAnnotation]
			// prev == "" means we have never stamped this Deployment (e.g. first
			// reconcile after upgrading to this operator version): adopt the hash
			// silently rather than count a rollout we cannot attribute.
			if prev != "" && prev != desired {
				events = append(events, rolloutEvent{component: component, trigger: rolloutTrigger(live, dep)})
			}
		}
	}
	return events
}

// commitRollouts reports previously-detected rollout events to the rollout
// counter. Call it only after the apply that carries the stamped template hash
// has succeeded, so a failed apply (which leaves the live object's annotation
// unchanged, and is retried on the next reconcile) is never counted.
func commitRollouts(events []rolloutEvent) {
	for _, e := range events {
		metrics.IncOperandRollout(e.component, e.trigger)
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
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

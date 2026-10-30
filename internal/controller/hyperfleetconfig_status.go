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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

// aggregateStatus rolls up component health into the CR's status: the
// controller-level Degraded condition (missing referenced Secrets, reconcile
// errors) plus Available (AND) and Progressing (OR) over the components'
// reported conditions into status.conditions, observedGeneration, and each
// observed component's own entry in status.components (HYPERFLEET-1653). It
// reports whether anything changed, so Reconcile can skip a no-op
// Status().Update().
//
// componentStatuses carries only components whose Conditions() were actually
// gathered this reconcile, and componentsCollected is true only when the
// render/apply/Conditions loop ran to completion. When it did not, Available
// and Progressing are left at their last-known values rather than fabricated
// as healthy; Degraded is always evaluated, since a reconcile error is itself
// the signal.
func aggregateStatus(cr *hyperfleetv1alpha1.HyperFleetConfig, componentStatuses []hyperfleetv1alpha1.ComponentStatus, componentsCollected bool, missingSecrets []string, reconcileErr error) bool {
	changed := false

	componentConditions := flattenComponentConditions(cr, componentStatuses)

	conditions := []metav1.Condition{degradedCondition(missingSecrets, reconcileErr)}
	if componentsCollected {
		conditions = append(conditions,
			rollupCondition(hyperfleetv1alpha1.ConditionAvailable, componentConditions,
				hyperfleetv1alpha1.ReasonDeploymentAvailable, "all components are available"),
			rollupProgressing(componentConditions),
		)
	}

	for _, cond := range conditions {
		cond.ObservedGeneration = cr.Generation
		if meta.SetStatusCondition(&cr.Status.Conditions, cond) {
			changed = true
		}
	}

	if mergeComponentStatuses(cr, componentStatuses, componentsCollected) {
		changed = true
	}

	if componentsCollected && cr.Status.ObservedGeneration != cr.Generation {
		cr.Status.ObservedGeneration = cr.Generation
		changed = true
	}

	return changed
}

// flattenComponentConditions returns the conditions the rollup consumes: each
// observed component's reported conditions, flattened. This is the same data
// that lands in status.components, so there is a single health calculation.
//
// A component reporting an empty condition set (a Conditions() contract
// violation) falls back to its retained status.components entry's last-known
// conditions, so the top-level rollup can never contradict that entry. As a
// consequence the top-level conditions carry cr.Generation even though part of
// their input was evaluated at an earlier one; the per-component entries keep
// the truer, per-source stamps.
func flattenComponentConditions(cr *hyperfleetv1alpha1.HyperFleetConfig, componentStatuses []hyperfleetv1alpha1.ComponentStatus) []metav1.Condition {
	total := 0
	for _, cs := range componentStatuses {
		total += len(cs.Conditions)
	}
	conds := make([]metav1.Condition, 0, total)
	for _, cs := range componentStatuses {
		reported := cs.Conditions
		if len(reported) == 0 {
			if retained := findComponentStatus(cr.Status.Components, cs.Name); retained != nil {
				reported = retained.Conditions
			}
		}
		conds = append(conds, reported...)
	}
	return conds
}

// mergeComponentStatuses writes each observed component's conditions into its
// status.components entry, preserving the entry's identity and position and
// reusing meta.SetStatusCondition so unchanged values keep their
// lastTransitionTime and produce no change. Each condition's
// observedGeneration is stamped with the generation evaluated this reconcile,
// even on one that later aborts — that stamp is how a reader tells an entry
// that was freshly observed from one that was not.
//
// When componentsCollected is true, entries for components no longer in the
// resolved set are dropped; an aborted reconcile keeps them, since the
// unobserved remainder of the set is unknown. Similarly, a condition type an
// observed component stops reporting is dropped from its entry. A component
// reporting no conditions at all is skipped entirely — an empty report is a
// contract violation, not an intentional wipe — so it keeps its last-known
// entry and never gains one.
func mergeComponentStatuses(cr *hyperfleetv1alpha1.HyperFleetConfig, observed []hyperfleetv1alpha1.ComponentStatus, componentsCollected bool) bool {
	changed := false

	if componentsCollected {
		kept := make([]hyperfleetv1alpha1.ComponentStatus, 0, len(cr.Status.Components))
		for _, existing := range cr.Status.Components {
			if slices.ContainsFunc(observed, func(cs hyperfleetv1alpha1.ComponentStatus) bool { return cs.Name == existing.Name }) {
				kept = append(kept, existing)
			} else {
				changed = true
			}
		}
		cr.Status.Components = kept
	}

	for _, cs := range observed {
		if len(cs.Conditions) == 0 {
			// Contract violation (see above): keep the last-known entry rather
			// than persisting a conditions-less one.
			continue
		}
		entry := componentStatusEntry(&cr.Status.Components, cs.Name)
		changed = pruneStatusConditions(entry, cs.Conditions) || changed
		for _, cond := range cs.Conditions {
			cond.ObservedGeneration = cr.Generation
			if meta.SetStatusCondition(&entry.Conditions, cond) {
				changed = true
			}
		}
	}

	return changed
}

// pruneStatusConditions drops conditions from entry whose type is not present
// in reported, so a condition type a component stopped reporting does not
// linger with a stale observedGeneration. It reports whether anything was
// dropped.
func pruneStatusConditions(entry *hyperfleetv1alpha1.ComponentStatus, reported []metav1.Condition) bool {
	pruned := false
	kept := make([]metav1.Condition, 0, len(entry.Conditions))
	for _, existing := range entry.Conditions {
		if slices.ContainsFunc(reported, func(c metav1.Condition) bool { return c.Type == existing.Type }) {
			kept = append(kept, existing)
		} else {
			pruned = true
		}
	}
	if pruned {
		entry.Conditions = kept
	}
	return pruned
}

// findComponentStatus returns the status.components entry with the given name,
// or nil when none exists. Read-only, unlike componentStatusEntry: it never
// appends, so the returned pointer stays valid regardless of later mutations.
func findComponentStatus(components []hyperfleetv1alpha1.ComponentStatus, name string) *hyperfleetv1alpha1.ComponentStatus {
	for i := range components {
		if components[i].Name == name {
			return &components[i]
		}
	}
	return nil
}

// componentStatusEntry returns a pointer to the status.components entry with
// the given name, appending a new one when none exists yet (appended, not
// prepended, so a stable component set renders a stable list). The pointer is
// invalidated by any subsequent append through components, so complete every
// write through it before calling again.
func componentStatusEntry(components *[]hyperfleetv1alpha1.ComponentStatus, name string) *hyperfleetv1alpha1.ComponentStatus {
	if entry := findComponentStatus(*components, name); entry != nil {
		return entry
	}
	*components = append(*components, hyperfleetv1alpha1.ComponentStatus{Name: name})
	return &(*components)[len(*components)-1]
}

// rollupCondition ANDs every component condition of the given type: True only
// when every reported condition of that type is True. The first non-True
// condition's reason/message is surfaced. Absent any component conditions of
// this type, it reports True with the supplied default reason/message
// (vacuously true — there is nothing to be unavailable).
func rollupCondition(condType string, componentConditions []metav1.Condition, trueReason, trueMessage string) metav1.Condition {
	for _, c := range componentConditions {
		if c.Type == condType && c.Status != metav1.ConditionTrue {
			return metav1.Condition{
				Type:    condType,
				Status:  c.Status,
				Reason:  c.Reason,
				Message: c.Message,
			}
		}
	}
	return metav1.Condition{
		Type:    condType,
		Status:  metav1.ConditionTrue,
		Reason:  trueReason,
		Message: trueMessage,
	}
}

// rollupProgressing ORs every component's Progressing condition: True if any
// component reports Progressing=True.
func rollupProgressing(componentConditions []metav1.Condition) metav1.Condition {
	for _, c := range componentConditions {
		if c.Type == hyperfleetv1alpha1.ConditionProgressing && c.Status == metav1.ConditionTrue {
			return metav1.Condition{
				Type:    hyperfleetv1alpha1.ConditionProgressing,
				Status:  metav1.ConditionTrue,
				Reason:  c.Reason,
				Message: c.Message,
			}
		}
	}
	return metav1.Condition{
		Type:    hyperfleetv1alpha1.ConditionProgressing,
		Status:  metav1.ConditionFalse,
		Reason:  hyperfleetv1alpha1.ReasonRolloutComplete,
		Message: "all components have completed rollout",
	}
}

// degradedCondition reports Degraded=True when one or more referenced Secrets
// are missing or the most recent reconcile failed; a missing Secret takes
// precedence in its message when both are true, since it is usually the root
// cause of a downstream reconcile error (e.g. a mount failure). The
// reconcile-error branch deliberately does NOT echo reconcileErr.Error() into
// the Message: status.conditions is readable by any principal with get/list on
// this cluster-scoped CRD, and wrapped errors on the JWKS-discovery path can
// contain internal network details (see blockDiscoveryDial in
// hyperfleetconfig_rollout.go, which names the disallowed resolved IP in its
// error text). The caller logs the real error; only a safe, static summary is
// published here.
func degradedCondition(missingSecrets []string, reconcileErr error) metav1.Condition {
	switch {
	case len(missingSecrets) > 0:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonReferencedSecretMissing,
			Message: "referenced secret(s) missing in the operator's namespace: " + strings.Join(missingSecrets, ", "),
		}
	case reconcileErr != nil:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonReconcileError,
			Message: "the last reconcile failed; see the operator's logs for details",
		}
	default:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionDegraded,
			Status:  metav1.ConditionFalse,
			Reason:  hyperfleetv1alpha1.ReasonAsExpected,
			Message: "no failure detected",
		}
	}
}

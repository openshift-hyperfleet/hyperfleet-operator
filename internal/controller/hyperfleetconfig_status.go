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
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

// aggregateStatus rolls up each component's reported Conditions plus the
// controller-level Degraded signal (a missing referenced Secret, or a
// reconcile error) into the CR's top-level status.conditions, and sets
// status.observedGeneration. It reports whether anything actually changed, so
// Reconcile can skip a no-op Status().Update() call.
//
// Available is the AND of every component's Available condition.
// Progressing is the OR of every component's Progressing condition. Degraded is driven only
// by controller-level signals, not by component health, keeping the "component" boundary
// limited to what it can observe about its own operands (see internal/bundle.Component).
//
// componentsCollected must be true only when every component's Conditions()
// was actually gathered this reconcile (the render/apply/Conditions loop ran
// to completion without error). When false — Reconcile aborted early, e.g. a
// failed OIDC discovery or a failed apply before any component reported in —
// Available/Progressing are deliberately left untouched rather than defaulted
// to a fabricated healthy value: with zero fresh data, publishing "True" would
// contradict the Degraded=True this same reconcile is about to report, and
// would misrepresent an operand whose real state was never checked. Degraded
// is always evaluated regardless, since a reconcile error is itself the signal.
func aggregateStatus(cr *hyperfleetv1alpha1.HyperFleetConfig, componentConditions []metav1.Condition, componentsCollected bool, missingSecrets []string, reconcileErr error) bool {
	changed := false

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

	if cr.Status.ObservedGeneration != cr.Generation {
		cr.Status.ObservedGeneration = cr.Generation
		changed = true
	}

	return changed
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

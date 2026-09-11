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
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

func condition(condType string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: "test"}
}

func conditionByType(cr *hyperfleetv1alpha1.HyperFleetConfig, condType string) metav1.Condition {
	for _, c := range cr.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	return metav1.Condition{}
}

func TestAggregateStatusNoBumpOnUnrelatedReconcile(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	componentConditions := []metav1.Condition{
		condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
		condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete),
	}

	changed := aggregateStatus(cr, componentConditions, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "first write must report a change")
	firstTransition := conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).LastTransitionTime

	// Second reconcile: Generation moved (an unrelated spec/annotation edit) but
	// component health did not — observedGeneration must still advance
	// (changed=true) while LastTransitionTime must stay byte-identical (AC#5).
	cr.Generation = 2
	changed = aggregateStatus(cr, componentConditions, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "observedGeneration must still advance even though health is unchanged")
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).LastTransitionTime).To(Equal(firstTransition))
	g.Expect(cr.Status.ObservedGeneration).To(Equal(int64(2)))

	// A third reconcile with nothing changed at all (same generation, same
	// health) must report no change whatsoever.
	changed = aggregateStatus(cr, componentConditions, true, nil, nil)
	g.Expect(changed).To(BeFalse(), "a fully unrelated no-op reconcile must not report a change")
}

func TestAggregateStatusAvailableIsAndOverComponents(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	componentConditions := []metav1.Condition{
		condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
		condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
		condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonDeploymentNotReady),
	}

	aggregateStatus(cr, componentConditions, true, nil, nil)

	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentNotReady))
}

func TestAggregateStatusProgressingIsOrOverComponents(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	componentConditions := []metav1.Condition{
		condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete),
		condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete),
		condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonRolloutInProgress),
	}

	aggregateStatus(cr, componentConditions, true, nil, nil)

	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
}

func TestAggregateStatusDegradedPrecedence(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1

	// missingSecrets and a reconcile error both set: missingSecrets takes
	// precedence in the surfaced reason (it is usually the root cause).
	aggregateStatus(cr, nil, false, []string{"database"}, errors.New("apply component \"api\": boom"))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonReferencedSecretMissing))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Message).To(ContainSubstring("database"))

	// Only a reconcile error: ReconcileError, with a safe static message (never
	// the raw wrapped error text — see degradedCondition's doc comment).
	cr2 := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr2.Generation = 1
	aggregateStatus(cr2, nil, false, nil, errors.New("apply component \"api\": boom"))
	g.Expect(conditionByType(cr2, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(conditionByType(cr2, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonReconcileError))
	g.Expect(conditionByType(cr2, hyperfleetv1alpha1.ConditionDegraded).Message).NotTo(ContainSubstring("boom"))

	// Neither: AsExpected/False.
	cr3 := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr3.Generation = 1
	aggregateStatus(cr3, nil, false, nil, nil)
	g.Expect(conditionByType(cr3, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(conditionByType(cr3, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonAsExpected))
}

func TestAggregateStatusSetsObservedGeneration(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 3

	// componentsCollected=true with zero component conditions models a bundle
	// that genuinely has no components — the vacuous-true default is correct
	// here, unlike the componentsCollected=false case tested below.
	changed := aggregateStatus(cr, nil, true, nil, nil)
	g.Expect(changed).To(BeTrue())
	g.Expect(cr.Status.ObservedGeneration).To(Equal(int64(3)))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutComplete))
	for _, c := range cr.Status.Conditions {
		g.Expect(c.ObservedGeneration).To(Equal(int64(3)))
	}

	// A later reconcile at the same generation, same health: no change.
	changed = aggregateStatus(cr, nil, true, nil, nil)
	g.Expect(changed).To(BeFalse())
}

// TestAggregateStatusComponentsNotCollectedLeavesAvailableProgressingUnset
// covers the bug this story shipped and fixed: when Reconcile aborts before
// any component reports health (componentsCollected=false), Available and
// Progressing must NOT be fabricated as healthy — only Degraded is written.
func TestAggregateStatusComponentsNotCollectedLeavesAvailableProgressingUnset(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1

	changed := aggregateStatus(cr, nil, false, nil, errors.New("resolve JWKS URL: boom"))
	g.Expect(changed).To(BeTrue(), "Degraded must still be written")

	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonReconcileError))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable)).To(Equal(metav1.Condition{}),
		"Available must not be fabricated when components were never collected")
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing)).To(Equal(metav1.Condition{}),
		"Progressing must not be fabricated when components were never collected")
}

// TestAggregateStatusComponentsNotCollectedPreservesPriorAvailable covers the
// same fix from the other direction: a transient failure on a LATER reconcile
// (after a prior successful one already recorded real health) must not
// overwrite that last-known-good Available/Progressing with a guess.
func TestAggregateStatusComponentsNotCollectedPreservesPriorAvailable(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1

	// A prior, fully successful reconcile recorded real health.
	componentConditions := []metav1.Condition{
		condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
		condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete),
	}
	aggregateStatus(cr, componentConditions, true, nil, nil)
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))

	// A later reconcile fails before reaching any component (e.g. a transient
	// OIDC discovery blip): Available must remain exactly as last recorded.
	cr.Generation = 2
	aggregateStatus(cr, nil, false, nil, errors.New("resolve JWKS URL: boom"))

	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue),
		"a failed reconcile that never reached the components must not change the last-known Available value")
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
}

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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	apicomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
	sentinelcomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/sentinel"
)

const (
	// Component names used across these tests, derived from the production
	// constants so a rename in a component package fails here at compile time
	// rather than silently testing the wrong name.
	apiComponentName      = apicomponent.ComponentName
	sentinelClustersName  = sentinelcomponent.ComponentName + "-clusters"
	sentinelNodepoolsName = sentinelcomponent.ComponentName + "-nodepools"
	testMessage           = "test status condition message"
)

func condition(condType string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: testMessage}
}

func conditionByType(cr *hyperfleetv1alpha1.HyperFleetConfig, condType string) metav1.Condition {
	for _, c := range cr.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	return metav1.Condition{}
}

// newComponentStatus wraps conditions under a component name, mirroring how
// Reconcile collects them per component. Named differently from the
// ComponentStatus type it builds so the two stay visually distinct.
func newComponentStatus(name string, conds ...metav1.Condition) hyperfleetv1alpha1.ComponentStatus {
	return hyperfleetv1alpha1.ComponentStatus{Name: name, Conditions: conds}
}

// componentEntry returns the status.components entry for a component name, or
// nil when the component has no entry. It delegates to the production lookup;
// TestAggregateStatusPerComponentEntries additionally asserts the full
// components slice structurally, so the lookup itself is verified
// independently of this helper.
func componentEntry(cr *hyperfleetv1alpha1.HyperFleetConfig, name string) *hyperfleetv1alpha1.ComponentStatus {
	return findComponentStatus(cr.Status.Components, name)
}

// zeroTransitionTimes returns a copy of cs with every condition's
// lastTransitionTime zeroed, so a structurally-expected value can be compared
// against reconcile output without knowing the clock SetStatusCondition used.
func zeroTransitionTimes(cs []hyperfleetv1alpha1.ComponentStatus) []hyperfleetv1alpha1.ComponentStatus {
	out := make([]hyperfleetv1alpha1.ComponentStatus, len(cs))
	for i, entry := range cs {
		out[i] = entry
		conds := make([]metav1.Condition, len(entry.Conditions))
		copy(conds, entry.Conditions)
		for j := range conds {
			conds[j].LastTransitionTime = metav1.Time{}
		}
		out[i].Conditions = conds
	}
	return out
}

// healthyComponents is the per-component input of a fully successful
// reconcile with the API and one Sentinel both healthy.
//
// seedHealthyComponents applies one fully successful healthy reconcile to a
// fresh CR at generation 1 — the shared setup of the per-component tests.
// Tests that need a later generation set cr.Generation on the returned CR
// before their next aggregateStatus call.
func seedHealthyComponents(t *testing.T) *hyperfleetv1alpha1.HyperFleetConfig {
	t.Helper()
	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	aggregateStatus(cr, healthyComponents(), true, nil, nil)
	return cr
}

func healthyComponents() []hyperfleetv1alpha1.ComponentStatus {
	return []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
		newComponentStatus(sentinelClustersName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
	}
}

func TestAggregateStatusNoBumpOnUnrelatedReconcile(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	componentStatuses := healthyComponents()

	changed := aggregateStatus(cr, componentStatuses, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "first write must report a change")
	firstTransition := conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).LastTransitionTime
	apiEntry := componentEntry(cr, apiComponentName)
	g.Expect(apiEntry).NotTo(BeNil())
	g.Expect(apiEntry.Conditions).To(HaveLen(2))
	firstComponentTransition := apiEntry.Conditions[0].LastTransitionTime

	// Second reconcile: Generation moved (an unrelated spec/annotation edit) but
	// component health did not — observedGeneration must still advance
	// (changed=true) while LastTransitionTime must stay byte-identical (AC#5).
	cr.Generation = 2
	changed = aggregateStatus(cr, componentStatuses, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "observedGeneration must still advance even though health is unchanged")
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).LastTransitionTime).To(Equal(firstTransition))
	g.Expect(cr.Status.ObservedGeneration).To(Equal(int64(2)))

	// A third reconcile with nothing changed at all (same generation, same
	// health) must report no change whatsoever.
	changed = aggregateStatus(cr, componentStatuses, true, nil, nil)
	g.Expect(changed).To(BeFalse(), "a fully unrelated no-op reconcile must not report a change")
	noOpEntry := componentEntry(cr, apiComponentName)
	g.Expect(noOpEntry).NotTo(BeNil())
	g.Expect(noOpEntry.Conditions[0].LastTransitionTime).To(Equal(firstComponentTransition),
		"per-component LastTransitionTime must be preserved across no-op reconciles")
}

func TestAggregateStatusAvailableIsAndOverComponents(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	componentStatuses := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName, condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable)),
		newComponentStatus(sentinelClustersName, condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable)),
		newComponentStatus(sentinelNodepoolsName, condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonDeploymentNotReady)),
	}

	aggregateStatus(cr, componentStatuses, true, nil, nil)

	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentNotReady))
}

func TestAggregateStatusProgressingIsOrOverComponents(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	componentStatuses := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName, condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
		newComponentStatus(sentinelClustersName, condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
		newComponentStatus(sentinelNodepoolsName, condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonRolloutInProgress)),
	}

	aggregateStatus(cr, componentStatuses, true, nil, nil)

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

	// componentsCollected=true with zero component statuses models a bundle
	// that genuinely has no components — the vacuous-true default is correct
	// here, unlike the componentsCollected=false case tested below.
	changed := aggregateStatus(cr, nil, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "the vacuous-true rollup conditions must be written for the first time")
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
	g.Expect(changed).To(BeFalse(), "a same-generation, same-health reconcile must be a no-op")
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
	g.Expect(cr.Status.ObservedGeneration).To(Equal(int64(0)),
		"top-level observedGeneration must not advance when component health was never collected")
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

	// A prior, fully successful reconcile recorded real health.
	cr := seedHealthyComponents(t)
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))

	// A later reconcile fails before reaching any component (e.g. a transient
	// OIDC discovery blip): Available must remain exactly as last recorded.
	cr.Generation = 2
	aggregateStatus(cr, nil, false, nil, errors.New("resolve JWKS URL: boom"))

	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue),
		"a failed reconcile that never reached the components must not change the last-known Available value")
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cr.Status.ObservedGeneration).To(Equal(int64(1)),
		"top-level observedGeneration must stay pinned to the last fully processed generation")

	// Nothing was observed, so the per-component entries must be retained
	// unchanged: mergeComponentStatuses must neither prune nor overwrite.
	g.Expect(cr.Status.Components).To(HaveLen(2),
		"an abort before any component reports must keep every entry")
	for _, entry := range cr.Status.Components {
		g.Expect(entry.Conditions).To(HaveLen(2),
			"entry %s must keep its full conditions", entry.Name)
		for _, c := range entry.Conditions {
			g.Expect(c.ObservedGeneration).To(Equal(int64(1)),
				"entry %s must not be re-stamped by an aborted reconcile", entry.Name)
		}
	}
}

// TestAggregateStatusPerComponentEntries (HYPERFLEET-1653) verifies that each
// observed component gets its own status.components entry with its own
// conditions, and that a Sentinel failure is visible only on the Sentinel
// entry — the API entry is not overwritten.
func TestAggregateStatusPerComponentEntries(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1

	componentStatuses := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
		newComponentStatus(sentinelClustersName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonDeploymentUnavailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonRolloutInProgress)),
	}

	changed := aggregateStatus(cr, componentStatuses, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "the first per-component write must report a change")
	g.Expect(cr.Status.Components).To(HaveLen(2))

	apiEntry := componentEntry(cr, apiComponentName)
	g.Expect(apiEntry).NotTo(BeNil())
	g.Expect(apiEntry.Conditions).To(HaveLen(2))
	apiAvailable := meta.FindStatusCondition(apiEntry.Conditions, hyperfleetv1alpha1.ConditionAvailable)
	g.Expect(apiAvailable).NotTo(BeNil())
	g.Expect(apiAvailable.Status).To(Equal(metav1.ConditionTrue), "the API itself is healthy")
	sentinelEntry := componentEntry(cr, sentinelClustersName)
	g.Expect(sentinelEntry).NotTo(BeNil())
	g.Expect(sentinelEntry.Conditions).To(HaveLen(2))
	sentinelAvailable := meta.FindStatusCondition(sentinelEntry.Conditions, hyperfleetv1alpha1.ConditionAvailable)
	g.Expect(sentinelAvailable).NotTo(BeNil())
	g.Expect(sentinelAvailable.Status).To(Equal(metav1.ConditionFalse), "the Sentinel entry carries its own failure")

	// The rollup still reflects the worst component, while the API's own entry
	// records its individual health — a partner can tell which component is
	// unavailable without inspecting Deployments.
	rollup := conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable)
	g.Expect(rollup.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(rollup.Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))

	// Every per-component condition is stamped with the evaluated generation.
	for _, entry := range cr.Status.Components {
		for _, c := range entry.Conditions {
			g.Expect(c.ObservedGeneration).To(Equal(int64(1)))
		}
	}

	// Structural check of the FULL slice, independent of the componentEntry
	// helper (which mirrors the production name lookup): the entries must be
	// exactly the observed conditions with observedGeneration stamped, in
	// order. lastTransitionTime is zeroed on both sides since SetStatusCondition
	// fills it with the reconcile-time clock.
	g.Expect(zeroTransitionTimes(cr.Status.Components)).To(Equal([]hyperfleetv1alpha1.ComponentStatus{
		{
			Name: apiComponentName,
			Conditions: []metav1.Condition{
				{Type: hyperfleetv1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: hyperfleetv1alpha1.ReasonDeploymentAvailable, Message: testMessage, ObservedGeneration: 1},
				{Type: hyperfleetv1alpha1.ConditionProgressing, Status: metav1.ConditionFalse, Reason: hyperfleetv1alpha1.ReasonRolloutComplete, Message: testMessage, ObservedGeneration: 1},
			},
		},
		{
			Name: sentinelClustersName,
			Conditions: []metav1.Condition{
				{Type: hyperfleetv1alpha1.ConditionAvailable, Status: metav1.ConditionFalse, Reason: hyperfleetv1alpha1.ReasonDeploymentUnavailable, Message: testMessage, ObservedGeneration: 1},
				{Type: hyperfleetv1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: hyperfleetv1alpha1.ReasonRolloutInProgress, Message: testMessage, ObservedGeneration: 1},
			},
		},
	}))
}

// TestAggregateStatusPerComponentNoOp verifies the no-op contract on
// status.components: a reconcile with unchanged per-component values preserves
// lastTransitionTime and reports no change.
func TestAggregateStatusPerComponentNoOp(t *testing.T) {
	g := NewWithT(t)

	cr := seedHealthyComponents(t)
	firstEntry := componentEntry(cr, apiComponentName)
	g.Expect(firstEntry).NotTo(BeNil())
	g.Expect(firstEntry.Conditions).To(HaveLen(2))
	// Deep-copy the conditions by value: copying only the ComponentStatus
	// struct would leave Conditions aliased to the slice the second
	// aggregateStatus call can mutate in place, making the no-op comparison
	// below vacuous.
	firstConditions := make([]metav1.Condition, len(firstEntry.Conditions))
	copy(firstConditions, firstEntry.Conditions)
	firstTransition := firstConditions[0].LastTransitionTime

	changed := aggregateStatus(cr, healthyComponents(), true, nil, nil)
	g.Expect(changed).To(BeFalse(), "an unchanged per-component report must not report a change")
	g.Expect(cr.Status.Components).To(HaveLen(2))
	second := componentEntry(cr, apiComponentName)
	g.Expect(second).NotTo(BeNil())
	g.Expect(second.Conditions).To(HaveLen(2))
	g.Expect(second.Conditions[0].LastTransitionTime).To(Equal(firstTransition),
		"an unchanged per-component report must preserve lastTransitionTime")
	g.Expect(second.Conditions).To(Equal(firstConditions),
		"an unchanged per-component report must leave every condition value untouched")
}

// TestAggregateStatusPerComponentTransition covers a per-component condition
// status transition through mergeComponentStatuses: flipping a component's
// Available True→False between reconciles reports a change, updates the value
// with the new reason and an advanced lastTransitionTime, and preserves the
// entry's identity and position.
func TestAggregateStatusPerComponentTransition(t *testing.T) {
	g := NewWithT(t)

	cr := seedHealthyComponents(t)
	before := componentEntry(cr, sentinelClustersName)
	g.Expect(before).NotTo(BeNil())
	beforeAvailable := meta.FindStatusCondition(before.Conditions, hyperfleetv1alpha1.ConditionAvailable)
	g.Expect(beforeAvailable).NotTo(BeNil())
	g.Expect(beforeAvailable.Status).To(Equal(metav1.ConditionTrue))
	// Copy the timestamp by value: beforeAvailable points into the entry that
	// the second aggregateStatus call mutates in place.
	beforeTransition := beforeAvailable.LastTransitionTime

	// The Sentinel goes down while the API stays healthy: its entry must flip,
	// advance lastTransitionTime, and remain the same entry.
	cr.Generation = 2
	observed := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
		newComponentStatus(sentinelClustersName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonDeploymentUnavailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
	}
	changed := aggregateStatus(cr, observed, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "a status transition must report a change")

	after := componentEntry(cr, sentinelClustersName)
	g.Expect(after).NotTo(BeNil())
	g.Expect(after.Conditions).To(HaveLen(2))
	afterAvailable := meta.FindStatusCondition(after.Conditions, hyperfleetv1alpha1.ConditionAvailable)
	g.Expect(afterAvailable).NotTo(BeNil())
	g.Expect(afterAvailable.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(afterAvailable.Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
	g.Expect(afterAvailable.ObservedGeneration).To(Equal(int64(2)))
	g.Expect(afterAvailable.LastTransitionTime.After(beforeTransition.Time)).To(BeTrue(),
		"a status transition must advance lastTransitionTime")
	g.Expect(cr.Status.Components[1].Name).To(Equal(sentinelClustersName),
		"the entry must keep its identity and position")
}

// TestAggregateStatusAbortedReconcileDoesNotFabricateComponentEntries covers
// the aborted-reconcile contract: a component whose Conditions() ran before a
// later component failed is recorded as freshly evaluated, while the failed
// component's prior entry is left untouched (not fabricated, not stamped with
// the new generation).
func TestAggregateStatusAbortedReconcileDoesNotFabricateComponentEntries(t *testing.T) {
	g := NewWithT(t)

	cr := seedHealthyComponents(t)
	g.Expect(cr.Status.Components).To(HaveLen(2))

	// Generation moves and the Sentinel's apply fails: only the API (already
	// processed) reports in, and the loop aborts before Sentinel.
	cr.Generation = 2
	observed := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
	}
	changed := aggregateStatus(cr, observed, false, nil, errors.New("apply component \"sentinel-clusters\": boom"))
	g.Expect(changed).To(BeTrue(), "the freshly observed API entry and Degraded must be written")

	apiEntry := componentEntry(cr, apiComponentName)
	g.Expect(apiEntry).NotTo(BeNil())
	for _, c := range apiEntry.Conditions {
		g.Expect(c.ObservedGeneration).To(Equal(int64(2)),
			"the API was observed at generation 2 and its entry must say so")
	}

	sentinelEntry := componentEntry(cr, sentinelClustersName)
	g.Expect(sentinelEntry).NotTo(BeNil(), "the prior Sentinel entry must be retained")
	for _, c := range sentinelEntry.Conditions {
		g.Expect(c.ObservedGeneration).To(Equal(int64(1)),
			"the unobserved Sentinel entry must not be marked as freshly evaluated")
	}
	g.Expect(cr.Status.ObservedGeneration).To(Equal(int64(1)))
}

// TestAggregateStatusPrunesRemovedComponents verifies that a fully successful
// reconcile drops status.components entries for components no longer in the
// observed set (e.g. a component removed from the bundle across an operator
// upgrade), while an aborted reconcile keeps them.
func TestAggregateStatusPrunesRemovedComponents(t *testing.T) {
	g := NewWithT(t)

	cr := seedHealthyComponents(t)
	g.Expect(cr.Status.Components).To(HaveLen(2))

	// A later full reconcile observes only the API (Sentinel removed from the
	// bundle): the stale Sentinel entry is pruned.
	observed := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName, condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable)),
	}
	changed := aggregateStatus(cr, observed, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "pruning a removed component's entry must report a change")
	g.Expect(cr.Status.Components).To(HaveLen(1))
	g.Expect(cr.Status.Components[0].Name).To(Equal(apiComponentName))

	// The same observed set on an aborted reconcile (componentsCollected=false)
	// must not prune: re-seed the sentinel entry first so the branch is
	// actually exercised — the unobserved remainder of the set is unknown.
	cr.Generation = 2
	aggregateStatus(cr, healthyComponents(), true, nil, nil)
	changed = aggregateStatus(cr, observed, false, nil, errors.New("boom"))
	g.Expect(changed).To(BeTrue(), "the freshly observed API entry and Degraded must be written")
	g.Expect(cr.Status.Components).To(HaveLen(2), "an aborted reconcile must not prune unobserved components")
	g.Expect(componentEntry(cr, sentinelClustersName)).NotTo(BeNil(),
		"the unobserved sentinel entry must survive the aborted reconcile")
}

// TestAggregateStatusPrunesStoppedConditionTypes verifies that a condition
// type an observed component stops reporting is dropped from its entry, so it
// cannot linger with a stale observedGeneration — while an empty condition
// report does not wipe the entry.
func TestAggregateStatusPrunesStoppedConditionTypes(t *testing.T) {
	g := NewWithT(t)

	cr := seedHealthyComponents(t)
	apiEntry := componentEntry(cr, apiComponentName)
	g.Expect(apiEntry).NotTo(BeNil())
	g.Expect(apiEntry.Conditions).To(HaveLen(2))

	// A later reconcile where the API stops reporting Progressing: the stale
	// Progressing condition is pruned rather than lingering next to the
	// freshly stamped Available one.
	cr.Generation = 2
	observed := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName, condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable)),
	}
	changed := aggregateStatus(cr, observed, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "pruning the stopped Progressing condition must report a change")

	apiEntry = componentEntry(cr, apiComponentName)
	g.Expect(apiEntry).NotTo(BeNil())
	g.Expect(apiEntry.Conditions).To(HaveLen(1))
	g.Expect(apiEntry.Conditions[0].Type).To(Equal(hyperfleetv1alpha1.ConditionAvailable))
	g.Expect(apiEntry.Conditions[0].ObservedGeneration).To(Equal(int64(2)))

	// An empty condition report must NOT wipe the entry: that is more
	// plausibly a contract violation than an intentional "report nothing".
	empty := []hyperfleetv1alpha1.ComponentStatus{{Name: apiComponentName}}
	changed = aggregateStatus(cr, empty, true, nil, nil)
	g.Expect(changed).To(BeFalse(), "an empty report with unchanged health is a no-op")
	apiEntry = componentEntry(cr, apiComponentName)
	g.Expect(apiEntry).NotTo(BeNil())
	g.Expect(apiEntry.Conditions).To(HaveLen(1),
		"an empty condition report must not wipe the entry's last-known conditions")
}

// TestAggregateStatusEmptyReportDoesNotCreateEntry covers the other half of the
// empty-report contract: a component that reports no conditions and has no
// prior entry must not gain one. Before this was fixed, mergeComponentStatuses
// appended a conditions-less entry that nothing marked as a change, so an
// otherwise no-op reconcile skipped Status().Update and silently dropped it.
func TestAggregateStatusEmptyReportDoesNotCreateEntry(t *testing.T) {
	g := NewWithT(t)

	cr := seedHealthyComponents(t)
	g.Expect(cr.Status.Components).To(HaveLen(2))

	// A brand-new component (never seen before) reports an empty condition set
	// on a fully successful, otherwise-unchanged reconcile: no entry may be
	// created, and the append must not masquerade as a no-op change.
	observed := append(healthyComponents(), hyperfleetv1alpha1.ComponentStatus{Name: "brand-new"})
	changed := aggregateStatus(cr, observed, true, nil, nil)
	g.Expect(changed).To(BeFalse(), "an empty report from a new component must not report a change")
	g.Expect(cr.Status.Components).To(HaveLen(2),
		"a component that has never reported conditions must not gain an entry")
	g.Expect(componentEntry(cr, "brand-new")).To(BeNil())
}

// TestAggregateStatusEmptyReportKeepsRollupConsistent covers the interaction
// between the empty-report no-wipe guard and the rollup: a component whose
// retained entry says Available=False must keep the top-level Available False
// even when it reports no conditions, so status.conditions and
// status.components can never contradict each other.
func TestAggregateStatusEmptyReportKeepsRollupConsistent(t *testing.T) {
	g := NewWithT(t)

	// Seed both components, with the Sentinel unavailable.
	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Generation = 1
	seed := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
		newComponentStatus(sentinelClustersName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonDeploymentUnavailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
	}
	aggregateStatus(cr, seed, true, nil, nil)
	g.Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse))

	// The Sentinel reports an empty condition set while the API stays healthy:
	// the rollup must fall back to the Sentinel's retained Available=False
	// rather than computing Available=True over the API alone.
	cr.Generation = 2
	observed := []hyperfleetv1alpha1.ComponentStatus{
		newComponentStatus(apiComponentName,
			condition(hyperfleetv1alpha1.ConditionAvailable, metav1.ConditionTrue, hyperfleetv1alpha1.ReasonDeploymentAvailable),
			condition(hyperfleetv1alpha1.ConditionProgressing, metav1.ConditionFalse, hyperfleetv1alpha1.ReasonRolloutComplete)),
		{Name: sentinelClustersName},
	}
	changed := aggregateStatus(cr, observed, true, nil, nil)
	g.Expect(changed).To(BeTrue(), "the generation bump must advance observedGeneration")

	available := conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable)
	g.Expect(available.Status).To(Equal(metav1.ConditionFalse),
		"an empty report must not let the rollup contradict the retained component entry")
	g.Expect(available.Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))

	sentinelEntry := componentEntry(cr, sentinelClustersName)
	g.Expect(sentinelEntry).NotTo(BeNil())
	g.Expect(sentinelEntry.Conditions).To(HaveLen(2),
		"the empty-report no-wipe guard must keep the retained conditions")
}

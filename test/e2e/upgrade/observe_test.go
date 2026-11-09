// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestInitialUnavailabilityCannotProveContinuousAvailability(t *testing.T) {
	now := time.Now()
	observations := []observation{
		sample(now, metav1.ConditionFalse, metav1.ConditionFalse),
		sample(now.Add(time.Second), metav1.ConditionTrue, metav1.ConditionTrue),
		sample(now.Add(2*time.Second), metav1.ConditionTrue, metav1.ConditionFalse),
	}
	if validateHistory(observations, 0) == nil {
		t.Fatal("unavailable baseline passed")
	}
}

func TestInventoryRejectsOmittedSentinelsAndForeignDeployments(t *testing.T) {
	expected := testExpectations()
	api := readyDeployment()
	api.Name = expected.api().Deployment
	api.OwnerReferences = []metav1.OwnerReference{{UID: "hfc-owner"}}
	if err := validateInventory([]appsv1.Deployment{api}, expected, "hfc-owner"); err != nil {
		t.Fatal(err)
	}
	sentinel := *api.DeepCopy()
	sentinel.Name = "hyperfleet-sentinel-clusters"
	if validateInventory([]appsv1.Deployment{api, sentinel}, expected, "hfc-owner") == nil {
		t.Fatal("omitted operator-owned Sentinel passed")
	}
	api.OwnerReferences = nil
	if validateInventory([]appsv1.Deployment{api}, expected, "hfc-owner") == nil {
		t.Fatal("foreign deployment satisfied the expected inventory")
	}
}

func sample(at time.Time, available, progressing metav1.ConditionStatus) observation {
	return observation{At: at, UID: "hfc-uid", Generation: 1, ResourceVersion: "1", Status: hfcStatus{
		ObservedGeneration: 1, Conditions: []metav1.Condition{
			{Type: conditionAvailable, Status: available, ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(at)},
			{Type: "Progressing", Status: progressing, ObservedGeneration: 1},
			{Type: "Degraded", Status: metav1.ConditionFalse, ObservedGeneration: 1}}}}
}

func TestConditionHistory(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		change  func([]observation) []observation
		gap     time.Duration
		invalid bool
	}{
		{"complete", func(o []observation) []observation { return o }, 0, false},
		{"missed progress", func(o []observation) []observation {
			o[1].Status.Conditions[1].Status = metav1.ConditionFalse
			return o
		}, 0, true},
		{"availability regression", func(o []observation) []observation {
			o[1].Status.Conditions[0].Status = metav1.ConditionFalse
			return o
		}, 0, true},
		{"bounded gap", func(o []observation) []observation {
			o[1].Status.Conditions[0].Status = metav1.ConditionFalse
			return o
		}, 2 * time.Second, false},
		{"excessive gap", func(o []observation) []observation {
			o[1].Status.Conditions[0].Status = metav1.ConditionFalse
			return o
		}, time.Millisecond, true},
		{"replacement", func(o []observation) []observation { o[1].UID = testOtherIdentity; return o }, 0, true},
		{"spec change", func(o []observation) []observation { o[1].Generation = 2; return o }, 0, true},
		{"not finished", func(o []observation) []observation { return o[:2] }, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := []observation{sample(now, metav1.ConditionTrue, metav1.ConditionFalse),
				sample(now.Add(time.Second), metav1.ConditionTrue, metav1.ConditionTrue),
				sample(now.Add(2*time.Second), metav1.ConditionTrue, metav1.ConditionFalse)}
			err := validateHistory(tc.change(samples), tc.gap)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v; invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestStaleAndMissingComponentHealth(t *testing.T) {
	o := sample(time.Now(), metav1.ConditionTrue, metav1.ConditionFalse)
	expected := testExpectations()
	if !statusHealthy(o, expected) {
		t.Fatal("healthy older release was rejected")
	}
	expected.ComponentStatus = true
	if statusHealthy(o, expected) {
		t.Fatal("missing per-component status passed")
	}
	o.Status.Components = []componentStatus{{Name: apiComponentName,
		Conditions: append([]metav1.Condition{}, o.Status.Conditions[:2]...)}}
	if !statusHealthy(o, expected) {
		t.Fatal("healthy component rejected")
	}
	o.Status.Components[0].Conditions[0].ObservedGeneration = 0
	if statusHealthy(o, expected) {
		t.Fatal("stale component passed")
	}
}

func TestDeploymentConvergenceRequiresNewReplicas(t *testing.T) {
	dep := readyDeployment()
	if !deploymentReady(&dep) {
		t.Fatal("ready deployment rejected")
	}
	dep.Status.UpdatedReplicas = 0
	if deploymentReady(&dep) {
		t.Fatal("old ready pods masked failed new rollout")
	}
	dep = readyDeployment()
	dep.Status.Replicas = 2
	if deploymentReady(&dep) {
		t.Fatal("old replicas still present")
	}
	dep = readyDeployment()
	dep.Spec.Replicas = ptr.To[int32](0)
	if deploymentReady(&dep) {
		t.Fatal("scaled-to-zero deployment passed")
	}
}

func TestMigrationMustActuallyComplete(t *testing.T) {
	status := []corev1.ContainerStatus{{Name: testMigrationContainer, ImageID: "sha256:runtime",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	if !successfulInit(status, testMigrationContainer) {
		t.Fatal("successful migration rejected")
	}
	status[0].State.Terminated.ExitCode = 1
	if successfulInit(status, testMigrationContainer) {
		t.Fatal("failed migration accepted")
	}
	status[0].State.Terminated = nil
	if successfulInit(status, testMigrationContainer) {
		t.Fatal("unfinished migration accepted")
	}
}

func TestHashExpectationsAndIdentity(t *testing.T) {
	expected := testExpectations()
	before := map[string]workloadState{apiComponentName: {UID: "dep", Hash: "old"}}
	after := map[string]workloadState{apiComponentName: {UID: "dep", Hash: "new"}}
	if validateHashes(before, after, expected) == nil {
		t.Fatal("unexpected hash change passed")
	}
	expected.Components[0].HashChange = ptr.To(true)
	if err := validateHashes(before, after, expected); err != nil {
		t.Fatal(err)
	}
	after[apiComponentName] = workloadState{UID: "recreated", Hash: "new"}
	if validateHashes(before, after, expected) == nil {
		t.Fatal("recreated deployment passed")
	}
}

func TestOnlyAddedComponentsMayExplainAggregateUnavailability(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		existing, added         metav1.ConditionStatus
		includeAdded, wantError bool
	}{
		{"new sentinel starting", metav1.ConditionTrue, metav1.ConditionFalse, true, false},
		{"existing api unavailable", metav1.ConditionFalse, metav1.ConditionFalse, true, true},
		{"unattributed dip", metav1.ConditionTrue, metav1.ConditionTrue, true, true},
		{"missing new component evidence", metav1.ConditionTrue, metav1.ConditionFalse, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			observations := []observation{sample(now, metav1.ConditionTrue, metav1.ConditionFalse),
				sample(now.Add(time.Second), metav1.ConditionFalse, metav1.ConditionTrue),
				sample(now.Add(2*time.Second), metav1.ConditionTrue, metav1.ConditionFalse)}
			observations[1].Status.Components = []componentStatus{{Name: apiComponentName,
				Conditions: []metav1.Condition{{Type: conditionAvailable, Status: tc.existing, ObservedGeneration: 1}}}}
			if tc.includeAdded {
				observations[1].Status.Components = append(observations[1].Status.Components,
					componentStatus{Name: "sentinel", Conditions: []metav1.Condition{{Type: conditionAvailable,
						Status: tc.added, ObservedGeneration: 1}}})
			}
			s := Scenario{From: testExpectations(), To: testExpectations(), MaxUnavailable: "5s"}
			s.To.Components = append(s.To.Components, Component{Name: "sentinel"})
			if err := validateUpgradeHistory(observations, s); (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestMigrationCannotBeReplacedByRegularContainer(t *testing.T) {
	want := corev1.PodSpec{InitContainers: []corev1.Container{{Name: testMigrationContainer, Image: fieldImage}}}
	actual := corev1.PodSpec{Containers: want.InitContainers}
	if matchContainerImages(actual, want) == nil {
		t.Fatal("regular container incorrectly satisfied migration init container")
	}
}

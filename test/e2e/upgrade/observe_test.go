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

func TestInventoryRejectsOmittedSentinelsAndForeignDeployments(t *testing.T) {
	expected := testExpectations()
	api := readyDeployment()
	api.Name = expected.api().Deployment
	api.OwnerReferences = []metav1.OwnerReference{{UID: "hfc-owner"}}
	sentinel := *api.DeepCopy()
	sentinel.Name = "hyperfleet-sentinel-clusters"
	foreign := *api.DeepCopy()
	foreign.OwnerReferences = nil
	for _, tc := range []struct {
		name        string
		deployments []appsv1.Deployment
		wantError   bool
	}{
		{"complete", []appsv1.Deployment{api}, false},
		{"omitted sentinel", []appsv1.Deployment{api, sentinel}, true},
		{"foreign deployment", []appsv1.Deployment{foreign}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateInventory(tc.deployments, expected, "hfc-owner"); (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
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
		{"unavailable baseline", func(o []observation) []observation {
			o[0].Status.Conditions[0].Status = metav1.ConditionFalse
			return o
		}, 0, true},
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
	for _, tc := range []struct {
		name                                                            string
		componentStatus, includeComponent, stale, degraded, wantHealthy bool
	}{
		{"older release", false, false, false, false, true},
		{"missing status", true, false, false, false, false},
		{"healthy component", true, true, false, false, true},
		{"stale component", true, true, true, false, false},
		{"degraded HFC", false, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := sample(time.Now(), metav1.ConditionTrue, metav1.ConditionFalse)
			expected := testExpectations()
			expected.ComponentStatus = tc.componentStatus
			if tc.includeComponent {
				o.Status.Components = []componentStatus{{Name: apiComponentName,
					Conditions: append([]metav1.Condition{}, o.Status.Conditions[:2]...)}}
			}
			if tc.stale {
				o.Status.Components[0].Conditions[0].ObservedGeneration = 0
			}
			if tc.degraded {
				o.Status.Conditions[2].Status = metav1.ConditionTrue
			}
			if statusHealthy(o, expected) != tc.wantHealthy {
				t.Fatal("incorrect health decision")
			}
		})
	}
}

func TestDeploymentConvergenceRequiresNewReplicas(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*appsv1.Deployment)
		wantReady bool
	}{
		{fieldReady, func(*appsv1.Deployment) {}, true},
		{"old ready pods", func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 0 }, false},
		{"old replicas remain", func(d *appsv1.Deployment) { d.Status.Replicas = 2 }, false},
		{"scaled to zero", func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To[int32](0) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := readyDeployment()
			tc.change(&dep)
			if deploymentReady(&dep) != tc.wantReady {
				t.Fatal("incorrect rollout convergence decision")
			}
		})
	}
}

func TestMigrationMustActuallyComplete(t *testing.T) {
	for _, tc := range []struct {
		name        string
		terminated  *corev1.ContainerStateTerminated
		wantSuccess bool
	}{
		{"completed", &corev1.ContainerStateTerminated{ExitCode: 0}, true},
		{"failed", &corev1.ContainerStateTerminated{ExitCode: 1}, false},
		{"unfinished", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statuses := []corev1.ContainerStatus{{Name: testMigrationContainer, ImageID: "sha256:runtime",
				State: corev1.ContainerState{Terminated: tc.terminated}}}
			if successfulInit(statuses, testMigrationContainer) != tc.wantSuccess {
				t.Fatal("incorrect migration result")
			}
		})
	}
}

func TestHashExpectationsAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		hashChange, replacement, wantError bool
	}{
		{"unexpected hash", false, false, true},
		{"expected hash", true, false, false},
		{"replacement", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := testExpectations()
			expected.Components[0].HashChange = ptr.To(tc.hashChange)
			before := map[string]workloadState{apiComponentName: {UID: "dep", Hash: "old"}}
			next := workloadState{UID: "dep", Hash: "new"}
			if tc.replacement {
				next.UID = "recreated"
			}
			err := validateHashes(before, map[string]workloadState{apiComponentName: next}, expected)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
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
					componentStatus{Name: testSentinelName, Conditions: []metav1.Condition{{Type: conditionAvailable,
						Status: tc.added, ObservedGeneration: 1}}})
			}
			s := Scenario{From: testExpectations(), To: testExpectations(), MaxUnavailable: "5s"}
			s.To.Components = append(s.To.Components, Component{Name: testSentinelName})
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

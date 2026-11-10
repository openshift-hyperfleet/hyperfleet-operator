// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testInitialHash    = "initial-hash"
	testDatabaseSecret = "test-db"
)

func healthyAPIHarness(t *testing.T) (*harness, appsv1.Deployment) {
	t.Helper()
	cr := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", map[string]any{
		apiComponentName: map[string]any{"database": map[string]any{
			"secretRef": map[string]any{fieldName: testDatabaseSecret}}},
	})
	baseline := sample(time.Now(), metav1.ConditionTrue, metav1.ConditionFalse)
	cr.SetUID(baseline.UID)
	cr.SetGeneration(baseline.Generation)
	status, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&baseline.Status)
	if err != nil {
		t.Fatal(err)
	}
	cr.Object["status"] = status
	h := fakeHarness(t, cr)
	h.state.HFCUID = cr.GetUID()
	h.config.Scenario.To = testExpectations()
	h.config.Scenario.SettleDuration = "5s"
	h.pair = testPair(t)
	dep := readyDeployment()
	dep.Name, dep.Namespace, dep.UID = testAPIImageName, h.config.Namespace, "api-deployment"
	dep.OwnerReferences = []metav1.OwnerReference{{UID: cr.GetUID()}}
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": apiComponentName}}
	dep.Spec.Template.Annotations = map[string]string{hashAnnotation: testInitialHash}
	dep.Spec.Template.Spec.Containers = []corev1.Container{
		{Name: apiComponentName, Image: h.pair.To.Images[testAPIImageName]}}
	dep.Spec.Template.Spec.InitContainers = []corev1.Container{
		{Name: testMigrationContainer, Image: h.pair.To.Images[testAPIImageName]}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-pod", Namespace: h.config.Namespace,
		Labels: dep.Spec.Selector.MatchLabels}, Spec: dep.Spec.Template.Spec,
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: apiComponentName, ImageID: testRuntimeImage, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: testMigrationContainer, ImageID: testRuntimeImage,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testDatabaseSecret, Namespace: h.config.Namespace,
		Labels: map[string]string{runLabel: h.state.RunID}}}
	kube := h.clients.kube.(*kubefake.Clientset)
	for _, obj := range []runtime.Object{&dep, pod, secret} {
		if err = kube.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	return h, dep
}

func TestHashProbeRequiresChangedHashAtFinalConvergence(t *testing.T) {
	for _, tc := range []struct {
		name, finalHash  string
		wantError        bool
		unrelatedRollout bool
	}{
		{"reverted", testInitialHash, true, false},
		{"persisted", "final-hash", false, false},
		{"unrelated rollout", "final-hash", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dep := healthyAPIHarness(t)
			kube := h.clients.kube.(*kubefake.Clientset)
			sentinel := addHealthySentinel(t, h, dep)
			stage := 0
			kube.PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
				stage = 1
				return false, nil, nil
			})
			current := func() *appsv1.Deployment {
				got := dep.DeepCopy()
				switch stage {
				case 1:
					got.Generation = 3
					got.Spec.Template.Annotations[hashAnnotation] = "transient-hash"
					stage = 2
				case 2:
					got.Generation = 4
					got.Spec.Template.Annotations[hashAnnotation] = tc.finalHash
				}
				got.Status.ObservedGeneration = got.Generation
				return got
			}
			currentSentinel := func() *appsv1.Deployment {
				got := sentinel.DeepCopy()
				if stage == 2 && tc.unrelatedRollout {
					got.Spec.Template.Annotations[hashAnnotation] = "unexpected-sentinel-hash"
				}
				return got
			}
			kube.PrependReactor("get", deploymentResource.Resource, func(a clienttesting.Action) (bool, runtime.Object, error) {
				if a.(clienttesting.GetAction).GetName() == sentinel.Name {
					return true, currentSentinel(), nil
				}
				return true, current(), nil
			})
			kube.PrependReactor("list", deploymentResource.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, &appsv1.DeploymentList{Items: []appsv1.Deployment{*current(), *currentSentinel()}}, nil
			})
			err := h.checkHashTriggers(t.Context())
			if (err != nil) != tc.wantError {
				t.Fatalf("hash probe error=%v; wantError=%v", err, tc.wantError)
			}
			message := "did not persist a new API config hash"
			if tc.unrelatedRollout {
				message = "rolled unrelated component"
			}
			if tc.wantError && !strings.Contains(err.Error(), message) {
				t.Fatal("probe failed for an unrelated reason", err)
			}
		})
	}
}

func addHealthySentinel(t *testing.T, h *harness, api appsv1.Deployment) appsv1.Deployment {
	t.Helper()
	component := Component{Name: testSentinelName, Deployment: "test-sentinel", Container: testSentinelName,
		RelatedImage: "hyperfleet-sentinel"}
	h.config.Scenario.To.Components = append(h.config.Scenario.To.Components, component)
	image := testImage(testSentinelName, "c")
	h.pair.To.Images[component.RelatedImage] = image
	dep := *api.DeepCopy()
	dep.Name, dep.UID = component.Deployment, "sentinel-uid"
	dep.Spec.Selector.MatchLabels = map[string]string{"app": component.Name}
	dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: component.Container, Image: image}}
	dep.Spec.Template.Spec.InitContainers = nil
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sentinel-pod", Namespace: h.config.Namespace,
		Labels: dep.Spec.Selector.MatchLabels}, Spec: dep.Spec.Template.Spec,
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: component.Container, ImageID: testRuntimeImage, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	kube := h.clients.kube.(*kubefake.Clientset)
	for _, obj := range []runtime.Object{&dep, pod} {
		if err := kube.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	return dep
}

func TestStableWindowRejectsUnexpectedRollouts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*appsv1.Deployment)
	}{
		{"hash", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[hashAnnotation] = "unexpected-hash" }},
		{"generation", func(d *appsv1.Deployment) { d.Generation++ }},
		{"identity", func(d *appsv1.Deployment) { d.UID = "replaced-during-window" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dep := healthyAPIHarness(t)
			before := map[string]workloadState{apiComponentName: {UID: dep.UID, Generation: dep.Generation,
				Hash: dep.Spec.Template.Annotations[hashAnnotation]}}
			calls := 0
			h.clients.kube.(*kubefake.Clientset).PrependReactor("get", deploymentResource.Resource,
				func(clienttesting.Action) (bool, runtime.Object, error) {
					calls++
					got := dep.DeepCopy()
					if calls > 1 {
						tc.change(got)
					}
					return true, got, nil
				})
			if err := h.unchangedFor(t.Context(), before, time.Second); err == nil ||
				!strings.Contains(err.Error(), "unexpected rollout") {
				t.Fatal("missed rollout during window", err)
			}
		})
	}
}

func TestHealthUsesOneDeploymentSnapshot(t *testing.T) {
	h, _ := healthyAPIHarness(t)
	kube := h.clients.kube.(*kubefake.Clientset)
	kube.ClearActions()
	if _, err := h.healthy(t.Context(), h.pair.To, h.config.Scenario.To); err != nil {
		t.Fatal(err)
	}
	lists := 0
	for _, action := range kube.Actions() {
		if action.GetResource() != deploymentResource {
			continue
		}
		if action.GetVerb() != "list" {
			t.Fatal("health check fetched a Deployment outside its inventory snapshot")
		}
		lists++
	}
	if lists != 1 {
		t.Fatalf("expected one Deployment list, got %d", lists)
	}
}

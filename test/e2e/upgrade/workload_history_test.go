// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestWorkloadHistoryCatchesAnOutageDespiteHealthyFinalPods(t *testing.T) {
	for _, event := range []watch.EventType{watch.Modified, watch.Deleted} {
		t.Run(string(event), func(t *testing.T) {
			baseline := workloadObservation{Name: testAPIImageName, UID: "api-uid",
				Desired: 1, Available: 1, Event: watch.Added}
			unavailable := baseline
			unavailable.Available, unavailable.Event = 0, event
			before := map[string]workloadState{apiComponentName: {UID: baseline.UID}}
			if err := validateWorkloadAvailability([]workloadObservation{baseline}, testExpectations(), before); err != nil {
				t.Fatal(err)
			}
			if validateWorkloadAvailability([]workloadObservation{baseline, unavailable, baseline},
				testExpectations(), before) == nil {
				t.Fatal("intermediate outage passed because the final deployment recovered")
			}
		})
	}
}

func TestWorkloadHistoryOmitsCredentials(t *testing.T) {
	dep := readyDeployment()
	dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: "api", Image: fieldImage,
		Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "sensitive-fixture-password"}}}}
	dep.Annotations = map[string]string{"sensitive": "sensitive-fixture-password"}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&dep)
	if err != nil {
		t.Fatal(err)
	}
	o, err := observeWorkload(&unstructured.Unstructured{Object: obj}, deploymentResource, watch.Modified)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(o)
	if err != nil || strings.Contains(string(encoded), "sensitive-fixture-password") {
		t.Fatal("workload evidence leaked payload data", err)
	}
}

func TestWorkloadWatchResumesWithoutRelistingAndRejectsExpiry(t *testing.T) {
	h := fakeHarness(t)
	dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
	dyn.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{Object: map[string]any{
			"metadata": map[string]any{"resourceVersion": "10"}}}, nil
	})
	first, second := watch.NewRaceFreeFake(), watch.NewRaceFreeFake()
	versions := make(chan string, 2)
	calls := 0
	dyn.PrependWatchReactor("deployments", func(a clienttesting.Action) (bool, watch.Interface, error) {
		versions <- a.(clienttesting.WatchAction).GetWatchRestrictions().ResourceVersion
		calls++
		if calls == 1 {
			return true, first, nil
		}
		return true, second, nil
	})
	history, err := startWorkloadHistory(t.Context(), h.resource(deploymentResource), deploymentResource)
	if err != nil {
		t.Fatal(err)
	}
	defer history.stop()
	if got := <-versions; got != "10" {
		t.Fatalf("started at %s", got)
	}
	obj := object("apps/v1", "Deployment", "api", h.config.Namespace, map[string]any{})
	obj.SetResourceVersion("11")
	first.Modify(obj)
	if err = h.poll(t.Context(), "watch event", func(context.Context) (bool, error) {
		records, readErr := history.snapshot()
		return len(records) == 1, readErr
	}); err != nil {
		t.Fatal(err)
	}
	first.Stop()
	select {
	case got := <-versions:
		if got != "11" {
			t.Fatalf("resumed at %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no resumed watch")
	}
	second.Error(&metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired})
	select {
	case <-history.done:
	case <-time.After(time.Second):
		t.Fatal("expired history did not stop")
	}
	if _, err = history.snapshot(); err == nil {
		t.Fatal("expired workload history accepted")
	}
}

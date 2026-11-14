// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"strconv"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestCleanupHFCUsesBackgroundPropagation(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		t.Run(strconv.FormatBool(terminating), func(t *testing.T) {
			cr := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", nil)
			cr.SetUID("hfc-uid")
			if terminating {
				now := metav1.Now()
				cr.SetDeletionTimestamp(&now)
				cr.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
			}
			h := fakeHarness(t, cr)
			h.state.HFCUID = cr.GetUID()
			dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
			deleted := false
			dyn.PrependReactor(testDeleteVerb, hfcResource.Resource,
				func(action clienttesting.Action) (bool, runtime.Object, error) {
					options := action.(clienttesting.DeleteAction).GetDeleteOptions()
					if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationBackground {
						t.Fatal("HFC deletion can block on dependents recreated by the running operator")
					}
					if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != cr.GetUID() {
						t.Fatal("HFC deletion lost its UID precondition")
					}
					deleted = true
					return false, nil, nil
				})
			if err := h.cleanup(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !deleted || !h.state.Cleaned {
				t.Fatal("cleanup did not complete HFC deletion")
			}
			_, err := h.clients.kube.CoreV1().Namespaces().Get(t.Context(), h.state.Namespace, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("namespace still exists after cleanup: %v", err)
			}
		})
	}
}

func TestCleanupRetainsLeaseUntilResourcesDisappear(t *testing.T) {
	for _, blockedResource := range []string{hfcResource.Resource, "namespaces"} {
		t.Run(blockedResource, func(t *testing.T) {
			cr := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", nil)
			cr.SetUID("hfc-uid")
			cr.SetFinalizers([]string{"example.com/cleanup"})
			h := fakeHarness(t, cr)
			h.state.HFCUID = cr.GetUID()
			h.config.Timeout = 20 * time.Millisecond
			if err := h.saveState(); err != nil {
				t.Fatal(err)
			}
			// Model an accepted DELETE that has not finished finalization. Fake
			// clients do not implement Kubernetes finalizers or garbage collection.
			client := &h.clients.dynamic.(*dynamicfake.FakeDynamicClient).Fake
			if blockedResource == "namespaces" {
				client = &h.clients.kube.(*kubefake.Clientset).Fake
			}
			client.PrependReactor(testDeleteVerb, blockedResource,
				func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
			if err := h.cleanup(t.Context()); err == nil {
				t.Fatal("cleanup accepted an unfinished deletion")
			}
			if h.state.Cleaned || savedReceipt(t, h).Cleaned {
				t.Fatal("unfinished cleanup was marked complete")
			}
			if _, err := h.clients.kube.CoordinationV1().Leases(systemNamespace).
				Get(t.Context(), lockName, metav1.GetOptions{}); err != nil {
				t.Fatalf("run lock released before cleanup finished: %v", err)
			}
			for _, action := range h.clients.dynamic.(*dynamicfake.FakeDynamicClient).Actions() {
				if action.GetVerb() == "patch" || action.GetVerb() == "update" {
					t.Fatal("cleanup must not patch away custom finalizers")
				}
			}
		})
	}
}

func TestCleanupHFCRefusesReplacedUID(t *testing.T) {
	cr := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", nil)
	cr.SetUID("replacement-uid")
	h := fakeHarness(t, cr)
	h.state.HFCUID = "original-uid"
	if err := h.cleanupHFC(t.Context()); err == nil {
		t.Fatal("cleanup accepted a replacement HFC")
	}
	assertNoDeletes(t, h)
}

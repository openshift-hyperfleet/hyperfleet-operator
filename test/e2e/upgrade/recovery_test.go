// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testLeaseResource = "leases"

func savedReceipt(t *testing.T, h *harness) receipt {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.config.ArtifactDir, "cleanup.json"))
	if err != nil {
		t.Fatal("read durable cleanup receipt", err)
	}
	var state receipt
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal("decode cleanup receipt", err)
	}
	return state
}

func TestCleanupRecoversCommittedCreatesAfterLostResponses(t *testing.T) {
	for _, lost := range []string{testLeaseResource, "namespaces"} {
		t.Run(lost, func(t *testing.T) {
			h := fakeHarness(t)
			kube := h.clients.kube.(*kubefake.Clientset)
			leaseGVR := schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: testLeaseResource}
			nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
			if err := kube.Tracker().Delete(leaseGVR, systemNamespace, lockName); err != nil {
				t.Fatal(err)
			}
			if err := kube.Tracker().Delete(nsGVR, "", h.state.Namespace); err != nil {
				t.Fatal(err)
			}
			h.state.LeaseUID, h.state.NamespaceUID = "", ""
			kube.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
				obj := action.(clienttesting.CreateAction).GetObject()
				intent := savedReceipt(t, h)
				switch created := obj.(type) {
				case *coordinationv1.Lease:
					if !intent.LeasePending || intent.ClusterUID == "" {
						t.Fatal("lease creation preceded durable intent")
					}
					created.UID = "created-lease"
				case *corev1.Namespace:
					if !intent.NamespacePending || intent.LeaseUID == "" {
						t.Fatal("namespace creation preceded durable intent")
					}
					created.UID = "created-namespace"
				default:
					return false, nil, nil
				}
				if err := kube.Tracker().Create(action.GetResource(), obj, action.GetNamespace()); err != nil {
					t.Fatal(err)
				}
				if action.GetResource().Resource == lost {
					return true, nil, errors.New("response lost after committed create")
				}
				return true, obj, nil
			})
			if err := h.prepare(t.Context(), nil); err == nil {
				t.Fatal("expected ambiguous Create failure")
			}
			// Recover from disk, as an always-run cleanup process would.
			h.state = savedReceipt(t, h)
			if err := h.cleanup(t.Context()); err != nil {
				t.Fatal("recover interrupted creation", err)
			}
			_, err := kube.CoordinationV1().Leases(systemNamespace).Get(t.Context(), lockName, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatal("exclusive lease leaked", err)
			}
			_, err = kube.CoreV1().Namespaces().Get(t.Context(), h.state.Namespace, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) || !savedReceipt(t, h).Cleaned {
				t.Fatal("namespace or incomplete receipt remained", err)
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() == testDeleteVerb {
					options := action.(clienttesting.DeleteAction).GetDeleteOptions()
					if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID == "" {
						t.Fatal("recovery deletion lacked UID protection")
					}
				}
			}
		})
	}
}

func TestPendingCreationNeverAdoptsForeignResources(t *testing.T) {
	for _, resource := range []string{testLeaseResource, fieldNamespace} {
		t.Run(resource, func(t *testing.T) {
			h := fakeHarness(t)
			kube := h.clients.kube.(*kubefake.Clientset)
			if resource == testLeaseResource {
				h.state.LeaseUID, h.state.LeasePending = "", true
				// The fixture lease has the same holder but no run label: both are required for recovery.
			} else {
				h.state.NamespaceUID, h.state.NamespacePending = "", true
				ns, err := kube.CoreV1().Namespaces().Get(t.Context(), h.state.Namespace, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				ns.Labels[runLabel] = "foreign-run"
				if _, err = kube.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.cleanup(t.Context()); err == nil {
				t.Fatal("foreign resource was accepted for recovery")
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() == testDeleteVerb {
					t.Fatal("deleted a resource after failed ownership recovery")
				}
			}
		})
	}
}

func TestAlreadyExistingNamespaceIsNotRecoveredOrDeleted(t *testing.T) {
	h := fakeHarness(t)
	h.state.NamespaceUID, h.state.LeaseUID = "", ""
	kube := h.clients.kube.(*kubefake.Clientset)
	leaseGVR := schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: testLeaseResource}
	if err := kube.Tracker().Delete(leaseGVR, systemNamespace, lockName); err != nil {
		t.Fatal(err)
	}
	kube.PrependReactor("create", testLeaseResource, func(a clienttesting.Action) (bool, runtime.Object, error) {
		lease := a.(clienttesting.CreateAction).GetObject().(*coordinationv1.Lease)
		lease.UID = "new-lease"
		return false, nil, nil
	})
	if err := h.prepare(t.Context(), nil); !apierrors.IsAlreadyExists(err) {
		t.Fatal("expected existing namespace rejection", err)
	}
	h.state = savedReceipt(t, h)
	if h.state.NamespacePending {
		t.Fatal("AlreadyExists was incorrectly recorded as an ambiguous create")
	}
	if err := h.cleanup(t.Context()); err != nil {
		t.Fatal("cleanup of a rejected namespace create", err)
	}
	if _, err := kube.CoreV1().Namespaces().Get(t.Context(), h.state.Namespace, metav1.GetOptions{}); err != nil {
		t.Fatal("pre-existing namespace was removed", err)
	}
}

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

const (
	testLeaseResource     = "leases"
	testNamespaceResource = "namespaces"
	testCoordinationGroup = "coordination.k8s.io"
)

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
	for _, lost := range []string{testLeaseResource, testNamespaceResource} {
		t.Run(lost, func(t *testing.T) {
			h := fakeHarness(t)
			kube := h.clients.kube.(*kubefake.Clientset)
			leaseGVR := schema.GroupVersionResource{Group: testCoordinationGroup, Version: "v1", Resource: testLeaseResource}
			nsGVR := schema.GroupVersionResource{Version: "v1", Resource: testNamespaceResource}
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
			assertDeletesCarryUIDPreconditions(t, h)
		})
	}
}

func TestRejectedCreatesClearIntentAndReleaseHeldLease(t *testing.T) {
	for _, tc := range []struct {
		name, resource string
		reject         error
	}{
		{"lease forbidden", testLeaseResource, apierrors.NewForbidden(
			schema.GroupResource{Group: testCoordinationGroup, Resource: testLeaseResource}, lockName,
			errors.New("denied"))},
		{"namespace bad request", testNamespaceResource, apierrors.NewBadRequest("rejected namespace")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHarness(t)
			kube := h.clients.kube.(*kubefake.Clientset)
			leaseGVR := schema.GroupVersionResource{Group: testCoordinationGroup, Version: "v1", Resource: testLeaseResource}
			nsGVR := schema.GroupVersionResource{Version: "v1", Resource: testNamespaceResource}
			if err := kube.Tracker().Delete(leaseGVR, systemNamespace, lockName); err != nil {
				t.Fatal(err)
			}
			if err := kube.Tracker().Delete(nsGVR, "", h.state.Namespace); err != nil {
				t.Fatal(err)
			}
			h.state.LeaseUID, h.state.NamespaceUID = "", ""
			if tc.resource != testLeaseResource {
				kube.PrependReactor("create", testLeaseResource, func(action clienttesting.Action) (bool, runtime.Object, error) {
					lease := action.(clienttesting.CreateAction).GetObject().(*coordinationv1.Lease)
					lease.UID = "created-lease"
					return false, nil, nil
				})
			}
			kube.PrependReactor("create", tc.resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.reject
			})
			if err := h.prepare(t.Context(), nil); err == nil {
				t.Fatal("rejected Create unexpectedly succeeded")
			}
			// Re-read the saved receipt as the separate cleanup command does.
			h.state = savedReceipt(t, h)
			if h.state.LeasePending || h.state.NamespacePending {
				t.Fatal("definitive rejection retained a pending creation", h.state)
			}
			if tc.resource != testLeaseResource && h.state.LeaseUID == "" {
				t.Fatal("the created Lease was not recorded for cleanup")
			}
			if err := h.cleanup(t.Context()); err != nil {
				t.Fatal("cleanup after rejected Create", err)
			}
			_, err := kube.CoordinationV1().Leases(systemNamespace).Get(t.Context(), lockName, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatal("rejected Create left the exclusive Lease", err)
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
			assertNoDeletes(t, h)
		})
	}
}

func TestAlreadyExistingNamespaceIsNotRecoveredOrDeleted(t *testing.T) {
	h := fakeHarness(t)
	h.state.NamespaceUID, h.state.LeaseUID = "", ""
	kube := h.clients.kube.(*kubefake.Clientset)
	leaseGVR := schema.GroupVersionResource{Group: testCoordinationGroup, Version: "v1", Resource: testLeaseResource}
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

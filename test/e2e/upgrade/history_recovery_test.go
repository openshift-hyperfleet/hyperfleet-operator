// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func historyObject(t *testing.T, rv string, progressing metav1.ConditionStatus) *unstructured.Unstructured {
	t.Helper()
	o := sample(time.Now(), metav1.ConditionTrue, progressing)
	for i := range o.Status.Conditions {
		o.Status.Conditions[i].Reason = "TestObservation"
		o.Status.Conditions[i].LastTransitionTime = metav1.Now()
		o.Status.Conditions[i].Message = "sensitive condition message"
	}
	status, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&o.Status)
	if err != nil {
		t.Fatal(err)
	}
	obj := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", nil)
	obj.SetUID(o.UID)
	obj.SetGeneration(o.Generation)
	obj.SetResourceVersion(rv)
	obj.Object["status"] = status
	return obj
}

func historyList(obj *unstructured.Unstructured) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*obj.DeepCopy()}}
	list.SetResourceVersion("20")
	return list
}

func waitForHistory(t *testing.T, h *history, check func([]observation) bool) []observation {
	t.Helper()
	var records []observation
	err := wait.PollUntilContextTimeout(t.Context(), time.Millisecond, 5*time.Second, true,
		func(context.Context) (bool, error) {
			var err error
			records, err = h.snapshot()
			return check(records), err
		})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestConditionHistoryRecoversExpiredWatch(t *testing.T) {
	const reconnectRequest = "reconnect request"
	for _, mode := range []string{"initial request", reconnectRequest, "expired event", "gone event"} {
		t.Run(mode, func(t *testing.T) {
			baseline := historyObject(t, "10", metav1.ConditionFalse)
			h := fakeHarness(t)
			dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
			first, next := watch.NewRaceFreeFake(), watch.NewRaceFreeFake()
			defer first.Stop()
			defer next.Stop()
			calls := 0
			versions := make(chan string, 3)
			dyn.PrependWatchReactor(hfcResource.Resource, func(action clienttesting.Action) (bool, watch.Interface, error) {
				versions <- action.(clienttesting.WatchAction).GetWatchRestrictions().ResourceVersion
				calls++
				if (mode == "initial request" && calls == 1) || (mode == reconnectRequest && calls == 2) {
					return true, nil, apierrors.NewResourceExpired("old cache")
				}
				if calls == 1 {
					return true, first, nil
				}
				return true, next, nil
			})
			dyn.PrependReactor("list", hfcResource.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
				request := action.(clienttesting.ListActionImpl).GetListOptions()
				if request.FieldSelector != "metadata.name="+hfcName || request.ResourceVersion != "" {
					return true, nil, errors.New("resync did not request a fresh singleton snapshot")
				}
				// The object itself has not changed; only the collection checkpoint
				// advanced when the CRD storage was rebuilt.
				return true, historyList(baseline), nil
			})
			history, err := startHistory(t.Context(), dyn, baseline, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer history.stop()
			switch mode {
			case reconnectRequest:
				first.Stop()
			case "expired event":
				first.Error(&metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired})
			case "gone event":
				first.Error(&metav1.Status{Code: 410, Reason: metav1.StatusReasonGone})
			}
			records := waitForHistory(t, history, func(o []observation) bool { return len(o) == 2 })
			marker := records[1].Resync
			if marker == nil || marker.FromResourceVersion != "10" || marker.ResourceVersion != "20" ||
				records[1].ResourceVersion != "10" || records[0].Resync != nil {
				t.Fatalf("lost baseline or resync checkpoint: %+v", records)
			}
			want := []string{"10", "20"}
			if mode == reconnectRequest {
				want = []string{"10", "10", "20"}
			}
			for _, version := range want {
				select {
				case actual := <-versions:
					if actual != version {
						t.Fatalf("watch resumed at %q, want %q", actual, version)
					}
				case <-time.After(time.Second):
					t.Fatal("watch did not restart")
				}
			}
			next.Modify(historyObject(t, "21", metav1.ConditionTrue))
			next.Modify(historyObject(t, "22", metav1.ConditionFalse))
			records = waitForHistory(t, history, func(o []observation) bool { return len(o) == 4 })
			if err = validateHistory(records, 0); err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				for _, condition := range record.Status.Conditions {
					if condition.Message != "" {
						t.Fatal("resync exported a raw condition message")
					}
				}
			}
		})
	}
}

func TestConditionResyncRejectsInvalidSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*unstructured.UnstructuredList)
	}{
		{"missing HFC", func(l *unstructured.UnstructuredList) { l.Items = nil }},
		{"extra HFC", func(l *unstructured.UnstructuredList) { l.Items = append(l.Items, l.Items[0]) }},
		{"replaced UID", func(l *unstructured.UnstructuredList) { l.Items[0].SetUID(testOtherIdentity) }},
		{"spec changed", func(l *unstructured.UnstructuredList) { l.Items[0].SetGeneration(2) }},
		{"deleting", func(l *unstructured.UnstructuredList) { now := metav1.Now(); l.Items[0].SetDeletionTimestamp(&now) }},
		{"missing checkpoint", func(l *unstructured.UnstructuredList) { l.SetResourceVersion("") }},
		{"missing object version", func(l *unstructured.UnstructuredList) { l.Items[0].SetResourceVersion("") }},
		{"invalid status", func(l *unstructured.UnstructuredList) { l.Items[0].Object["status"] = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline := historyObject(t, "10", metav1.ConditionFalse)
			initial, err := observe(baseline)
			if err != nil {
				t.Fatal(err)
			}
			history := &history{observations: []observation{initial}, listTimeout: time.Second}
			h := fakeHarness(t)
			list := historyList(baseline)
			tc.change(list)
			h.clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", hfcResource.Resource,
				func(clienttesting.Action) (bool, runtime.Object, error) { return true, list, nil })
			options := metav1.ListOptions{FieldSelector: "metadata.name=" + hfcName, ResourceVersion: "10"}
			if err = history.resync(t.Context(), h.clients.dynamic, &options); err == nil {
				t.Fatal("invalid resync snapshot accepted")
			}
			if options.ResourceVersion != "10" || len(history.observations) != 1 {
				t.Fatal("invalid resync advanced the watch or discarded prior evidence")
			}
		})
	}
}

func TestConditionResyncDoesNotInventOrEraseTransitions(t *testing.T) {
	for _, regression := range []bool{false, true} {
		t.Run(strconv.FormatBool(regression), func(t *testing.T) {
			now := time.Now()
			records := []observation{sample(now, metav1.ConditionTrue, metav1.ConditionFalse)}
			if regression {
				records = append(records, sample(now, metav1.ConditionFalse, metav1.ConditionTrue))
			}
			resync := sample(now, metav1.ConditionTrue, metav1.ConditionFalse)
			resync.Resync = &conditionResync{FromResourceVersion: "10", ResourceVersion: "20"}
			records = append(records, resync, sample(now, metav1.ConditionTrue, metav1.ConditionFalse))
			if err := validateHistory(records, 0); err == nil {
				t.Fatal("resync hid missed progress or an observed availability regression")
			}
		})
	}
}

func TestConditionWatchBoundsRepeatedExpiry(t *testing.T) {
	baseline := historyObject(t, "10", metav1.ConditionFalse)
	h := fakeHarness(t)
	dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
	calls := 0
	dyn.PrependWatchReactor(hfcResource.Resource, func(clienttesting.Action) (bool, watch.Interface, error) {
		calls++
		return true, nil, apierrors.NewResourceExpired("still expired")
	})
	dyn.PrependReactor("list", hfcResource.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, historyList(baseline), nil
	})
	_, err := startHistory(t.Context(), dyn, baseline, time.Second)
	if err == nil || !strings.Contains(err.Error(), "repeatedly expired") || calls != 3 {
		t.Fatalf("repeated expiry was not bounded: calls=%d, error=%v", calls, err)
	}
}

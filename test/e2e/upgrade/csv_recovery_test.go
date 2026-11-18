// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"errors"
	"testing"
	"time"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func csvApprovalHarness(t *testing.T) (*harness, *unstructured.Unstructured) {
	t.Helper()
	pair := testPair(t)
	plan := validPlan()
	plan.SetNamespace(testNamespace)
	plan.SetUID("plan-uid")
	plan.SetResourceVersion("1")
	plan.Object[fieldSpec].(map[string]any)["clusterServiceVersionNames"] = []any{pair.To.CSV.Name}
	step := plan.Object["status"].(map[string]any)["plan"].([]any)[0].(map[string]any)
	step["resource"].(map[string]any)[fieldName] = pair.To.CSV.Name
	step["resource"].(map[string]any)[fieldSourceNamespace] = testNamespace
	sub := object(olmAPIVersion, subscriptionKind, subscriptionName, testNamespace, map[string]any{
		fieldName: packageName, fieldChannel: testChannel, fieldSource: catalogName,
		fieldSourceNamespace: testNamespace, fieldStartingCSV: pair.From.CSV.Name,
		fieldInstallPlanApproval: approvalManual})
	sub.SetUID("subscription-uid")
	sub.SetLabels(map[string]string{runLabel: testRunID})
	sub.Object["status"] = map[string]any{"installPlanRef": map[string]any{
		fieldName: plan.GetName(), fieldUID: string(plan.GetUID()), fieldNamespace: testNamespace}}
	catalog := object(olmAPIVersion, "CatalogSource", catalogName, testNamespace,
		map[string]any{fieldImage: testImage("catalog", "e")})
	catalog.SetUID("catalog-uid")
	h := fakeHarness(t, sub, plan, catalog)
	h.pair, h.subscriptionUID, h.state.SubscriptionUID = pair, sub.GetUID(), sub.GetUID()
	h.state.CatalogUID, h.state.CSVs = catalog.GetUID(), []string{pair.To.CSV.Name}
	h.config.Channel, h.config.Catalog = testChannel, stringField(catalog, fieldSpec, fieldImage)
	h.clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", csvResource.Resource,
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			options := action.(clienttesting.ListActionImpl).GetListOptions()
			if options.FieldSelector != "metadata.name="+pair.To.CSV.Name || options.ResourceVersion != "" {
				return true, nil, errors.New("invalid pre-approval CSV checkpoint request")
			}
			list := &unstructured.UnstructuredList{}
			list.SetResourceVersion("10")
			return true, list, nil
		})
	return h, plan
}

func commitCSVApproval(t *testing.T, h *harness, plan *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	plan = plan.DeepCopy()
	plan.Object[fieldSpec].(map[string]any)["approved"] = true
	status := plan.Object["status"].(map[string]any)
	status[fieldPhase] = string(operatorsv1alpha1.InstallPlanPhaseComplete)
	status["plan"].([]any)[0].(map[string]any)["status"] = string(operatorsv1alpha1.StepStatusCreated)
	dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
	if err := dyn.Tracker().Update(planResource, plan, testNamespace); err != nil {
		t.Fatal(err)
	}
	csv := object(olmAPIVersion, csvKind, h.state.CSVs[0], testNamespace, nil)
	csv.SetUID("created-csv-uid")
	csv.SetResourceVersion("11")
	if err := dyn.Tracker().Create(csvResource, csv, testNamespace); err != nil {
		t.Fatal(err)
	}
	return csv
}

func replayCSVCreation(t *testing.T, h *harness, event watch.Event) {
	t.Helper()
	dyn := h.clients.watches.(*dynamicfake.FakeDynamicClient)
	dyn.PrependWatchReactor(csvResource.Resource, func(action clienttesting.Action) (bool, watch.Interface, error) {
		options := action.(clienttesting.WatchAction).GetWatchRestrictions()
		if options.ResourceVersion != "10" || options.Fields.String() != "metadata.name="+h.state.CSVs[0] ||
			action.GetNamespace() != testNamespace {
			return true, nil, errors.New("CSV replay discarded its approval checkpoint or namespace")
		}
		stream := watch.NewRaceFreeFake()
		stream.Action(event.Type, event.Object)
		return true, stream, nil
	})
}

func TestCleanupRecoversCSVAfterCommittedApprovalResponseIsLost(t *testing.T) {
	h, plan := csvApprovalHarness(t)
	dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
	patchErr := errors.New("approval committed but response was lost")
	var created *unstructured.Unstructured
	dyn.PrependReactor("patch", planResource.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		intent := savedReceipt(t, h).CSVApprovals[h.state.CSVs[0]]
		if intent.PlanUID != plan.GetUID() || intent.CSVResourceVersion != "10" {
			t.Fatal("approval preceded recoverable provenance")
		}
		created = commitCSVApproval(t, h, plan)
		return true, nil, patchErr
	})
	if _, err := h.approve(t.Context(), h.pair.To, ""); !errors.Is(err, patchErr) {
		t.Fatalf("expected lost approval response, got %v", err)
	}
	replayCSVCreation(t, h, watch.Event{Type: watch.Added, Object: created})
	// Standalone cleanup has only the receipt, clients and cleanup configuration.
	h.state = savedReceipt(t, h)
	h.pair, h.subscriptionUID = releasePair{}, ""
	if len(h.state.CSVUIDs) != 0 {
		t.Fatal("test did not exercise the missing CSV UID window")
	}
	dyn.PrependReactor(testDeleteVerb, "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		if savedReceipt(t, h).CSVUIDs[h.state.CSVs[0]] != "created-csv-uid" {
			t.Fatal("cleanup destroyed provenance before persisting the recovered CSV UID")
		}
		return false, nil, nil
	})
	if err := h.cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := h.clients.kube.CoordinationV1().Leases(systemNamespace).Get(t.Context(), lockName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) || !savedReceipt(t, h).Cleaned {
		t.Fatal("cleanup left the run lock or incomplete receipt", err)
	}
	for _, resource := range []struct {
		name string
		kind string
	}{{plan.GetName(), planResource.Resource}, {h.state.CSVs[0], csvResource.Resource}} {
		if _, err := dyn.Tracker().Get(olmResource(resource.kind), testNamespace, resource.name); err == nil {
			t.Fatalf("%s was not deleted", resource.kind)
		}
	}
	assertDeletesCarryUIDPreconditions(t, h)
}

func TestCleanupRefusesUnprovenCSVRecovery(t *testing.T) {
	for _, mode := range []string{"no intent", "missing plan", "replaced plan", "foreign plan owner",
		"unapproved plan", "preexisting CSV", "wrong catalog", "expired history", "missing creation", "replaced CSV"} {
		t.Run(mode, func(t *testing.T) {
			h, plan := csvApprovalHarness(t)
			if err := h.recordCSVApproval(t.Context(), plan, h.state.CSVs[0]); err != nil {
				t.Fatal(err)
			}
			csv := commitCSVApproval(t, h, plan)
			dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
			updated, err := h.resource(planResource).Get(t.Context(), plan.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			event := watch.Event{Type: watch.Added, Object: csv.DeepCopy()}
			switch mode {
			case "no intent":
				h.state.CSVApprovals = nil
			case "replaced plan":
				updated.SetUID("replacement-plan")
			case "foreign plan owner":
				updated.SetOwnerReferences([]metav1.OwnerReference{{Kind: subscriptionKind, UID: testOtherIdentity}})
			case "unapproved plan":
				updated.Object[fieldSpec].(map[string]any)["approved"] = false
			case "preexisting CSV":
				updated.Object["status"].(map[string]any)["plan"].([]any)[0].(map[string]any)["status"] =
					string(operatorsv1alpha1.StepStatusPresent)
			case "wrong catalog":
				step := updated.Object["status"].(map[string]any)["plan"].([]any)[0].(map[string]any)
				step["resource"].(map[string]any)[fieldSourceNamespace] = "foreign-namespace"
			case "expired history":
				event = watch.Event{Type: watch.Error, Object: &metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired}}
			case "missing creation":
				event.Type = watch.Modified
			case "replaced CSV":
				csv.SetUID("replacement-csv")
				if err = dyn.Tracker().Update(csvResource, csv, testNamespace); err != nil {
					t.Fatal(err)
				}
			}
			if err = dyn.Tracker().Update(planResource, updated, testNamespace); err != nil {
				t.Fatal(err)
			}
			if mode == "missing plan" {
				if err = dyn.Tracker().Delete(planResource, testNamespace, plan.GetName()); err != nil {
					t.Fatal(err)
				}
			}
			replayCSVCreation(t, h, event)
			if err = h.cleanup(t.Context()); err == nil {
				t.Fatal("unproven CSV recovery succeeded")
			}
			assertNoDeletes(t, h)
			if h.state.Cleaned || h.state.CSVUIDs[csv.GetName()] != "" {
				t.Fatal("cleanup accepted an unproven CSV identity")
			}
		})
	}
}

func TestCSVRecoveryWaitIsBounded(t *testing.T) {
	h, plan := csvApprovalHarness(t)
	csv := commitCSVApproval(t, h, plan)
	h.config.Timeout = 20 * time.Millisecond
	stream := watch.NewRaceFreeFake()
	h.clients.watches.(*dynamicfake.FakeDynamicClient).PrependWatchReactor(csvResource.Resource,
		func(clienttesting.Action) (bool, watch.Interface, error) { return true, stream, nil })
	if err := h.recoverCSVCreationUID(t.Context(), csv, csvApproval{CSVResourceVersion: "10"}); err == nil {
		t.Fatal("missing creation event passed recovery")
	}
	if !stream.IsStopped() {
		t.Fatal("timed-out recovery leaked its watch")
	}
}

func TestApprovalRequiresAbsentCSVCheckpoint(t *testing.T) {
	for _, mode := range []string{"preexisting object", "missing version", "zero version"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := csvApprovalHarness(t)
			dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
			dyn.PrependReactor("list", csvResource.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				list := &unstructured.UnstructuredList{}
				switch mode {
				case "preexisting object":
					list.SetResourceVersion("10")
					list.Items = []unstructured.Unstructured{*object(olmAPIVersion, csvKind, h.state.CSVs[0], testNamespace, nil)}
				case "zero version":
					list.SetResourceVersion("0")
				}
				return true, list, nil
			})
			if _, err := h.approve(t.Context(), h.pair.To, ""); err == nil {
				t.Fatal("approval accepted an unsafe creation checkpoint")
			}
			for _, action := range dyn.Actions() {
				if action.GetVerb() == "patch" {
					t.Fatal("unsafe checkpoint did not prevent InstallPlan approval")
				}
			}
		})
	}
}

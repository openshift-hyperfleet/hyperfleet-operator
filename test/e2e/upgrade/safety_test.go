// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestWatchReconnectPreservesResourceVersion(t *testing.T) {
	baseline := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", map[string]any{})
	baseline.SetUID("hfc")
	baseline.SetResourceVersion("10")
	h := fakeHarness(t)
	first, second := watch.NewRaceFreeFake(), watch.NewRaceFreeFake()
	versions := make(chan string, 2)
	calls := 0
	h.clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependWatchReactor("hyperfleetconfigs",
		func(action clienttesting.Action) (bool, watch.Interface, error) {
			versions <- action.(clienttesting.WatchAction).GetWatchRestrictions().ResourceVersion
			calls++
			if calls == 1 {
				return true, first, nil
			}
			return true, second, nil
		})
	history, err := startHistory(t.Context(), h.clients.dynamic, baseline, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer history.stop()
	if got := <-versions; got != "10" {
		t.Fatalf("initial watch version=%s", got)
	}
	updated := baseline.DeepCopy()
	updated.SetResourceVersion("11")
	first.Modify(updated)
	err = wait.PollUntilContextTimeout(t.Context(), time.Millisecond, time.Second, true,
		func(context.Context) (bool, error) {
			observations, err := history.snapshot()
			return len(observations) == 2, err
		})
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()
	select {
	case got := <-versions:
		if got != "11" {
			t.Fatalf("reconnect started at %s instead of the last observed version", got)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not reconnect")
	}
}

func TestWatchReconnectFailureIsReported(t *testing.T) {
	baseline := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", map[string]any{})
	baseline.SetResourceVersion("10")
	h := fakeHarness(t)
	stream := watch.NewRaceFreeFake()
	first := true
	h.clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependWatchReactor("hyperfleetconfigs",
		func(clienttesting.Action) (bool, watch.Interface, error) {
			if first {
				first = false
				return true, stream, nil
			}
			return true, nil, errors.New("connection lost")
		})
	history, err := startHistory(t.Context(), h.clients.dynamic, baseline, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer history.stop()
	stream.Stop()
	select {
	case <-history.done:
	case <-time.After(time.Second):
		t.Fatal("watch reconnect failure was hidden")
	}
	if _, err := history.snapshot(); err == nil {
		t.Fatal("failed reconnect passed")
	}
}

func validPlan() *unstructured.Unstructured {
	p := object(olmAPIVersion, "InstallPlan", "install-target", "run-ns", map[string]any{
		"approval": approvalManual, "approved": false, "clusterServiceVersionNames": []any{testTargetCSV}})
	p.SetOwnerReferences([]metav1.OwnerReference{{Kind: subscriptionKind, UID: "subscription-uid"}})
	p.Object["status"] = map[string]any{fieldPhase: "RequiresApproval",
		"plan": []any{map[string]any{"resource": map[string]any{
			fieldKind: csvKind, fieldName: testTargetCSV, "sourceName": catalogName,
			fieldSourceNamespace: "run-ns"}}}}
	return p
}

func TestInstallPlanApprovalRejectsWrongIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*unstructured.Unstructured)
		invalid bool
	}{
		{"exact plan", func(*unstructured.Unstructured) {}, false},
		{"wrong csv", func(p *unstructured.Unstructured) {
			p.Object[fieldSpec].(map[string]any)["clusterServiceVersionNames"] = []any{testOtherIdentity}
		}, true},
		{"extra csv", func(p *unstructured.Unstructured) {
			p.Object[fieldSpec].(map[string]any)["clusterServiceVersionNames"] = []any{testTargetCSV, testOtherIdentity}
		}, true},
		{"foreign subscription", func(p *unstructured.Unstructured) {
			p.SetOwnerReferences([]metav1.OwnerReference{{Kind: subscriptionKind, UID: testOtherIdentity}})
		}, true},
		{"automatic", func(p *unstructured.Unstructured) {
			p.Object[fieldSpec].(map[string]any)["approval"] = "Automatic"
		}, true},
		{"already approved", func(p *unstructured.Unstructured) {
			p.Object[fieldSpec].(map[string]any)["approved"] = true
		}, true},
		{"unresolved", func(p *unstructured.Unstructured) { p.Object["status"].(map[string]any)["plan"] = []any{} }, true},
		{"failed", func(p *unstructured.Unstructured) {
			p.Object["status"].(map[string]any)[fieldPhase] = phaseFailed
		}, true},
		{"foreign catalog", func(p *unstructured.Unstructured) {
			step := p.Object["status"].(map[string]any)["plan"].([]any)[0].(map[string]any)
			step["resource"].(map[string]any)["sourceName"] = testOtherIdentity
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlan()
			tc.change(p)
			err := planForApproval(p, "subscription-uid", testTargetCSV, "run-ns")
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v; invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestInstallPlanReferenceMustMatchUIDAndNamespace(t *testing.T) {
	plan := validPlan()
	plan.SetUID("plan-uid")
	for _, field := range []string{fieldUID, fieldNamespace, fieldName} {
		t.Run(field, func(t *testing.T) {
			sub := object(olmAPIVersion, subscriptionKind, subscriptionName,
				plan.GetNamespace(), map[string]any{})
			ref := map[string]any{fieldUID: string(plan.GetUID()), fieldNamespace: plan.GetNamespace(),
				fieldName: plan.GetName()}
			sub.Object["status"] = map[string]any{"installPlanRef": ref}
			if err := validatePlanReference(sub, plan); err != nil {
				t.Fatal(err)
			}
			ref[field] = "replaced"
			if validatePlanReference(sub, plan) == nil {
				t.Fatal("mismatched InstallPlan reference accepted")
			}
		})
	}
}

func fakeHarness(t *testing.T, objects ...runtime.Object) *harness {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			hfcResource: "HyperFleetConfigList", crdResource: "CustomResourceDefinitionList",
			subResource: "SubscriptionList",
			csvResource: "ClusterServiceVersionList", planResource: "InstallPlanList",
			catalogResource:    "CatalogSourceList",
			deploymentResource: "DeploymentList", podResource: "PodList",
		}, objects...)
	state := receipt{RunID: testRunID, ClusterUID: "cluster-uid", Namespace: testNamespace,
		NamespaceUID: "namespace-uid", LeaseUID: "lease-uid"}
	kube := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: systemNamespace, UID: state.ClusterUID}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: state.Namespace, UID: state.NamespaceUID,
			Labels: map[string]string{runLabel: state.RunID}}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: lockName, Namespace: systemNamespace,
			UID: state.LeaseUID},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(state.RunID)}},
	)
	return &harness{state: state, clients: clients{kube: kube, dynamic: dyn, watches: dyn,
		metadata: metadatafake.NewSimpleMetadataClient(metadatafake.NewTestScheme())}, config: Config{
		Namespace: state.Namespace, ArtifactDir: t.TempDir(), Timeout: time.Second, PollInterval: time.Millisecond}}
}

func TestCleanupRefusesReplacedResources(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(*harness)
	}{
		{"cluster", func(h *harness) { h.state.ClusterUID = testOtherIdentity }},
		{fieldNamespace, func(h *harness) { h.state.NamespaceUID = testOtherIdentity }},
		{"lease", func(h *harness) { h.state.LeaseUID = testOtherIdentity }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHarness(t)
			tc.replace(h)
			if err := h.cleanup(t.Context()); err == nil {
				t.Fatal("foreign identity accepted")
			}
			assertNoDeletes(t, h)
		})
	}
}

func TestCleanupUsesUIDPreconditions(t *testing.T) {
	cr := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", map[string]any{})
	cr.SetUID("hfc-uid")
	h := fakeHarness(t, cr)
	h.state.HFCUID = cr.GetUID()
	if err := h.cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertDeletesCarryUIDPreconditions(t, h)
	if !h.state.Cleaned {
		t.Fatal("cleanup completion not recorded")
	}
	if err := h.cleanup(t.Context()); err != nil {
		t.Fatal("cleanup is not idempotent", err)
	}
}

func TestCleanupDoesNotRemovePreexistingCRD(t *testing.T) {
	crd := object("apiextensions.k8s.io/v1", "CustomResourceDefinition", hfcCRD, "", map[string]any{})
	crd.SetUID("preexisting")
	h := fakeHarness(t, crd)
	h.state.NamespaceUID = "" // setup refused the existing CRD before creating its namespace
	if err := h.cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.clients.dynamic.Resource(crdResource).Get(t.Context(), hfcCRD, metav1.GetOptions{}); err != nil {
		t.Fatal("pre-existing CRD was removed", err)
	}
}

func TestCleanupCancelsOnlyOwnedInstallPlansBeforeCSVs(t *testing.T) {
	for _, subPresent := range []bool{true, false} {
		t.Run(strconv.FormatBool(subPresent), func(t *testing.T) {
			owned := validPlan()
			owned.SetNamespace(testNamespace)
			owned.SetUID("plan-uid")
			foreign := owned.DeepCopy()
			foreign.SetName("foreign-plan")
			foreign.SetUID("foreign-plan-uid")
			foreign.SetOwnerReferences([]metav1.OwnerReference{{Kind: subscriptionKind, UID: testOtherIdentity}})
			csv := object(olmAPIVersion, csvKind, testTargetCSV, testNamespace, map[string]any{})
			csv.SetUID("csv-uid")
			objects := []runtime.Object{owned, foreign, csv}
			if subPresent {
				sub := object(olmAPIVersion, subscriptionKind, subscriptionName,
					testNamespace, map[string]any{})
				sub.SetUID("subscription-uid")
				sub.SetLabels(map[string]string{runLabel: testRunID})
				objects = append(objects, sub)
			}
			h := fakeHarness(t, objects...)
			h.state.SubscriptionUID = "subscription-uid"
			h.state.CSVs = []string{testTargetCSV}
			h.state.CSVUIDs = map[string]types.UID{testTargetCSV: csv.GetUID()}
			if err := h.cleanupOLM(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err := h.resource(planResource).Get(t.Context(), owned.GetName(), metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("owned pending plan remains: %v", err)
			}
			if _, err := h.resource(planResource).Get(t.Context(), foreign.GetName(), metav1.GetOptions{}); err != nil {
				t.Fatal("foreign InstallPlan was removed", err)
			}
			planDeleted := false
			for _, action := range h.clients.dynamic.(*dynamicfake.FakeDynamicClient).Actions() {
				if action.GetVerb() != testDeleteVerb {
					continue
				}
				if action.GetResource() == planResource {
					planDeleted = true
				}
				if action.GetResource() == csvResource && !planDeleted {
					t.Fatal("CSV deletion raced a pending InstallPlan")
				}
			}
		})
	}
}

func TestCleanupRefusesUnrecordedOrReplacedCSV(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  types.UID
	}{
		{name: "unrecorded"},
		{name: "replaced", uid: "previous-csv-uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			csv := object(olmAPIVersion, csvKind, testTargetCSV, testNamespace, map[string]any{})
			csv.SetUID("current-csv-uid")
			h := fakeHarness(t, csv)
			h.state.CSVs = []string{testTargetCSV}
			if tc.uid != "" {
				h.state.CSVUIDs = map[string]types.UID{testTargetCSV: tc.uid}
			}
			if err := h.cleanupOLM(t.Context()); err == nil {
				t.Fatal("cleanup accepted a CSV without the recorded identity")
			}
			assertNoDeletes(t, h)
		})
	}
}

func TestCSVUIDReceiptRejectsReplacement(t *testing.T) {
	h := fakeHarness(t)
	first := object(olmAPIVersion, csvKind, testTargetCSV, testNamespace, map[string]any{})
	first.SetUID("original-csv-uid")
	if err := h.recordCSVUID(first); err != nil {
		t.Fatal(err)
	}
	if got := savedReceipt(t, h).CSVUIDs[testTargetCSV]; got != first.GetUID() {
		t.Fatalf("CSV identity was not saved: %s", got)
	}
	replacement := first.DeepCopy()
	replacement.SetUID("replacement-csv-uid")
	if err := h.recordCSVUID(replacement); err == nil {
		t.Fatal("a replacement CSV was accepted during installation")
	}
}

func TestCleanupRecoversOnlyThisRunsInterruptedHFCCreate(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(strconv.FormatBool(owned), func(t *testing.T) {
			cr := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", map[string]any{})
			cr.SetUID("interrupted-create-uid")
			if owned {
				cr.SetLabels(map[string]string{runLabel: testRunID})
			}
			h := fakeHarness(t, cr)
			h.state.Approved = true
			if err := h.cleanupHFC(t.Context()); (err == nil) != owned {
				t.Fatalf("owned=%v cleanup error=%v", owned, err)
			}
			_, err := h.clients.dynamic.Resource(hfcResource).Get(t.Context(), hfcName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) != owned {
				t.Fatalf("incorrect cleanup decision: %v", err)
			}
		})
	}
}

func TestUnavailableAPIDoesNotPreventKubernetesCleanup(t *testing.T) {
	h := fakeHarness(t)
	h.state.ProbeID = "unreachable-record"
	h.state.Probe.Service = "missing-api"
	if err := h.cleanup(t.Context()); err == nil {
		t.Fatal("failed data deletion must remain a cleanup failure")
	}
	_, err := h.clients.kube.CoreV1().Namespaces().Get(t.Context(), h.state.Namespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("API outage prevented namespace teardown", err)
	}
	if h.state.Cleaned || h.state.ProbeID == "" {
		t.Fatal("lost the receipt needed to investigate external data cleanup")
	}
}

func TestNonExpiryWatchErrorInvalidatesEvidence(t *testing.T) {
	baseline := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", map[string]any{})
	baseline.SetUID(types.UID("hfc"))
	baseline.SetResourceVersion("10")
	h := fakeHarness(t)
	stream := watch.NewRaceFreeFake()
	h.clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependWatchReactor("hyperfleetconfigs",
		func(clienttesting.Action) (bool, watch.Interface, error) { return true, stream, nil })
	history, err := startHistory(t.Context(), h.clients.dynamic, baseline, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer history.stop()
	stream.Error(&metav1.Status{Code: 403, Reason: metav1.StatusReasonForbidden})
	select {
	case <-history.done:
	case <-time.After(time.Second):
		t.Fatal("forbidden watch did not fail")
	}
	if _, err = history.snapshot(); err == nil {
		t.Fatal("forbidden history accepted")
	}
}

func TestClosedHistoryWatchesBackOffAndCancel(t *testing.T) {
	for _, workload := range []bool{false, true} {
		t.Run(strconv.FormatBool(workload), func(t *testing.T) {
			h := fakeHarness(t)
			dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
			var calls atomic.Int32
			dyn.PrependWatchReactor("*", func(clienttesting.Action) (bool, watch.Interface, error) {
				calls.Add(1)
				stream := watch.NewRaceFreeFake()
				stream.Stop()
				return true, stream, nil
			})
			dyn.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, &unstructured.UnstructuredList{Object: map[string]any{
					fieldMetadata: map[string]any{fieldResourceVersion: "10"}}}, nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stop func()
			var done <-chan struct{}
			if workload {
				history, err := startWorkloadHistory(ctx, h.resource(deploymentResource), deploymentResource,
					h.config.Timeout)
				if err != nil {
					t.Fatal(err)
				}
				stop, done = history.stop, history.done
			} else {
				baseline := object("hyperfleet.redhat.com/v1alpha1", "HyperFleetConfig", hfcName, "", nil)
				baseline.SetResourceVersion("10")
				history, err := startHistory(ctx, dyn, baseline, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				stop, done = history.stop, history.done
			}
			defer stop()
			// A permanently closed stream must not generate a hot reconnect loop.
			<-time.After(100 * time.Millisecond)
			if calls.Load() > 1 {
				t.Fatal("watch reconnected without backoff")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("backoff blocked cancellation")
			}
		})
	}
}

func TestFixturesRejectClusterScopeAndMutableImages(t *testing.T) {
	for _, fixture := range []string{
		"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: foreign\n",
		`apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
spec:
  template:
    spec:
      containers:
      - name: db
        image: postgres:latest
`,
	} {
		path := filepath.Join(t.TempDir(), "fixtures.yaml")
		if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFixtures(path); err == nil {
			t.Fatal("unsafe fixture was accepted")
		}
	}
}

func TestDiagnosticsRedactCredentialForms(t *testing.T) {
	secret := "private/value+with space"
	encoded, _ := json.Marshal(secret)
	input := strings.Join([]string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), url.QueryEscape(secret),
		string(encoded), "Bearer opaque-access-token", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1c2VyIn0.signature"}, "\n")
	output := redact(input, []string{secret})
	for _, forbidden := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)),
		url.QueryEscape(secret), "opaque-access-token", "eyJhbGci"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("redaction leaked a credential form")
		}
	}
}

func TestDataFingerprintIgnoresStatusButDetectsDataLoss(t *testing.T) {
	record := map[string]any{"id": "record-id", fieldName: "record",
		fieldSpec: map[string]any{"enabled": true}, "status": "pending"}
	before, err := recordFingerprint(record)
	if err != nil {
		t.Fatal(err)
	}
	record["status"] = fieldReady
	after, err := recordFingerprint(record)
	if err != nil || after != before {
		t.Fatal("status changes must not change the data fingerprint")
	}
	record[fieldSpec] = map[string]any{"enabled": false}
	after, err = recordFingerprint(record)
	if err != nil || after == before {
		t.Fatal("data corruption was not detected")
	}
}

const (
	testRunID      = "run"
	testDeleteVerb = "delete"
)

func deleteActions(h *harness) []clienttesting.Action {
	return append(h.clients.kube.(*kubefake.Clientset).Actions(),
		h.clients.dynamic.(*dynamicfake.FakeDynamicClient).Actions()...)
}

func assertNoDeletes(t *testing.T, h *harness) {
	t.Helper()
	for _, action := range deleteActions(h) {
		if action.GetVerb() == testDeleteVerb || action.GetVerb() == "delete-collection" {
			t.Fatalf("deleted %s despite ownership mismatch", action.GetResource())
		}
	}
}

func assertDeletesCarryUIDPreconditions(t *testing.T, h *harness) {
	t.Helper()
	deletes := 0
	for _, action := range deleteActions(h) {
		if action.GetVerb() == "delete-collection" {
			t.Fatal("bulk deletion cannot enforce individual UID ownership")
		}
		if action.GetVerb() != testDeleteVerb {
			continue
		}
		deletes++
		options := action.(clienttesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID == "" {
			t.Fatalf("delete %s lacked UID protection", action.GetResource())
		}
	}
	if deletes == 0 {
		t.Fatal("expected cleanup to perform UID-protected deletions")
	}
}

const (
	testNamespace    = "test-ns"
	testRuntimeImage = "runtime"
	testSentinelName = "sentinel"
	testEnabledRegex = "enabled_regex"
)

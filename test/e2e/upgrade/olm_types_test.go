// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"encoding/json"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestTypedOLMConstructionPreservesInstallSelection(t *testing.T) {
	h := fakeHarness(t)
	h.pair = testPair(t)
	h.config.Catalog, h.config.Channel = testImage("catalog", "c"), testChannel
	created := map[string]*unstructured.Unstructured{}
	dyn := h.clients.dynamic.(*dynamicfake.FakeDynamicClient)
	dyn.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		obj := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		obj.SetUID("test-uid")
		created[action.GetResource().Resource] = obj.DeepCopy()
		return true, obj, nil
	})
	dyn.PrependReactor("get", "catalogsources", func(clienttesting.Action) (bool, runtime.Object, error) {
		catalog := created["catalogsources"].DeepCopy()
		catalog.Object["status"] = map[string]any{"connectionState": map[string]any{"lastObservedState": "READY"}}
		return true, catalog, nil
	})
	if err := h.createOLM(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		resource, apiVersion, kind, name string
		spec                             map[string]any
	}{
		{"catalogsources", olmAPIVersion, "CatalogSource", catalogName,
			map[string]any{"sourceType": "grpc", fieldImage: h.config.Catalog, "displayName": "HyperFleet upgrade test",
				"icon": map[string]any{"base64data": "", "mediatype": ""}}},
		{"operatorgroups", "operators.coreos.com/v1", "OperatorGroup", "upgrade", map[string]any{}},
		{"subscriptions", olmAPIVersion, subscriptionKind, subscriptionName, map[string]any{
			"name": packageName, "channel": testChannel, "source": catalogName, "sourceNamespace": h.config.Namespace,
			"startingCSV": h.pair.From.CSV.Name, "installPlanApproval": "Manual"}},
	} {
		obj := created[tc.resource]
		if obj == nil || obj.GetAPIVersion() != tc.apiVersion || obj.GetKind() != tc.kind ||
			obj.GetName() != tc.name || obj.GetNamespace() != h.config.Namespace ||
			obj.GetLabels()[runLabel] != h.state.RunID {
			t.Fatalf("wrong identity or run ownership for %s", tc.resource)
		}
		spec, _, err := unstructured.NestedMap(obj.Object, fieldSpec)
		if err != nil || !reflect.DeepEqual(spec, tc.spec) {
			t.Fatalf("%s spec changed during typed conversion: got %v, want %v", tc.resource, spec, tc.spec)
		}
	}
	if len(created) != 3 || h.state.CatalogUID != "test-uid" || h.state.SubscriptionUID != "test-uid" {
		t.Fatal("creation inventory or receipt identities changed")
	}
}

func TestDecodeCSVRejectsMissingOrInvalidVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"missing", nil}, {"empty", ""}, {"null", nil}, {"non-string", 123},
		{"invalid", "not-a-version"}, {"incomplete", "1.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{}
			if tc.name != "missing" {
				spec["version"] = tc.value
			}
			data, err := json.Marshal(map[string]any{"kind": csvKind, "spec": spec})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = decodeCSV(data); err == nil {
				t.Fatal("missing or invalid version was accepted as a release")
			}
		})
	}
}

func TestBundleDecodingIgnoresNonCSVVersionFields(t *testing.T) {
	r := testRelease(t, testTargetVersion, "b")
	var bundle catalogObject
	if err := json.Unmarshal(bundleJSON(t, r), &bundle); err != nil {
		t.Fatal(err)
	}
	// Other bundle objects have their own schemas. A numeric version on such an
	// object must not be decoded as the CSV's semantic version.
	value, err := json.Marshal(map[string][]byte{
		"data": []byte(`{"apiVersion":"example.org/v1","kind":"Other","spec":{"version":42}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle.Properties = append(bundle.Properties, property{Type: bundleObjectProperty, Value: value})
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseBundle(data, r.Bundle)
	if err != nil || parsed.CSV.Name != r.CSV.Name || parsed.CSV.Spec.Version.String() != testTargetVersion {
		t.Fatal("non-CSV manifest interfered with CSV decoding", err)
	}
}

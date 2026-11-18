// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"os"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestCSVCreationReplayRejectsUnrecordedReplacement(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("requires envtest assets; run make test")
	}
	// Only metadata and real API-server watch history matter here. This minimal
	// CSV CRD is a test fixture, not OLM installation or release validation.
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: csvResource.Resource + "." + csvResource.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: csvResource.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: csvResource.Resource, Kind: csvKind},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: csvResource.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr.To(true)}}}}},
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDs: []*apiextensionsv1.CustomResourceDefinition{crd}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	namespaces := schema.GroupVersionResource{Version: "v1", Resource: testNamespaceResource}
	if _, err = client.Resource(namespaces).Create(t.Context(),
		object("v1", "Namespace", testNamespace, "", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	resource := client.Resource(csvResource).Namespace(testNamespace)
	checkpoint, err := resource.List(t.Context(), metav1.ListOptions{FieldSelector: "metadata.name=" + testTargetCSV})
	if err != nil || len(checkpoint.Items) != 0 {
		t.Fatal("expected absent CSV checkpoint", err)
	}
	fixture := object(olmAPIVersion, csvKind, testTargetCSV, testNamespace, nil)
	created, err := resource.Create(t.Context(), fixture, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h := fakeHarness(t)
	h.clients.watches = client
	h.config.Timeout = 5 * time.Second
	intent := csvApproval{CSVResourceVersion: checkpoint.GetResourceVersion()}
	if err = h.recoverCSVCreationUID(t.Context(), created, intent); err != nil {
		t.Fatal("could not replay real CSV creation event", err)
	}
	if savedReceipt(t, h).CSVUIDs[testTargetCSV] != created.GetUID() {
		t.Fatal("replayed original UID was not persisted")
	}
	if err = resource.Delete(t.Context(), testTargetCSV, metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}); err != nil {
		t.Fatal(err)
	}
	replacement, err := resource.Create(t.Context(), fixture, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash before any CSV UID was recorded: the retained Added event,
	// rather than a previous receipt UID, must identify the unseen replacement.
	h.state.CSVUIDs = nil
	if err = h.recoverCSVCreationUID(t.Context(), replacement, intent); err == nil {
		t.Fatal("recovery adopted an unrecorded replacement")
	}
	remaining, err := resource.Get(t.Context(), testTargetCSV, metav1.GetOptions{})
	if err != nil || remaining.GetUID() != replacement.GetUID() {
		t.Fatal("replacement was not preserved", err)
	}
	t.Log("replayed original CSV creation UID; refused same-name replacement without a previously recorded UID")
}

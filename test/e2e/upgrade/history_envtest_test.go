// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// make test supplies envtest assets. This starts its own control plane and never
// uses KUBECONFIG or an existing cluster; it exercises real CRD storage changes.
func TestConditionHistoryRecoversAfterCRDSchemaUpdate(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("requires envtest assets; run make test")
	}
	crdData, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "crd", "bases",
		"hyperfleet.redhat.com_hyperfleetconfigs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err = yaml.Unmarshal(crdData, &crd); err != nil {
		t.Fatal(err)
	}
	targetSpec := crd.Spec.DeepCopy()
	for i := range crd.Spec.Versions {
		delete(crd.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["status"].Properties, "components")
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDs: []*apiextensionsv1.CustomResourceDefinition{&crd}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	// Exercise a normal watch reconnect promptly rather than waiting for the
	// suite's 60-second watch timeout. The server supplies any expiry response.
	cfg.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(request *http.Request) (*http.Response, error) {
			copy := request.Clone(request.Context())
			query := copy.URL.Query()
			if watching, _ := strconv.ParseBool(query.Get("watch")); watching {
				query.Set("timeoutSeconds", "1")
				copy.URL.RawQuery = query.Encode()
			}
			return next.RoundTrip(copy)
		})
	})
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	crds, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	resource := client.Resource(hfcResource)
	sampleData, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "samples",
		"hyperfleet_v1alpha1_hyperfleetconfig.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cr := &unstructured.Unstructured{}
	if err = yaml.Unmarshal(sampleData, cr); err != nil {
		t.Fatal(err)
	}
	cr, err = resource.Create(ctx, cr, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cr = updateHistoryStatus(t, ctx, resource, cr, metav1.ConditionFalse)
	history, err := startHistory(ctx, client, cr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer history.stop()
	installed, err := crds.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, hfcCRD, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	installed.Spec = *targetSpec
	_, err = crds.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, installed, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Do not update the HFC yet: its object RV must remain older than the new
	// watch cache. A recovery that starts from a fresh GET's RV will expire again.
	err = wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		records, err := history.snapshot()
		return len(records) > 1 && records[len(records)-1].Resync != nil, err
	})
	if err != nil {
		t.Fatalf("CRD schema update did not recover through an actual watch expiry: %v", err)
	}
	records, err := history.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	resynced := records[len(records)-1]
	if resynced.ResourceVersion != cr.GetResourceVersion() || resynced.Resync.ResourceVersion == cr.GetResourceVersion() {
		t.Fatalf("expected unchanged HFC version and fresh collection checkpoint: %+v", resynced)
	}
	cr = updateHistoryStatus(t, ctx, resource, cr, metav1.ConditionTrue)
	waitForHistory(t, history, func(o []observation) bool {
		return o[len(o)-1].ResourceVersion == cr.GetResourceVersion()
	})
	cr = updateHistoryStatus(t, ctx, resource, cr, metav1.ConditionFalse)
	records = waitForHistory(t, history, func(o []observation) bool {
		return o[len(o)-1].ResourceVersion == cr.GetResourceVersion()
	})
	if err = validateHistory(records, 0); err != nil {
		t.Fatal(err)
	}
	t.Log("CRD added status.components; watch recovered through expiry and observed subsequent status transitions")
}

func updateHistoryStatus(t *testing.T, ctx context.Context, resource dynamic.ResourceInterface,
	cr *unstructured.Unstructured, progressing metav1.ConditionStatus) *unstructured.Unstructured {
	t.Helper()
	cr = cr.DeepCopy()
	cr.Object["status"] = historyObject(t, cr.GetResourceVersion(), progressing).Object["status"]
	updated, err := resource.UpdateStatus(ctx, cr, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

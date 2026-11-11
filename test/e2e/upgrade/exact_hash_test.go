// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func TestSecretMetadataClientRequestsOnlyPartialObjectMetadata(t *testing.T) {
	const accept = "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1"
	config := &rest.Config{Host: "https://example.invalid", Transport: roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		if request.Header.Get("Accept") != accept {
			t.Errorf("Secret metadata request allowed a full-object response: %q", request.Header.Get("Accept"))
		}
		body := `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata",` +
			`"metadata":{"name":"database","resourceVersion":"7"}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	client, err := newSecretMetadataClient(config)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := client.Resource(secretResource).Namespace("upgrade").Get(
		t.Context(), testDatabaseRole, metav1.GetOptions{})
	if err != nil || secret.ResourceVersion != "7" {
		t.Fatalf("metadata-only Secret GET failed: secret=%v err=%v", secret, err)
	}
}

func TestConfigHashV1GoldenVectors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		configs []string
		secrets []oracleSecret
		want    string
	}{
		{"sorted roles", []string{testConfigA}, []oracleSecret{
			{role: "tls", present: true, resourceVersion: "1002"},
			{role: testDatabaseRole, present: true, resourceVersion: "1001"}},
			"1a34466a5975a0f66c02b882ed6ff6d1df3058ba91a0a4d2bb3a4a7ffab400d2"},
		{"changed resourceVersion", []string{testConfigA}, []oracleSecret{
			{role: testDatabaseRole, present: true, resourceVersion: "1003"},
			{role: "tls", present: true, resourceVersion: "1002"}},
			"0e9e7a87c1f6695121b27506b5914815057c025aa08b9c897baf11854461c3a4"},
		{"ordered ConfigMaps", []string{testAlpha, testBroker}, []oracleSecret{
			{role: testBroker, present: true, resourceVersion: "42"}},
			"5046717811cf83af13a79ead0183b54d15350f9afb1daa0ac7b4d24dfb8aaaaf"},
		{"absent Secret", []string{testConfigA}, []oracleSecret{{role: testDatabaseRole}},
			"1cb7df39d2b34f7112ef1cfbf8923dbad95ddbf661efc1a07452aed4bb65c418"},
		{"present empty Secret version", []string{testConfigA}, []oracleSecret{{role: testDatabaseRole, present: true}},
			"d46e4c2b7877168986545bed050d8b5493c4aa94d4a54d3aa892c05b29a3bb4f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configHashV1(tc.configs, tc.secrets); got != tc.want {
				t.Fatalf("v1 hash=%s, want %s", got, tc.want)
			}
		})
	}
}

func exactHashHarness(t *testing.T) (*harness, Component) {
	t.Helper()
	h := fakeHarness(t)
	component := testScenario().To.Components[0]
	hash := configHashV1([]string{testConfigA}, []oracleSecret{{role: testDatabaseRole, present: true,
		resourceVersion: "1001"}})
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Deployment,
		Namespace: h.config.Namespace, UID: "api-uid", Generation: 1},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{hashAnnotation: hash}}}}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testAPIConfigName, Namespace: h.config.Namespace,
		UID: "config-uid", ResourceVersion: "1"}, Data: map[string]string{testConfigKey: testConfigA}}
	kube := h.clients.kube.(*kubefake.Clientset)
	for _, obj := range []runtime.Object{dep, cm} {
		if err := kube.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	h.clients.metadata.(*metadatafake.FakeMetadataClient).PrependReactor("get", secretResource.Resource,
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
				Name: testDatabaseSecret, Namespace: h.config.Namespace, UID: "db-uid", ResourceVersion: "1001"}}, nil
		})
	return h, component
}

func TestExactTargetHashMatchesAppliedInputs(t *testing.T) {
	h, api := exactHashHarness(t)
	results, err := h.verifyExactTargetHashes(t.Context(), []Component{api})
	if err != nil {
		t.Fatal(err)
	}
	if results[apiComponentName].Expected != results[apiComponentName].Actual ||
		len(results[apiComponentName].Sources) != 2 {
		t.Fatal("exact hash evidence did not match applied inputs")
	}
	for _, action := range h.clients.kube.(*kubefake.Clientset).Actions() {
		if action.GetResource().Resource == secretResource.Resource {
			t.Fatal("exact hash reader requested a full Secret instead of metadata")
		}
	}
}

func TestExactTargetHashChecksNewTargetComponent(t *testing.T) {
	h, api := exactHashHarness(t)
	sentinel := Component{Name: "sentinel-clusters", Deployment: "sentinel-clusters",
		HashInputs: &HashInputs{ConfigMaps: []HashConfigMap{
			{Name: "sentinel-config", Key: testConfigKey},
			{Name: "sentinel-broker", Key: "broker.yaml"}},
			Secrets: []HashSecret{{Role: testBroker, Name: testBrokerSecretName}}}}
	correctHash := configHashV1([]string{testAlpha, testBroker}, []oracleSecret{
		{role: testBroker, present: true, resourceVersion: "42"}})
	kube := h.clients.kube.(*kubefake.Clientset)
	for _, obj := range []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: sentinel.Deployment, Namespace: h.config.Namespace},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{hashAnnotation: "wrong"}}}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sentinel-config", Namespace: h.config.Namespace},
			Data: map[string]string{testConfigKey: testAlpha}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sentinel-broker", Namespace: h.config.Namespace},
			Data: map[string]string{"broker.yaml": testBroker}},
	} {
		if err := kube.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	h.clients.metadata.(*metadatafake.FakeMetadataClient).PrependReactor("get", secretResource.Resource,
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.(clienttesting.GetAction).GetName() == testBrokerSecretName {
				return true, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
					Name: testBrokerSecretName, ResourceVersion: "42"}}, nil
			}
			return false, nil, nil
		})
	_, err := h.verifyExactTargetHashes(t.Context(), []Component{api, sentinel})
	if err == nil || !strings.Contains(err.Error(), "sentinel-clusters") ||
		!strings.Contains(err.Error(), "ConfigMap/sentinel-config[config.yaml]") ||
		!strings.Contains(err.Error(), "Secret/broker-secret[broker]") {
		t.Fatalf("new component's wrong hash was not diagnosed with its inputs: %v", err)
	}
	obj, err := kube.Tracker().Get(deploymentResource, h.config.Namespace, sentinel.Deployment)
	if err != nil {
		t.Fatal(err)
	}
	dep := obj.(*appsv1.Deployment)
	dep.Spec.Template.Annotations[hashAnnotation] = correctHash
	if err = kube.Tracker().Update(deploymentResource, dep, h.config.Namespace); err != nil {
		t.Fatal(err)
	}
	result, err := h.verifyExactTargetHashes(t.Context(), []Component{api, sentinel})
	if err != nil || len(result) != 2 || result[sentinel.Name].Expected != correctHash {
		t.Fatalf("all target components were not verified: result=%v err=%v", result, err)
	}
}

func TestExactTargetHashRejectsWrongAnnotationAndMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *harness, *Component)
		want   string
	}{
		{"wrong annotation", func(t *testing.T, h *harness, c *Component) {
			kube := h.clients.kube.(*kubefake.Clientset)
			obj, err := kube.Tracker().Get(deploymentResource, h.config.Namespace, c.Deployment)
			if err != nil {
				t.Fatal(err)
			}
			dep := obj.(*appsv1.Deployment)
			dep.Spec.Template.Annotations[hashAnnotation] = "wrong"
			if err = kube.Tracker().Update(deploymentResource, dep, h.config.Namespace); err != nil {
				t.Fatal(err)
			}
		}, "differs from applied inputs"},
		{"missing ConfigMap key", func(_ *testing.T, _ *harness, c *Component) {
			c.HashInputs.ConfigMaps[0].Key = "missing.yaml"
		}, "lacks hash input key"},
		{"missing Secret", func(_ *testing.T, h *harness, _ *Component) {
			h.clients.metadata.(*metadatafake.FakeMetadataClient).PrependReactor("get", secretResource.Resource,
				func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: secretResource.Resource},
						testDatabaseSecret)
				})
		}, "Secret metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, component := exactHashHarness(t)
			tc.mutate(t, h, &component)
			_, err := h.verifyExactTargetHashes(t.Context(), []Component{component})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected exact hash result: %v", err)
			}
		})
	}
}

func TestExactTargetHashRetriesChangingSnapshot(t *testing.T) {
	h, component := exactHashHarness(t)
	newValue := "config-b"
	newHash := configHashV1([]string{newValue}, []oracleSecret{{role: testDatabaseRole, present: true,
		resourceVersion: "1001"}})
	kube := h.clients.kube.(*kubefake.Clientset)
	obj, err := kube.Tracker().Get(deploymentResource, h.config.Namespace, component.Deployment)
	if err != nil {
		t.Fatal(err)
	}
	dep := obj.(*appsv1.Deployment)
	dep.Spec.Template.Annotations[hashAnnotation] = newHash
	if err = kube.Tracker().Update(deploymentResource, dep, h.config.Namespace); err != nil {
		t.Fatal(err)
	}
	reads := 0
	kube.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		reads++
		value, version := testConfigA, "1"
		if reads > 1 {
			value, version = newValue, "2"
		}
		return true, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testAPIConfigName,
			Namespace: h.config.Namespace, UID: "config-uid", ResourceVersion: version},
			Data: map[string]string{testConfigKey: value}}, nil
	})
	result, err := h.verifyExactTargetHashes(context.Background(), []Component{component})
	if err != nil || reads < 4 || result[apiComponentName].Expected != newHash {
		t.Fatalf("changing snapshot was not retried: reads=%d result=%v err=%v", reads, result, err)
	}
}

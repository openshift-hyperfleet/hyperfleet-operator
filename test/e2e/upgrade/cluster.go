// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const hfcCRD = "hyperfleetconfigs.hyperfleet.redhat.com"

var (
	hfcResource = schema.GroupVersionResource{Group: "hyperfleet.redhat.com", Version: "v1alpha1",
		Resource: "hyperfleetconfigs"}
	crdResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1",
		Resource: "customresourcedefinitions"}
	csvResource     = olmResource("clusterserviceversions")
	subResource     = olmResource("subscriptions")
	planResource    = olmResource("installplans")
	catalogResource = olmResource("catalogsources")
)

func olmResource(name string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: name}
}

type clients struct {
	kube    kubernetes.Interface
	dynamic dynamic.Interface
	watches dynamic.Interface
	rest    *rest.Config
}

func newClients(kubeconfig string) (clients, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return clients{}, fmt.Errorf("load explicitly selected kubeconfig: %w", err)
	}
	config.UserAgent = "hyperfleet-upgrade-e2e"
	config.Timeout = 30 * time.Second
	// POST is not idempotent. In particular, a server timeout may commit a
	// Create even if a retry later receives a definitive rejection.
	config.Wrap(withoutCreateRetries)
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return clients{}, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return clients{}, err
	}
	// The ordinary request timeout would truncate the 60-second history watches.
	// Watch lifetime is bounded by the suite context and server-side timeout.
	watches, err := newWatchClient(config)
	return clients{kube: kube, dynamic: dyn, watches: watches, rest: config}, err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// withoutCreateRetries keeps client-go from retrying POST on Retry-After
// responses. Ambiguous outcomes must remain visible to receipt-based recovery.
func withoutCreateRetries(next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := next.RoundTrip(request)
		if response != nil && request.Method == http.MethodPost {
			response.Header.Del("Retry-After")
		}
		return response, err
	})
}

func newWatchClient(config *rest.Config) (dynamic.Interface, error) {
	watchConfig := rest.CopyConfig(config)
	watchConfig.Timeout = 0
	return dynamic.NewForConfig(watchConfig)
}

func object(apiVersion, kind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, fieldKind: kind,
		fieldMetadata: map[string]any{fieldName: name, fieldNamespace: namespace}, fieldSpec: spec}}
}

func stringField(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, fields...)
	return value
}

func (h *harness) poll(ctx context.Context, description string, check func(context.Context) (bool, error)) error {
	err := wait.PollUntilContextTimeout(ctx, h.config.PollInterval, h.config.Timeout, true, check)
	if err != nil {
		return fmt.Errorf("%s: %w", description, err)
	}
	return nil
}

func (h *harness) createOLM(ctx context.Context) error {
	ns := h.config.Namespace
	items := []struct {
		resource schema.GroupVersionResource
		object   *unstructured.Unstructured
	}{
		{catalogResource, object(olmAPIVersion, "CatalogSource", catalogName, ns,
			map[string]any{"sourceType": "grpc", fieldImage: h.config.Catalog,
				"displayName": "HyperFleet upgrade test"})},
		{schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1", Resource: "operatorgroups"},
			object(operatorGroupAPIVersion, "OperatorGroup", "upgrade", ns, map[string]any{})},
	}
	for _, item := range items {
		item.object.SetLabels(map[string]string{runLabel: h.state.RunID})
		created, err := h.clients.dynamic.Resource(item.resource).Namespace(ns).Create(ctx, item.object,
			metav1.CreateOptions{})
		if err != nil {
			return err
		}
		if item.resource == catalogResource {
			h.state.CatalogUID = created.GetUID()
			if err = h.saveState(); err != nil {
				return err
			}
		}
	}
	if err := h.poll(ctx, "catalog readiness", func(ctx context.Context) (bool, error) {
		catalog, err := h.clients.dynamic.Resource(catalogResource).Namespace(ns).Get(ctx, catalogName,
			metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return stringField(catalog, "status", "connectionState", "lastObservedState") == "READY", nil
	}); err != nil {
		return err
	}
	sub := object(olmAPIVersion, subscriptionKind, subscriptionName, ns, map[string]any{
		fieldName: packageName, fieldChannel: h.config.Channel, fieldSource: catalogName, fieldSourceNamespace: ns,
		fieldStartingCSV: h.pair.From.CSV.Metadata.Name, fieldInstallPlanApproval: approvalManual})
	sub.SetLabels(map[string]string{runLabel: h.state.RunID})
	created, err := h.clients.dynamic.Resource(subResource).Namespace(ns).Create(ctx, sub, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	h.subscriptionUID = created.GetUID()
	h.state.SubscriptionUID = created.GetUID()
	return h.saveState()
}

// planForApproval validates the exact plan referred to by our Subscription. A
// broad list-and-approve can accidentally approve the pending N plan at baseline.
func planForApproval(plan *unstructured.Unstructured, subUID types.UID, csv, namespace string) error {
	if plan.GetNamespace() != namespace {
		return fmt.Errorf("InstallPlan namespace differs from Subscription")
	}
	names, _, err := unstructured.NestedStringSlice(plan.Object, fieldSpec, "clusterServiceVersionNames")
	if err != nil || !slices.Equal(names, []string{csv}) {
		return fmt.Errorf("InstallPlan does not select exactly %s", csv)
	}
	owned := false
	for _, owner := range plan.GetOwnerReferences() {
		owned = owned || owner.Kind == subscriptionKind && owner.UID == subUID
	}
	if !owned {
		return fmt.Errorf("InstallPlan is not owned by this Subscription")
	}
	approved, _, _ := unstructured.NestedBool(plan.Object, fieldSpec, "approved")
	if approved || stringField(plan, fieldSpec, "approval") != approvalManual || stringField(plan,
		"status", fieldPhase) != "RequiresApproval" {
		return fmt.Errorf("InstallPlan must be unapproved and RequiresApproval with Manual approval")
	}
	steps, _, _ := unstructured.NestedSlice(plan.Object, "status", "plan")
	found := false
	for _, entry := range steps {
		step, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("malformed InstallPlan step")
		}
		resource, _, _ := unstructured.NestedMap(step, "resource")
		if resource[fieldKind] != csvKind {
			continue
		}
		if resource[fieldName] != csv || resource["sourceName"] != catalogName ||
			resource[fieldSourceNamespace] != namespace {
			return fmt.Errorf("InstallPlan CSV or catalog source differs from selected release")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("InstallPlan has no resolved CSV step")
	}
	return nil
}

func (h *harness) approve(ctx context.Context, r release, previousPlan string) (string, error) {
	var approvedName string
	err := h.poll(ctx, "resolve InstallPlan for "+r.CSV.Metadata.Name, func(ctx context.Context) (bool, error) {
		sub, err := h.resource(subResource).Get(ctx, subscriptionName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if sub.GetUID() != h.subscriptionUID {
			return false, fmt.Errorf("subscription was replaced")
		}
		if err = h.validateSubscription(ctx, sub); err != nil {
			return false, err
		}
		name := stringField(sub, "status", "installPlanRef", fieldName)
		if name == "" || name == previousPlan {
			return false, nil
		}
		plan, err := h.resource(planResource).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if err = validatePlanReference(sub, plan); err != nil {
			return false, err
		}
		phase := stringField(plan, "status", fieldPhase)
		if phase == "" || phase == "Planning" {
			return false, nil
		}
		if err = planForApproval(plan, h.subscriptionUID, r.CSV.Metadata.Name, h.config.Namespace); err != nil {
			return false, err
		}
		// JSON Patch tests protect against both replacement and concurrent edits.
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", fieldPath: "/metadata/uid", fieldValue: plan.GetUID()},
			{"op": "test", fieldPath: "/metadata/resourceVersion", fieldValue: plan.GetResourceVersion()},
			{"op": "replace", fieldPath: "/spec/approved", fieldValue: true},
		})
		// Record intent before sending a mutation whose response could be lost.
		h.state.Approved = true
		if err = h.saveState(); err != nil {
			return false, err
		}
		_, err = h.resource(planResource).Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return false, err
		}
		approvedName = name
		return true, h.saveState()
	})
	return approvedName, err
}

func validatePlanReference(sub, plan *unstructured.Unstructured) error {
	if stringField(sub, "status", "installPlanRef", fieldName) != plan.GetName() ||
		stringField(sub, "status", "installPlanRef", fieldNamespace) != plan.GetNamespace() ||
		stringField(sub, "status", "installPlanRef", fieldUID) != string(plan.GetUID()) || plan.GetUID() == "" {
		return fmt.Errorf("InstallPlan identity differs from Subscription installPlanRef")
	}
	return nil
}

func (h *harness) validateSubscription(ctx context.Context, sub *unstructured.Unstructured) error {
	for key, want := range map[string]string{fieldName: packageName, fieldChannel: h.config.Channel,
		fieldSource: catalogName, fieldSourceNamespace: h.config.Namespace, fieldInstallPlanApproval: approvalManual,
		fieldStartingCSV: h.pair.From.CSV.Metadata.Name} {
		if stringField(sub, fieldSpec, key) != want {
			return fmt.Errorf("subscription %s changed during the test", key)
		}
	}
	catalog, err := h.resource(catalogResource).Get(ctx, catalogName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if catalog.GetUID() != h.state.CatalogUID || stringField(catalog, fieldSpec, fieldImage) != h.config.Catalog {
		return fmt.Errorf("CatalogSource was replaced or its image changed")
	}
	return nil
}

func (h *harness) waitInstalled(ctx context.Context, r release, planName string) error {
	return h.poll(ctx, "install "+r.CSV.Metadata.Name, func(ctx context.Context) (bool, error) {
		plan, err := h.resource(planResource).Get(ctx, planName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		phase := stringField(plan, "status", fieldPhase)
		if phase == phaseFailed {
			return false, fmt.Errorf("InstallPlan %s failed", planName)
		}
		if phase != "Complete" {
			return false, nil
		}
		csv, err := h.resource(csvResource).Get(ctx, r.CSV.Metadata.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if stringField(csv, "status", fieldPhase) == phaseFailed {
			return false, fmt.Errorf("CSV failed")
		}
		if stringField(csv, "status", fieldPhase) != "Succeeded" {
			return false, nil
		}
		sub, err := h.resource(subResource).Get(ctx, subscriptionName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if sub.GetUID() != h.subscriptionUID {
			return false, fmt.Errorf("subscription was replaced during installation")
		}
		if stringField(sub, "status", "installedCSV") != r.CSV.Metadata.Name {
			return false, nil
		}
		if err = h.validateSubscription(ctx, sub); err != nil {
			return false, err
		}
		if err = validateInstalledCSV(csv, r); err != nil {
			return false, err
		}
		for _, expected := range r.CSV.Spec.Install.Spec.Deployments {
			dep, err := h.clients.kube.AppsV1().Deployments(h.config.Namespace).Get(ctx, expected.Name,
				metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if !deploymentReady(dep) {
				return false, nil
			}
			if err = matchContainerImages(dep.Spec.Template.Spec, expected.Spec.Template.Spec); err != nil {
				return false, err
			}
			if err = matchRelatedEnvironment(dep.Spec.Template.Spec, expected.Spec.Template.Spec); err != nil {
				return false, err
			}
			if err = h.readyPods(ctx, dep, expected.Spec.Template.Spec); err != nil {
				return false, nil
			}
		}
		crd, err := h.clients.dynamic.Resource(crdResource).Get(ctx, hfcCRD, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		h.state.CRDUID = crd.GetUID()
		return true, h.saveState()
	})
}

func validateInstalledCSV(csv *unstructured.Unstructured, r release) error {
	data, err := json.Marshal(csv.Object)
	if err != nil {
		return err
	}
	var installed csvDocument
	if err = json.Unmarshal(data, &installed); err != nil {
		return err
	}
	if installed.Spec.Version != r.CSV.Spec.Version {
		return fmt.Errorf("installed CSV version differs from bundle")
	}
	actual := release{CSV: installed, Images: map[string]string{}}
	if err = actual.validate(); err != nil {
		return err
	}
	if !maps.Equal(actual.Images, r.Images) {
		return fmt.Errorf("installed relatedImages differ from immutable bundle")
	}
	return nil
}

func deploymentReady(dep *appsv1.Deployment) bool {
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	return want > 0 && dep.Status.ObservedGeneration >= dep.Generation && dep.Status.Replicas == want &&
		dep.Status.UpdatedReplicas == want && dep.Status.ReadyReplicas == want && dep.Status.AvailableReplicas == want &&
		dep.Status.UnavailableReplicas == 0
}

func matchContainerImages(actual, expected corev1.PodSpec) error {
	if err := matchImages(actual.Containers, expected.Containers); err != nil {
		return err
	}
	return matchImages(actual.InitContainers, expected.InitContainers)
}

func matchImages(actual, expected []corev1.Container) error {
	for _, want := range expected {
		found := false
		for _, got := range actual {
			found = found || got.Name == want.Name && got.Image == want.Image
		}
		if !found {
			return fmt.Errorf("container %s has not converged to the expected image", want.Name)
		}
	}
	return nil
}

func matchRelatedEnvironment(actual, expected corev1.PodSpec) error {
	for _, want := range podImages(expected) {
		for _, got := range podImages(actual) {
			if got.Name != want.Name {
				continue
			}
			wantEnv, gotEnv := map[string]string{}, map[string]string{}
			for _, env := range want.Env {
				if strings.HasPrefix(env.Name, "RELATED_IMAGE_") {
					wantEnv[env.Name] = env.Value
				}
			}
			for _, env := range got.Env {
				if strings.HasPrefix(env.Name, "RELATED_IMAGE_") {
					if env.ValueFrom != nil {
						return fmt.Errorf("operator related image must be the bundle's literal value")
					}
					gotEnv[env.Name] = env.Value
				}
			}
			if !maps.Equal(wantEnv, gotEnv) {
				return fmt.Errorf("operator related image environment differs from the bundle")
			}
		}
	}
	return nil
}

func (h *harness) readyPods(ctx context.Context, dep *appsv1.Deployment, expected corev1.PodSpec) error {
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return err
	}
	pods, err := h.clients.kube.CoreV1().Pods(dep.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("deployment has no pods")
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("old or unfinished pod remains")
		}
		if err = matchContainerImages(pod.Spec, expected); err != nil {
			return err
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			ready = ready || condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
		}
		if !ready {
			return fmt.Errorf("pod is not ready")
		}
		for _, container := range expected.Containers {
			if !runningContainer(pod.Status.ContainerStatuses, container.Name) {
				return fmt.Errorf("container has no ready runtime image")
			}
		}
		for _, container := range expected.InitContainers {
			if !successfulInit(pod.Status.InitContainerStatuses, container.Name) {
				return fmt.Errorf("migration/init container has not succeeded")
			}
		}
	}
	return nil
}

func runningContainer(statuses []corev1.ContainerStatus, name string) bool {
	for _, status := range statuses {
		if status.Name == name {
			return status.Ready && status.ImageID != "" && status.State.Running != nil
		}
	}
	return false
}

func successfulInit(statuses []corev1.ContainerStatus, name string) bool {
	for _, status := range statuses {
		if status.Name == name {
			return status.ImageID != "" && status.State.Terminated != nil && status.State.Terminated.ExitCode == 0
		}
	}
	return false
}

func fromUnstructured[T any](obj *unstructured.Unstructured) (T, error) {
	var result T
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &result)
	return result, err
}

func (h *harness) resource(gvr schema.GroupVersionResource) dynamic.ResourceInterface {
	return h.clients.dynamic.Resource(gvr).Namespace(h.config.Namespace)
}

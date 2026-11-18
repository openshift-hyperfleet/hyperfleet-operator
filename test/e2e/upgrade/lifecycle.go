// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
)

// receipt contains identities only, never kubeconfig/Secret data. UID
// preconditions prevent a later cleanup from deleting replacement resources.
type receipt struct {
	RunID            string                 `json:"runID"`
	ClusterUID       types.UID              `json:"clusterUID"`
	Namespace        string                 `json:"namespace"`
	NamespaceUID     types.UID              `json:"namespaceUID,omitempty"`
	NamespacePending bool                   `json:"namespacePending,omitempty"`
	SubscriptionUID  types.UID              `json:"subscriptionUID,omitempty"`
	CatalogUID       types.UID              `json:"catalogUID,omitempty"`
	LeaseUID         types.UID              `json:"leaseUID,omitempty"`
	LeasePending     bool                   `json:"leasePending,omitempty"`
	HFCUID           types.UID              `json:"hfcUID,omitempty"`
	CRDUID           types.UID              `json:"crdUID,omitempty"`
	CSVs             []string               `json:"csvs"`
	CSVUIDs          map[string]types.UID   `json:"csvUIDs,omitempty"`
	CSVApprovals     map[string]csvApproval `json:"csvApprovals,omitempty"`
	Approved         bool                   `json:"approved"`
	Cleaned          bool                   `json:"cleaned"`
	ProbeID          string                 `json:"probeID,omitempty"`
	ProbePending     bool                   `json:"probePending,omitempty"`
	Probe            ProbeConfig            `json:"probe,omitempty"`
}

var fixtureResources = map[string]schema.GroupVersionResource{
	"v1/Secret":                {Version: "v1", Resource: "secrets"},
	"v1/ConfigMap":             {Version: "v1", Resource: "configmaps"},
	"v1/Service":               {Version: "v1", Resource: "services"},
	"v1/PersistentVolumeClaim": {Version: "v1", Resource: "persistentvolumeclaims"},
	"apps/v1/Deployment":       deploymentResource,
	"apps/v1/StatefulSet":      {Group: appsGroup, Version: "v1", Resource: "statefulsets"},
}

func loadFixtures(path string) ([]unstructured.Unstructured, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []unstructured.Unstructured
	seen := map[string]bool{}
	for {
		var obj unstructured.Unstructured
		if err = decoder.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("invalid fixture YAML")
		}
		if len(obj.Object) == 0 {
			continue
		}
		key := obj.GetAPIVersion() + "/" + obj.GetKind()
		if _, ok := fixtureResources[key]; !ok {
			return nil, fmt.Errorf("unsupported fixture kind %s", key)
		}
		identity := key + "/" + obj.GetName()
		if obj.GetName() == "" || seen[identity] || obj.GetUID() != "" || obj.GetResourceVersion() != "" ||
			len(obj.GetOwnerReferences()) != 0 || obj.GetGenerateName() != "" {
			return nil, fmt.Errorf("fixtures must be new, uniquely named resources")
		}
		seen[identity] = true
		if obj.GetKind() == "Deployment" || obj.GetKind() == "StatefulSet" {
			spec, _, _ := unstructured.NestedMap(obj.Object, fieldSpec, "template", fieldSpec)
			pod, err := fromUnstructured[corev1.PodSpec](&unstructured.Unstructured{Object: spec})
			if err != nil || len(pod.Containers) == 0 {
				return nil, fmt.Errorf("fixture has invalid pod specification")
			}
			for _, container := range podImages(pod) {
				if err = pinnedImage(container.Image); err != nil {
					return nil, fmt.Errorf("fixture image: %w", err)
				}
			}
		}
		objects = append(objects, obj)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("fixtures file is empty")
	}
	return objects, nil
}

func (h *harness) prepare(ctx context.Context, fixtures []unstructured.Unstructured) error {
	cluster, err := h.clients.kube.CoreV1().Namespaces().Get(ctx, systemNamespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	h.state.ClusterUID = cluster.UID
	h.state.LeasePending = true
	if err = h.saveState(); err != nil {
		return err
	}
	lease, err := h.clients.kube.CoordinationV1().Leases(systemNamespace).Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: lockName, Labels: map[string]string{runLabel: h.state.RunID}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(h.state.RunID)},
	}, metav1.CreateOptions{})
	if err != nil {
		if createDefinitelyRejected(err) {
			h.state.LeasePending = false
			return combineErrors(fmt.Errorf("obtain exclusive upgrade lease: %w", err), h.saveState())
		}
		return fmt.Errorf("obtain exclusive upgrade lease (never stolen automatically): %w", err)
	}
	h.state.LeaseUID = lease.UID
	h.state.LeasePending = false
	if err = h.saveState(); err != nil {
		return err
	}
	// Fresh installation is intentional. A pre-existing CRD might belong to an
	// existing release even if its singleton has not yet been created.
	if _, err = h.clients.dynamic.Resource(crdResource).Get(ctx, hfcCRD,
		metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("target already contains %s; an isolated clean target is required", hfcCRD)
	}
	subs, err := h.clients.dynamic.Resource(subResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("OLM Classic is not accessible: %w", err)
	}
	for _, sub := range subs.Items {
		if stringField(&sub, fieldSpec, fieldName) == packageName {
			return fmt.Errorf("target already has a HyperFleet Subscription")
		}
	}
	h.state.NamespacePending = true
	if err = h.saveState(); err != nil {
		return err
	}
	ns, err := h.clients.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: h.config.Namespace, Labels: map[string]string{runLabel: h.state.RunID}},
	}, metav1.CreateOptions{})
	if err != nil {
		if createDefinitelyRejected(err) {
			h.state.NamespacePending = false
			return combineErrors(fmt.Errorf("create fresh test namespace: %w", err), h.saveState())
		}
		return fmt.Errorf("create fresh test namespace: %w", err)
	}
	h.state.NamespaceUID = ns.UID
	h.state.NamespacePending = false
	if err = h.saveState(); err != nil {
		return err
	}
	for i := range fixtures {
		obj := fixtures[i].DeepCopy()
		if obj.GetNamespace() != "" && obj.GetNamespace() != h.config.Namespace {
			return fmt.Errorf("fixture targets another namespace")
		}
		if h.isOperandName(obj.GetName()) {
			return fmt.Errorf("fixture collides with an operator or operand resource")
		}
		obj.SetNamespace(h.config.Namespace)
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[runLabel] = h.state.RunID
		obj.SetLabels(labels)
		resource := fixtureResources[obj.GetAPIVersion()+"/"+obj.GetKind()]
		if _, err = h.resource(resource).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			// API validation errors can contain Secret values; do not forward them.
			return fmt.Errorf("failed to create fixture %s/%s", obj.GetKind(), obj.GetName())
		}
	}
	return nil
}

func (h *harness) isOperandName(name string) bool {
	for _, r := range []ReleaseExpectations{h.config.Scenario.From, h.config.Scenario.To} {
		for _, c := range r.Components {
			if c.Deployment == name {
				return true
			}
		}
	}
	for _, r := range []release{h.pair.From, h.pair.To} {
		for _, d := range r.CSV.Spec.InstallStrategy.StrategySpec.DeploymentSpecs {
			if d.Name == name {
				return true
			}
		}
	}
	return false
}

func (h *harness) saveState() error { return h.writeJSON("cleanup.json", h.state) }

func (h *harness) writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(h.config.ArtifactDir, name)
	if err = os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func uidDelete(uid types.UID) metav1.DeleteOptions {
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid},
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)}
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// These API rejections prove Create did not commit. Transport failures,
// timeouts, and server errors leave a pending intent for ownership recovery.
func createDefinitelyRejected(err error) bool {
	// Wrapped errors may retain evidence of earlier attempts. Do not discard
	// that ambiguity by unwrapping them into an otherwise definitive rejection.
	status, ok := err.(*apierrors.StatusError)
	if !ok {
		return false
	}
	switch status.ErrStatus.Reason {
	case metav1.StatusReasonAlreadyExists, metav1.StatusReasonBadRequest,
		metav1.StatusReasonForbidden, metav1.StatusReasonInvalid,
		metav1.StatusReasonMethodNotAllowed, metav1.StatusReasonNotFound,
		metav1.StatusReasonUnauthorized, metav1.StatusReasonUnsupportedMediaType:
		return true
	default:
		return false
	}
}

func (h *harness) cleanup(ctx context.Context) error {
	if h.state.Cleaned || (h.state.LeaseUID == "" && !h.state.LeasePending) {
		return nil
	}
	cluster, err := h.clients.kube.CoreV1().Namespaces().Get(ctx, systemNamespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cluster.UID != h.state.ClusterUID {
		return fmt.Errorf("cleanup kubeconfig selects another cluster")
	}
	lease, err := h.clients.kube.CoordinationV1().Leases(systemNamespace).Get(ctx, lockName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cleanup lease missing: %w", err)
	}
	if h.state.LeaseUID == "" && h.state.LeasePending {
		if lease.Labels[runLabel] != h.state.RunID || lease.Spec.HolderIdentity == nil ||
			*lease.Spec.HolderIdentity != h.state.RunID {
			return fmt.Errorf("pending lease creation does not belong to this run")
		}
		h.state.LeaseUID = lease.UID
		h.state.LeasePending = false
		if err = h.saveState(); err != nil {
			return err
		}
	}
	if lease.UID != h.state.LeaseUID || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != h.state.RunID {
		return fmt.Errorf("cleanup lease belongs to another run")
	}
	if err = h.recoverNamespace(ctx); err != nil {
		return err
	}
	if h.state.NamespaceUID != "" {
		if err = h.cleanupInstallation(ctx); err != nil {
			return err
		}
	}
	if err = h.clients.kube.CoordinationV1().Leases(systemNamespace).Delete(ctx, lockName,
		uidDelete(h.state.LeaseUID)); err != nil {
		return ignoreNotFound(err)
	}
	h.state.Cleaned = true
	return h.saveState()
}

// A missing object after an ambiguous Create is not proof the request can never
// commit. Keep the pending receipt on lookup failure for a later cleanup attempt.
func (h *harness) recoverNamespace(ctx context.Context) error {
	if h.state.NamespaceUID != "" || !h.state.NamespacePending {
		return nil
	}
	ns, err := h.clients.kube.CoreV1().Namespaces().Get(ctx, h.state.Namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("pending namespace creation is unresolved: %w", err)
	}
	if ns.Labels[runLabel] != h.state.RunID {
		return fmt.Errorf("pending namespace creation does not belong to this run")
	}
	h.state.NamespaceUID = ns.UID
	h.state.NamespacePending = false
	return h.saveState()
}

func (h *harness) cleanupInstallation(ctx context.Context) (result error) {
	ns, err := h.clients.kube.CoreV1().Namespaces().Get(ctx, h.state.Namespace, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && (ns.UID != h.state.NamespaceUID || ns.Labels[runLabel] != h.state.RunID) {
		return fmt.Errorf("cleanup namespace has been replaced; refusing deletion")
	}
	// A broken upgraded API must not prevent Kubernetes teardown. Keep the
	// failed probe receipt and fail cleanup for an external database's owner.
	if probeErr := h.deleteProbe(ctx); probeErr != nil {
		defer func() { result = combineErrors(result, fmt.Errorf("cleanup data probe: %w", probeErr)) }()
	}
	if err = h.cleanupHFC(ctx); err != nil {
		return err
	}
	if err = h.cleanupOLM(ctx); err != nil {
		return err
	}
	// Capture a partially installed CRD after stopping its installation. It may
	// have appeared even when waiting for the initial CSV failed.
	if h.state.Approved && h.state.CRDUID == "" {
		crd, readErr := h.clients.dynamic.Resource(crdResource).Get(ctx, hfcCRD, metav1.GetOptions{})
		if readErr == nil {
			h.state.CRDUID = crd.GetUID()
			if err = h.saveState(); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(readErr) {
			return readErr
		}
	}
	if err = h.cleanupCRD(ctx); err != nil {
		return err
	}
	err = h.clients.kube.CoreV1().Namespaces().Delete(ctx, h.state.Namespace, uidDelete(h.state.NamespaceUID))
	if ignoreNotFound(err) != nil {
		return err
	}
	return h.poll(ctx, "namespace cleanup", func(ctx context.Context) (bool, error) {
		_, err := h.clients.kube.CoreV1().Namespaces().Get(ctx, h.state.Namespace, metav1.GetOptions{})
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	})
}

func (h *harness) cleanupHFC(ctx context.Context) error {
	if h.state.HFCUID == "" && !h.state.Approved {
		return nil
	}
	cr, err := h.clients.dynamic.Resource(hfcResource).Get(ctx, hfcName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// A matching run label recovers an interrupted Create. Recorded UIDs also
	// protect cleanup against a later replacement, before attempting deletion.
	if h.state.HFCUID != "" {
		if cr.GetUID() != h.state.HFCUID {
			return fmt.Errorf("cleanup HyperFleetConfig has been replaced")
		}
	} else {
		if cr.GetLabels()[runLabel] != h.state.RunID {
			return fmt.Errorf("cleanup found a foreign HyperFleetConfig")
		}
		h.state.HFCUID = cr.GetUID()
		if err = h.saveState(); err != nil {
			return err
		}
	}
	// Published operators may still reconcile a terminating HFC and recreate
	// dependents, preventing foreground deletion from completing. Remove the
	// owner with background propagation; namespace teardown below waits for all
	// namespaced resources to disappear before the run's Lease is released.
	options := uidDelete(h.state.HFCUID)
	options.PropagationPolicy = ptr.To(metav1.DeletePropagationBackground)
	err = h.clients.dynamic.Resource(hfcResource).Delete(ctx, hfcName, options)
	if ignoreNotFound(err) != nil {
		return err
	}
	return h.waitGone(ctx, hfcResource, "", hfcName)
}

func (h *harness) cleanupOLM(ctx context.Context) error {
	// Stop subscription reconciliation before deleting its CSVs.
	sub, err := h.resource(subResource).Get(ctx, subscriptionName, metav1.GetOptions{})
	subPresent := err == nil
	if err == nil {
		if sub.GetLabels()[runLabel] != h.state.RunID ||
			(h.state.SubscriptionUID != "" && sub.GetUID() != h.state.SubscriptionUID) {
			return fmt.Errorf("cleanup found a foreign Subscription")
		}
		h.state.SubscriptionUID = sub.GetUID()
		if err = h.saveState(); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	// Recover before deleting the Subscription or its plans: garbage collection
	// could otherwise erase the evidence needed to identify an unrecorded CSV.
	if err = h.recoverCSVUIDs(ctx); err != nil {
		return err
	}
	if subPresent {
		err = h.resource(subResource).Delete(ctx, subscriptionName, uidDelete(sub.GetUID()))
		if ignoreNotFound(err) != nil {
			return err
		}
	}
	// An approved InstallPlan can continue installing after its Subscription is
	// deleted. Cancel owned plans before deleting CSVs or capturing a partial CRD.
	if err = h.cleanupPlans(ctx); err != nil {
		return err
	}
	if err = h.waitGone(ctx, subResource, h.state.Namespace, subscriptionName); err != nil {
		return err
	}
	for _, name := range h.state.CSVs {
		csv, readErr := h.resource(csvResource).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(readErr) {
			continue
		}
		if readErr != nil {
			return readErr
		}
		uid := h.state.CSVUIDs[name]
		if uid == "" {
			return fmt.Errorf("cleanup CSV %s has no recorded UID; refusing deletion", name)
		}
		if csv.GetUID() != uid {
			return fmt.Errorf("cleanup CSV %s has been replaced; refusing deletion", name)
		}
		err = h.resource(csvResource).Delete(ctx, name, uidDelete(uid))
		if ignoreNotFound(err) != nil {
			return err
		}
		if err = h.waitGone(ctx, csvResource, h.state.Namespace, name); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) cleanupPlans(ctx context.Context) error {
	if h.state.SubscriptionUID == "" {
		return nil
	}
	plans, err := h.resource(planResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range plans.Items {
		plan := &plans.Items[i]
		owned := false
		for _, owner := range plan.GetOwnerReferences() {
			owned = owned || (owner.Kind == subscriptionKind && owner.UID == h.state.SubscriptionUID)
		}
		if !owned {
			continue
		}
		if err = h.resource(planResource).Delete(ctx, plan.GetName(), uidDelete(plan.GetUID())); ignoreNotFound(err) != nil {
			return err
		}
		if err = h.waitGone(ctx, planResource, h.state.Namespace, plan.GetName()); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) cleanupCRD(ctx context.Context) error {
	if h.state.CRDUID == "" {
		return nil
	}
	instances, err := h.clients.dynamic.Resource(hfcResource).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if instances != nil && len(instances.Items) != 0 {
		return fmt.Errorf("refusing to delete CRD with remaining HyperFleetConfigs")
	}
	err = h.clients.dynamic.Resource(crdResource).Delete(ctx, hfcCRD, uidDelete(h.state.CRDUID))
	if ignoreNotFound(err) != nil {
		return err
	}
	return h.waitGone(ctx, crdResource, "", hfcCRD)
}

func (h *harness) waitGone(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	return h.poll(ctx, "delete "+gvr.Resource+"/"+name, func(ctx context.Context) (bool, error) {
		_, err := h.clients.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	})
}

// Cleanup removes only resources recorded by a retained/interrupted run.
func Cleanup(ctx context.Context, kubeconfig, artifactDir string) error {
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if kubeconfig == "" || artifactDir == "" {
		return fmt.Errorf("KUBECONFIG and ARTIFACT_DIR are required")
	}
	data, err := os.ReadFile(filepath.Join(artifactDir, "cleanup.json"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no cleanup receipt recorded in ARTIFACT_DIR; no cleanup attempted: " +
			"verify the run directory and whether cluster mutations occurred")
	}
	if err != nil {
		return err
	}
	var state receipt
	if err = json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.RunID == "" || state.Namespace == "" {
		return fmt.Errorf("invalid cleanup receipt")
	}
	// An eagerly saved receipt with no cluster identity precedes all mutations.
	if state.ClusterUID == "" {
		if state.LeaseUID != "" || state.LeasePending || state.NamespaceUID != "" || state.NamespacePending ||
			state.Approved || state.HFCUID != "" || state.CRDUID != "" || state.SubscriptionUID != "" ||
			state.CatalogUID != "" || len(state.CSVUIDs) != 0 || len(state.CSVApprovals) != 0 ||
			state.ProbeID != "" || state.ProbePending {
			return fmt.Errorf("invalid cleanup receipt: resources recorded without cluster identity")
		}
		return nil
	}
	client, err := newClients(kubeconfig)
	if err != nil {
		return err
	}
	h := harness{clients: client, state: state, config: Config{Kubeconfig: kubeconfig, Namespace: state.Namespace,
		ArtifactDir: artifactDir, Timeout: cleanupTimeout, PollInterval: defaultPollInterval}}
	return h.cleanup(ctx)
}

func combineErrors(primary, secondary error) error { return errors.Join(primary, secondary) }

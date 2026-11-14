// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"fmt"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
)

// OLM's CSV has no InstallPlan owner reference, and the plan's Created step has
// no CSV UID. Retain a pre-approval watch checkpoint so cleanup can recover the
// original creation UID instead of adopting an unseen same-name replacement.
type csvApproval struct {
	PlanName           string    `json:"planName"`
	PlanUID            types.UID `json:"planUID"`
	CSVResourceVersion string    `json:"csvResourceVersion"`
}

func (h *harness) recordCSVApproval(ctx context.Context, plan *unstructured.Unstructured, name string) error {
	if h.state.SubscriptionUID == "" || h.state.SubscriptionUID != h.subscriptionUID || plan.GetUID() == "" {
		return fmt.Errorf("CSV approval requires recorded Subscription and InstallPlan identities")
	}
	if _, exists := h.state.CSVApprovals[name]; exists {
		return fmt.Errorf("CSV %s already has an approval intent; refusing to replace its provenance", name)
	}
	current, err := h.resource(csvResource).List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return fmt.Errorf("read pre-approval CSV checkpoint failed")
	}
	if len(current.Items) != 0 || current.GetResourceVersion() == "" || current.GetResourceVersion() == "0" {
		return fmt.Errorf("CSV %s must be absent at a known resourceVersion before approval", name)
	}
	if h.state.CSVApprovals == nil {
		h.state.CSVApprovals = make(map[string]csvApproval)
	}
	h.state.CSVApprovals[name] = csvApproval{PlanName: plan.GetName(), PlanUID: plan.GetUID(),
		CSVResourceVersion: current.GetResourceVersion()}
	h.state.Approved = true
	return h.saveState()
}

func (h *harness) recoverCSVUIDs(ctx context.Context) error {
	for _, name := range h.state.CSVs {
		if h.state.CSVUIDs[name] != "" {
			continue
		}
		csv, err := h.resource(csvResource).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		intent, ok := h.state.CSVApprovals[name]
		if !ok || !h.state.Approved || h.state.SubscriptionUID == "" || intent.PlanName == "" ||
			intent.PlanUID == "" || intent.CSVResourceVersion == "" || intent.CSVResourceVersion == "0" {
			return fmt.Errorf("cleanup CSV %s has no recorded UID or recoverable approval intent; refusing deletion", name)
		}
		if err = h.waitForCSVCreation(ctx, name, intent); err != nil {
			return err
		}
		if err = h.recoverCSVCreationUID(ctx, csv, intent); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) waitForCSVCreation(ctx context.Context, name string, intent csvApproval) error {
	return h.poll(ctx, "CSV creation provenance for "+name, func(ctx context.Context) (bool, error) {
		obj, err := h.resource(planResource).Get(ctx, intent.PlanName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if obj.GetUID() != intent.PlanUID {
			return false, fmt.Errorf("InstallPlan was replaced; refusing CSV recovery")
		}
		plan, err := fromUnstructured[operatorsv1alpha1.InstallPlan](obj)
		if err != nil {
			return false, fmt.Errorf("malformed InstallPlan during CSV recovery")
		}
		if err = planSelectsCSV(&plan, h.state.SubscriptionUID, name, h.state.Namespace); err != nil {
			return false, err
		}
		if !plan.Spec.Approved || plan.Spec.Approval != operatorsv1alpha1.ApprovalManual {
			return false, fmt.Errorf("InstallPlan approval does not establish CSV provenance")
		}
		for _, step := range plan.Status.Plan {
			if step.Resource.Kind != csvKind {
				continue
			}
			switch step.Status {
			case operatorsv1alpha1.StepStatusCreated:
				return true, nil
			case operatorsv1alpha1.StepStatusPresent:
				return false, fmt.Errorf("InstallPlan found an existing CSV; refusing to adopt it")
			}
		}
		if plan.Status.Phase == operatorsv1alpha1.InstallPlanPhaseComplete ||
			plan.Status.Phase == operatorsv1alpha1.InstallPlanPhaseFailed {
			return false, fmt.Errorf("finished InstallPlan does not prove CSV creation")
		}
		// CSV creation and its step-status update are separate API requests.
		return false, nil
	})
}

func (h *harness) recoverCSVCreationUID(ctx context.Context,
	current *unstructured.Unstructured, intent csvApproval) error {
	ctx, cancel := context.WithTimeout(ctx, h.config.Timeout)
	defer cancel()
	client := h.clients.watches.Resource(csvResource).Namespace(h.state.Namespace)
	options := metav1.ListOptions{FieldSelector: "metadata.name=" + current.GetName(),
		ResourceVersion: intent.CSVResourceVersion, AllowWatchBookmarks: true, TimeoutSeconds: ptr.To[int64](60)}
	for {
		stream, err := client.Watch(ctx, options)
		if err != nil {
			return fmt.Errorf("CSV creation history unavailable; refusing unverified recovery")
		}
		created, err := readCSVCreation(ctx, stream, &options, current)
		stream.Stop()
		if err != nil {
			return err
		}
		if created != nil {
			// Persist before deleting any provenance or CSV, so an interrupted
			// cleanup can safely resume using the original UID alone.
			return h.recordCSVUID(created)
		}
		if !waitForWatchReconnect(ctx) {
			return ctx.Err()
		}
	}
}

func readCSVCreation(ctx context.Context, stream watch.Interface, options *metav1.ListOptions,
	current *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, open := <-stream.ResultChan():
			if !open {
				return nil, nil
			}
			if event.Type == watch.Error {
				return nil, fmt.Errorf("CSV creation history lost; refusing unverified recovery")
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok || obj.GetResourceVersion() == "" {
				return nil, fmt.Errorf("invalid CSV creation history")
			}
			options.ResourceVersion = obj.GetResourceVersion()
			if event.Type == watch.Bookmark {
				continue
			}
			if event.Type != watch.Added || obj.GetUID() == "" || obj.GetUID() != current.GetUID() ||
				obj.GetName() != current.GetName() || obj.GetNamespace() != current.GetNamespace() {
				return nil, fmt.Errorf("CSV is not the original creation after approval; refusing deletion")
			}
			return obj, nil
		}
	}
}

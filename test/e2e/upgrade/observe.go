// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
)

type componentStatus struct {
	Name       string             `json:"name"`
	Conditions []metav1.Condition `json:"conditions"`
}
type hfcStatus struct {
	ObservedGeneration int64              `json:"observedGeneration"`
	Conditions         []metav1.Condition `json:"conditions"`
	Components         []componentStatus  `json:"components,omitempty"`
}
type observation struct {
	At              time.Time `json:"at"`
	UID             types.UID `json:"uid"`
	Generation      int64     `json:"generation"`
	ResourceVersion string    `json:"resourceVersion"`
	Status          hfcStatus `json:"status"`
}

func observe(obj *unstructured.Unstructured) (observation, error) {
	status, _, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil {
		return observation{}, err
	}
	decoded, err := fromUnstructured[hfcStatus](&unstructured.Unstructured{Object: status})
	if err != nil {
		return observation{}, err
	}
	// Conditions are useful evidence without arbitrary error messages which may
	// include credentials or internal connection strings in older releases.
	for i := range decoded.Conditions {
		decoded.Conditions[i].Message = ""
	}
	for i := range decoded.Components {
		for j := range decoded.Components[i].Conditions {
			decoded.Components[i].Conditions[j].Message = ""
		}
	}
	return observation{At: time.Now().UTC(), UID: obj.GetUID(), Generation: obj.GetGeneration(),
		ResourceVersion: obj.GetResourceVersion(), Status: decoded}, nil
}

type history struct {
	mu           sync.Mutex
	observations []observation
	err          error
	cancel       context.CancelFunc
	done         chan struct{}
}

// startHistory establishes the initial watch before the InstallPlan is approved.
// Reconnects resume at the last resourceVersion. No relist can erase a gap: a
// watch error (including expired resourceVersion) fails the evidence check.
func startHistory(ctx context.Context, client dynamic.Interface,
	baseline *unstructured.Unstructured) (*history, error) {
	initial, err := observe(baseline)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &history{observations: []observation{initial}, cancel: cancel, done: make(chan struct{})}
	options := metav1.ListOptions{FieldSelector: "metadata.name=" + hfcName,
		ResourceVersion:     baseline.GetResourceVersion(),
		AllowWatchBookmarks: true, TimeoutSeconds: ptr.To[int64](60)}
	stream, err := client.Resource(hfcResource).Watch(ctx, options)
	if err != nil {
		cancel()
		return nil, err
	}
	go h.read(ctx, client, stream, options)
	return h, nil
}

func (h *history) read(ctx context.Context, client dynamic.Interface, stream watch.Interface,
	options metav1.ListOptions) {
	defer close(h.done)
	defer func() { stream.Stop() }()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-stream.ResultChan():
			if !ok {
				stream.Stop()
				if ctx.Err() != nil {
					return
				}
				next, err := client.Resource(hfcResource).Watch(ctx, options)
				if err != nil {
					h.fail(fmt.Errorf("condition history reconnect failed"))
					return
				}
				stream = next
				continue
			}
			if event.Type == watch.Error || event.Type == watch.Deleted {
				h.fail(fmt.Errorf("condition history lost or HyperFleetConfig deleted"))
				return
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok || obj.GetResourceVersion() == "" {
				h.fail(fmt.Errorf("invalid watch object"))
				return
			}
			options.ResourceVersion = obj.GetResourceVersion()
			if event.Type == watch.Bookmark {
				continue
			}
			observation, err := observe(obj)
			if err != nil {
				h.fail(err)
				return
			}
			h.mu.Lock()
			if len(h.observations) >= 10000 {
				h.err = fmt.Errorf("condition history exceeded bounded capacity")
				h.mu.Unlock()
				return
			}
			h.observations = append(h.observations, observation)
			h.mu.Unlock()
		}
	}
}

func (h *history) fail(err error) { h.mu.Lock(); defer h.mu.Unlock(); h.err = err }
func (h *history) stop()          { h.cancel(); <-h.done }
func (h *history) snapshot() ([]observation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]observation{}, h.observations...), h.err
}

func condition(conditions []metav1.Condition, name string) (metav1.Condition, bool) {
	for _, c := range conditions {
		if c.Type == name {
			return c, true
		}
	}
	return metav1.Condition{}, false
}

func conditionsHealthy(conditions []metav1.Condition, generation int64, degraded bool) bool {
	wants := map[string]metav1.ConditionStatus{conditionAvailable: metav1.ConditionTrue,
		"Progressing": metav1.ConditionFalse}
	if degraded {
		wants["Degraded"] = metav1.ConditionFalse
	}
	for name, want := range wants {
		got, ok := condition(conditions, name)
		if !ok || got.Status != want || got.ObservedGeneration != generation {
			return false
		}
	}
	return true
}

func statusHealthy(o observation, expected ReleaseExpectations) bool {
	if o.Status.ObservedGeneration != o.Generation || !conditionsHealthy(o.Status.Conditions, o.Generation, true) {
		return false
	}
	if !expected.ComponentStatus {
		return true
	}
	if len(o.Status.Components) != len(expected.Components) {
		return false
	}
	for _, component := range expected.Components {
		found := false
		for _, actual := range o.Status.Components {
			if actual.Name == component.Name && conditionsHealthy(actual.Conditions, o.Generation, false) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validateHistory(observations []observation, maxGap time.Duration) error {
	if len(observations) < 3 {
		return fmt.Errorf("insufficient evidence for False -> True -> False")
	}
	baseline := observations[0]
	progress, ok := condition(baseline.Status.Conditions, "Progressing")
	if !ok || progress.Status != metav1.ConditionFalse {
		return fmt.Errorf("upgrade baseline was not stable")
	}
	sawProgress, finished := false, false
	var unavailableSince time.Time
	for _, o := range observations {
		if o.UID != baseline.UID || o.Generation != baseline.Generation {
			return fmt.Errorf("HyperFleetConfig identity/spec changed during upgrade")
		}
		available, ok := condition(o.Status.Conditions, conditionAvailable)
		if !ok || available.ObservedGeneration != o.Generation {
			return fmt.Errorf("available condition missing or stale in history")
		}
		transition := available.LastTransitionTime.Time
		if transition.IsZero() {
			return fmt.Errorf("available condition has no transition timestamp")
		}
		if available.Status != metav1.ConditionTrue {
			if maxGap == 0 {
				return fmt.Errorf("reported availability did not remain true")
			}
			if unavailableSince.IsZero() {
				unavailableSince = transition
			}
		} else if !unavailableSince.IsZero() {
			if transition.Before(unavailableSince) || transition.Sub(unavailableSince) > maxGap {
				return fmt.Errorf("availability gap exceeded %s", maxGap)
			}
			unavailableSince = time.Time{}
		}
		p, exists := condition(o.Status.Conditions, "Progressing")
		if !exists || p.ObservedGeneration != o.Generation {
			return fmt.Errorf("missing/stale Progressing evidence")
		}
		if p.Status == metav1.ConditionTrue {
			sawProgress = true
			finished = false
		}
		if p.Status == metav1.ConditionFalse && sawProgress {
			finished = true
		}
	}
	if !unavailableSince.IsZero() || !finished {
		return fmt.Errorf("upgrade did not finish with restored availability and Progressing=False")
	}
	last := observations[len(observations)-1]
	if !conditionsHealthy(last.Status.Conditions, last.Generation, true) {
		return fmt.Errorf("final watched conditions are not healthy")
	}
	return nil
}

// Only new components may explain an aggregate availability dip. Existing
// workloads are also checked by the Deployment watch, including for previous
// releases that did not publish status.components.
func validateUpgradeHistory(observations []observation, s Scenario) error {
	gap, err := s.gap()
	if err != nil {
		return err
	}
	if err = validateHistory(observations, gap); err != nil {
		return err
	}
	existing, added := map[string]bool{}, map[string]bool{}
	for _, c := range s.From.Components {
		existing[c.Name] = true
	}
	for _, c := range s.To.Components {
		if !existing[c.Name] {
			added[c.Name] = true
		}
	}
	for _, o := range observations {
		attributed := false
		seen := map[string]bool{}
		for _, c := range o.Status.Components {
			seen[c.Name] = true
			available, found := condition(c.Conditions, conditionAvailable)
			if existing[c.Name] && (!found || available.Status != metav1.ConditionTrue ||
				available.ObservedGeneration != o.Generation) {
				return fmt.Errorf("existing component %s lost reported availability", c.Name)
			}
			if added[c.Name] && found && available.Status == metav1.ConditionFalse &&
				available.ObservedGeneration == o.Generation {
				attributed = true
			}
		}
		if s.From.ComponentStatus || len(o.Status.Components) > 0 {
			for name := range existing {
				if !seen[name] {
					return fmt.Errorf("existing component %s missing from condition history", name)
				}
			}
		}
		available, _ := condition(o.Status.Conditions, conditionAvailable)
		if available.Status != metav1.ConditionTrue && !attributed {
			return fmt.Errorf("aggregate unavailability was not attributed to added components")
		}
	}
	return nil
}

type workloadState struct {
	Generation int64     `json:"generation"`
	UID        types.UID `json:"uid"`
	Hash       string    `json:"hash"`
}

func (h *harness) healthy(ctx context.Context, r release,
	expected ReleaseExpectations) (map[string]workloadState, error) {
	states := map[string]workloadState{}
	err := h.poll(ctx, "operand convergence", func(ctx context.Context) (bool, error) {
		obj, err := h.clients.dynamic.Resource(hfcResource).Get(ctx, hfcName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if obj.GetUID() != h.state.HFCUID {
			return false, fmt.Errorf("HyperFleetConfig was replaced")
		}
		o, err := observe(obj)
		if err != nil {
			return false, err
		}
		if !statusHealthy(o, expected) {
			return false, nil
		}
		deployments, err := h.clients.kube.AppsV1().Deployments(h.config.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		if err = validateInventory(deployments.Items, expected, h.state.HFCUID); err != nil {
			return false, nil
		}
		byName := make(map[string]*appsv1.Deployment, len(deployments.Items))
		for i := range deployments.Items {
			dep := &deployments.Items[i]
			byName[dep.Name] = dep
		}
		for _, c := range expected.Components {
			dep, found := byName[c.Deployment]
			if !found {
				return false, nil
			}
			if !deploymentReady(dep) {
				return false, nil
			}
			owned := false
			for _, owner := range dep.OwnerReferences {
				owned = owned || owner.UID == h.state.HFCUID
			}
			if !owned {
				return false, fmt.Errorf("operand %s is not owned by the original HyperFleetConfig", c.Name)
			}
			image := r.Images[normalizeImageName(c.RelatedImage)]
			want := corev1.PodSpec{Containers: []corev1.Container{{Name: c.Container, Image: image}}}
			if c.MigrationContainer != "" {
				want.InitContainers = []corev1.Container{{Name: c.MigrationContainer, Image: image}}
			}
			if matchContainerImages(dep.Spec.Template.Spec, want) != nil {
				return false, nil
			}
			if h.readyPods(ctx, dep, want) != nil {
				return false, nil
			}
			hash := dep.Spec.Template.Annotations[hashAnnotation]
			if hash == "" {
				return false, nil
			}
			states[c.Name] = workloadState{Generation: dep.Generation, UID: dep.UID, Hash: hash}
		}
		return true, nil
	})
	return states, err
}

func validateInventory(deployments []appsv1.Deployment, expected ReleaseExpectations, owner types.UID) error {
	wanted := map[string]bool{}
	for _, component := range expected.Components {
		wanted[component.Deployment] = false
	}
	for _, dep := range deployments {
		owned := false
		for _, ref := range dep.OwnerReferences {
			owned = owned || ref.UID == owner
		}
		if !owned {
			continue
		}
		if _, ok := wanted[dep.Name]; !ok {
			return fmt.Errorf("unexpected operator-owned deployment %s", dep.Name)
		}
		wanted[dep.Name] = true
	}
	for name, present := range wanted {
		if !present {
			return fmt.Errorf("missing operator-owned deployment %s", name)
		}
	}
	return nil
}

func validateHashes(before, after map[string]workloadState, to ReleaseExpectations) error {
	for _, c := range to.Components {
		old, exists := before[c.Name]
		if !exists {
			continue
		}
		next, exists := after[c.Name]
		if !exists || next.UID != old.UID {
			return fmt.Errorf("component %s was removed/recreated instead of updated", c.Name)
		}
		if c.HashChange == nil || (old.Hash != next.Hash) != *c.HashChange {
			return fmt.Errorf("component %s config hash did not match the release-pair expectation", c.Name)
		}
	}
	return nil
}

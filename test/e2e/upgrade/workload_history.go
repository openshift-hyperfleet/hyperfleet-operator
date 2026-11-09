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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
)

var deploymentResource = schema.GroupVersionResource{Group: appsGroup, Version: "v1", Resource: "deployments"}
var podResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// A whitelist keeps fixture credentials, arbitrary annotations and status
// messages out of history. Pod replacement is evidence, not itself a failure.
type workloadObservation struct {
	At              time.Time       `json:"at"`
	Event           watch.EventType `json:"event"`
	Name            string          `json:"name"`
	UID             types.UID       `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Generation      int64           `json:"generation"`
	Desired         int32           `json:"desired,omitempty"`
	Available       int32           `json:"available,omitempty"`
	Updated         int32           `json:"updated,omitempty"`
	Ready           bool            `json:"ready,omitempty"`
	Images          []string        `json:"images"`
	Deleting        bool            `json:"deleting,omitempty"`
}

func observeWorkload(obj *unstructured.Unstructured, gvr schema.GroupVersionResource,
	event watch.EventType) (workloadObservation, error) {
	o := workloadObservation{At: time.Now().UTC(), Event: event, Name: obj.GetName(), UID: obj.GetUID(),
		ResourceVersion: obj.GetResourceVersion(), Generation: obj.GetGeneration(),
		Deleting: obj.GetDeletionTimestamp() != nil}
	var spec corev1.PodSpec
	if gvr == deploymentResource {
		dep, err := fromUnstructured[appsv1.Deployment](obj)
		if err != nil {
			return o, err
		}
		o.Desired, o.Available, o.Updated = ptr.Deref(dep.Spec.Replicas, 1), dep.Status.AvailableReplicas,
			dep.Status.UpdatedReplicas
		spec = dep.Spec.Template.Spec
	} else {
		pod, err := fromUnstructured[corev1.Pod](obj)
		if err != nil {
			return o, err
		}
		spec = pod.Spec
		for _, c := range pod.Status.Conditions {
			o.Ready = o.Ready || c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
		}
	}
	for _, c := range podImages(spec) {
		o.Images = append(o.Images, c.Name+"="+c.Image)
	}
	return o, nil
}

type workloadHistory struct {
	mu      sync.Mutex
	records []workloadObservation
	err     error
	client  dynamic.ResourceInterface
	gvr     schema.GroupVersionResource
	cancel  context.CancelFunc
	done    chan struct{}
}

func startWorkloadHistory(ctx context.Context, client dynamic.ResourceInterface,
	gvr schema.GroupVersionResource) (*workloadHistory, error) {
	baseline, err := client.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	if baseline.GetResourceVersion() == "" {
		return nil, fmt.Errorf("%s list has no history resourceVersion", gvr.Resource)
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &workloadHistory{client: client, gvr: gvr, cancel: cancel, done: make(chan struct{})}
	for i := range baseline.Items {
		if err = h.record(&baseline.Items[i], watch.Added); err != nil {
			cancel()
			return nil, err
		}
	}
	options := metav1.ListOptions{ResourceVersion: baseline.GetResourceVersion(),
		AllowWatchBookmarks: true, TimeoutSeconds: ptr.To[int64](60)}
	stream, err := client.Watch(ctx, options)
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		defer close(h.done)
		err := h.read(ctx, stream, options)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.err = err
	}()
	return h, nil
}

func (h *workloadHistory) read(ctx context.Context, stream watch.Interface, options metav1.ListOptions) error {
	defer func() { stream.Stop() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-stream.ResultChan():
			if !ok {
				stream.Stop()
				if ctx.Err() != nil {
					return nil
				}
				next, err := h.client.Watch(ctx, options)
				if err != nil {
					return fmt.Errorf("%s history reconnect failed", h.gvr.Resource)
				}
				stream = next
				continue
			}
			if event.Type == watch.Error {
				return fmt.Errorf("%s watch history was lost", h.gvr.Resource)
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok || obj.GetResourceVersion() == "" {
				return fmt.Errorf("invalid %s watch object", h.gvr.Resource)
			}
			options.ResourceVersion = obj.GetResourceVersion()
			if event.Type != watch.Bookmark {
				if err := h.record(obj, event.Type); err != nil {
					return err
				}
			}
		}
	}
}

func (h *workloadHistory) record(obj *unstructured.Unstructured, event watch.EventType) error {
	o, err := observeWorkload(obj, h.gvr, event)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) >= 20000 {
		return fmt.Errorf("%s history exceeded bounded capacity", h.gvr.Resource)
	}
	h.records = append(h.records, o)
	return nil
}

func (h *workloadHistory) stop() { h.cancel(); <-h.done }

func (h *workloadHistory) snapshot() ([]workloadObservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]workloadObservation{}, h.records...), h.err
}

// Wait for the watch to consume the final list's object versions. A healthy GET
// must not let an asynchronously read watch omit an earlier failed rollout.
func (h *harness) finishWorkloadHistory(ctx context.Context, history *workloadHistory) error {
	checkpoint, err := history.client.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	return h.poll(ctx, history.gvr.Resource+" history checkpoint", func(context.Context) (bool, error) {
		records, err := history.snapshot()
		if err != nil {
			return false, err
		}
		seen := map[types.UID]map[string]bool{}
		for _, o := range records {
			if seen[o.UID] == nil {
				seen[o.UID] = map[string]bool{}
			}
			seen[o.UID][o.ResourceVersion] = true
		}
		for _, obj := range checkpoint.Items {
			if !seen[obj.GetUID()][obj.GetResourceVersion()] {
				return false, nil
			}
		}
		return true, nil
	})
}

func validateWorkloadAvailability(records []workloadObservation, from ReleaseExpectations,
	before map[string]workloadState) error {
	expected := map[string]types.UID{}
	for _, c := range from.Components {
		expected[c.Deployment] = before[c.Name].UID
	}
	seen := map[string]bool{}
	for _, o := range records {
		uid, exists := expected[o.Name]
		if !exists {
			continue
		}
		seen[o.Name] = true
		if o.UID != uid || o.Event == watch.Deleted || o.Deleting || o.Desired < 1 || o.Available < o.Desired {
			return fmt.Errorf("existing deployment %s lost availability or identity during upgrade", o.Name)
		}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("deployment history omitted an existing component")
	}
	return nil
}

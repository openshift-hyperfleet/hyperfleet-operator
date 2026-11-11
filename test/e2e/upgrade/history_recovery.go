// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// A resync snapshot cannot recover intervening events. Retain both watch
// checkpoints in conditions.json instead of presenting it as continuous history.
type conditionResync struct {
	FromResourceVersion string `json:"fromResourceVersion"`
	ResourceVersion     string `json:"resourceVersion"`
}

func expiredWatch(err error) bool {
	return apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
}

func (h *history) openWatch(ctx context.Context, client dynamic.Interface,
	options *metav1.ListOptions, resync bool) (watch.Interface, error) {
	// A CRD update can expire both an active stream and its reconnect request.
	// Bound consecutive HTTP rejections, and back off every fresh snapshot.
	for range 3 {
		if resync {
			if !waitForWatchReconnect(ctx) {
				return nil, ctx.Err()
			}
			if err := h.resync(ctx, client, options); err != nil {
				return nil, err
			}
		}
		stream, err := client.Resource(hfcResource).Watch(ctx, *options)
		if err == nil {
			return stream, nil
		}
		if !expiredWatch(err) {
			// API errors may contain configuration or credentials from older
			// releases. Do not copy arbitrary server messages into artifacts.
			return nil, fmt.Errorf("condition history watch request failed")
		}
		resync = true
	}
	return nil, fmt.Errorf("condition history repeatedly expired while restarting watch")
}

func (h *history) resync(ctx context.Context, client dynamic.Interface, options *metav1.ListOptions) error {
	listCtx, cancel := context.WithTimeout(ctx, h.listTimeout)
	defer cancel()
	// The HFC's own resourceVersion may predate the new CRD's watch cache even
	// after a fresh GET. Use a consistent filtered LIST's collection version as
	// the watch checkpoint, keeping the object's actual version in its observation.
	current, err := client.Resource(hfcResource).List(listCtx, metav1.ListOptions{FieldSelector: options.FieldSelector})
	if err != nil {
		return fmt.Errorf("condition history resync list failed")
	}
	if len(current.Items) != 1 || current.GetResourceVersion() == "" {
		return fmt.Errorf("condition history resync requires the original HyperFleetConfig and a list resourceVersion")
	}
	marker := &conditionResync{FromResourceVersion: options.ResourceVersion, ResourceVersion: current.GetResourceVersion()}
	if err = h.record(&current.Items[0], marker); err != nil {
		return err
	}
	options.ResourceVersion = current.GetResourceVersion()
	return nil
}

func (h *history) record(obj *unstructured.Unstructured, resync *conditionResync) error {
	o, err := observe(obj)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	baseline := h.observations[0]
	if obj.GetName() != hfcName || o.UID != baseline.UID || o.Generation != baseline.Generation ||
		obj.GetDeletionTimestamp() != nil || o.ResourceVersion == "" {
		return fmt.Errorf("HyperFleetConfig identity/spec changed or is deleting during condition history")
	}
	if len(h.observations) >= 10000 {
		return fmt.Errorf("condition history exceeded bounded capacity")
	}
	o.Resync = resync
	h.observations = append(h.observations, o)
	return nil
}

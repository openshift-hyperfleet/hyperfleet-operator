// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// openHistoryWatch bounds the entire Watch call, including client-go retries,
// without applying an HTTP-client or context deadline to the resulting stream.
// TimeoutSeconds alone cannot bound a server that withholds response headers.
func openHistoryWatch(ctx context.Context, client dynamic.ResourceInterface,
	options metav1.ListOptions, timeout time.Duration) (watch.Interface, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(timeout, cancel)
	stream, err := client.Watch(watchCtx, options)
	if !timer.Stop() {
		cancel()
		if stream != nil {
			stream.Stop()
		}
		return nil, context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return &historyWatch{Interface: stream, cancel: cancel}, nil
}

type historyWatch struct {
	watch.Interface
	cancel context.CancelFunc
}

func (w *historyWatch) Stop() {
	w.cancel()
	w.Interface.Stop()
}

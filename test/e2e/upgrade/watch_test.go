// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type testHistoryHandle struct {
	stop func()
	done <-chan struct{}
	err  func() error
}

func startTestHistory(t *testing.T, ctx context.Context, client dynamic.Interface,
	gvr schema.GroupVersionResource, timeout time.Duration) (testHistoryHandle, error) {
	t.Helper()
	if gvr == hfcResource {
		h, err := startHistory(ctx, client, historyObject(t, "10", metav1.ConditionFalse), timeout)
		if err != nil {
			return testHistoryHandle{}, err
		}
		return testHistoryHandle{h.stop, h.done, func() error { _, err := h.snapshot(); return err }}, nil
	}
	h, err := startWorkloadHistory(ctx, client.Resource(gvr).Namespace(testNamespace), gvr, timeout)
	if err != nil {
		return testHistoryHandle{}, err
	}
	return testHistoryHandle{h.stop, h.done, func() error { _, err := h.snapshot(); return err }}, nil
}

func TestHistoryWatchEstablishmentTimeout(t *testing.T) {
	for _, gvr := range []schema.GroupVersionResource{hfcResource, deploymentResource, podResource} {
		for _, phase := range []string{"initial", "reconnect"} {
			t.Run(gvr.Resource+"/"+phase, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					watching, _ := strconv.ParseBool(r.URL.Query().Get("watch"))
					if !watching {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(&metav1.List{
							TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "List"},
							ListMeta: metav1.ListMeta{ResourceVersion: "10"}})
						return
					}
					if calls.Add(1) == 1 && phase == "reconnect" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
						return // Force a reconnect, then withhold its response headers.
					}
					<-r.Context().Done()
				}))
				defer server.Close()
				client, err := newWatchClient(&rest.Config{Host: server.URL})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				h, err := startTestHistory(t, ctx, client, gvr, 50*time.Millisecond)
				if phase == "initial" {
					if err == nil {
						h.stop()
						t.Fatal("watch without response headers unexpectedly started")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					defer h.stop()
					select {
					case <-h.done:
						err = h.err()
					case <-ctx.Done():
						t.Fatal("reconnect outlived its establishment timeout")
					}
				}
				if err == nil || ctx.Err() != nil {
					t.Fatalf("watch did not fail within its establishment timeout: %v (parent: %v)", err, ctx.Err())
				}
			})
		}
	}
}

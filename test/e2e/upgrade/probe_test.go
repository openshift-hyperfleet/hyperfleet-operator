// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testProbeID = "record-id"

func probeHarness(t *testing.T) *harness {
	t.Helper()
	h := fakeHarness(t)
	h.config.Timeout = 50 * time.Millisecond
	h.config.Scenario.DataProbe = ProbeConfig{Service: testAPIImageName, Port: 8000,
		Collection: "/api/hyperfleet/v1/channels", Body: map[string]any{
			"kind": "Channel", fieldSpec: map[string]any{"enabled_regex": ".*"},
			fieldLabels: map[string]any{"purpose": "upgrade-test"}}}
	return h
}

func ownedProbe(h *harness) map[string]any {
	return map[string]any{"id": testProbeID, fieldName: h.state.RunID,
		fieldSpec: map[string]any{"enabled_regex": ".*"}, fieldLabels: map[string]any{probeOwnerLabel: h.state.RunID}}
}

func TestLostProbeResponseRecoversOnlyOwnedRecordAndWaitsForDeletion(t *testing.T) {
	h := probeHarness(t)
	var created map[string]any
	posts, deletes, reads := 0, 0, 0
	h.probeCall = func(_ context.Context, _ ProbeConfig, method, path string,
		body map[string]any) (map[string]any, int, error) {
		switch method {
		case http.MethodPost:
			posts++
			intent := savedReceipt(t, h)
			if !intent.ProbePending || intent.Probe.Collection == "" || intent.Probe.Body != nil {
				t.Fatal("POST preceded durable, non-sensitive probe intent")
			}
			created = body
			created["id"] = testProbeID
			return nil, 0, errors.New("response lost after committed POST")
		case http.MethodDelete:
			deletes++
			if h.state.ProbeID != testProbeID {
				t.Fatal("recovery did not record the owned ID before DELETE")
			}
			return nil, http.StatusAccepted, nil
		case http.MethodGet:
			u, err := url.Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			if u.RawQuery != "" {
				if u.Query().Get("search") != "name='run' and labels.upgrade_run='run'" || u.Query().Get("size") != "2" {
					t.Fatal("recovery query was not scoped by both name and ownership")
				}
				return map[string]any{"items": []any{created}}, http.StatusOK, nil
			}
			reads++
			if savedReceipt(t, h).ProbeID != testProbeID {
				t.Fatal("receipt cleared before confirmed deletion")
			}
			if reads == 1 {
				return created, http.StatusOK, nil
			}
			return nil, http.StatusNotFound, nil
		default:
			t.Fatalf("unexpected method %s", method)
			return nil, 0, nil
		}
	}
	if _, err := h.createProbe(t.Context()); err == nil {
		t.Fatal("expected failed POST response")
	}
	h.state = savedReceipt(t, h)
	if err := h.deleteProbe(t.Context()); err != nil {
		t.Fatal("cleanup could not recover committed probe", err)
	}
	state := savedReceipt(t, h)
	if posts != 1 || deletes != 1 || reads != 2 || state.ProbeID != "" || state.ProbePending || state.Probe.Service != "" {
		t.Fatalf("incorrect recovery/deletion lifecycle: posts=%d deletes=%d reads=%d state=%+v",
			posts, deletes, reads, state)
	}
}

func TestPendingProbeRecoveryRejectsUnknownOwnershipAndOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items func(*harness) []any
	}{
		{"absent", func(*harness) []any { return []any{} }},
		{"ambiguous", func(h *harness) []any { return []any{ownedProbe(h), ownedProbe(h)} }},
		{"foreign", func(h *harness) []any {
			p := ownedProbe(h)
			p[fieldLabels].(map[string]any)[probeOwnerLabel] = "foreign-run"
			return []any{p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := probeHarness(t)
			h.state.Probe, h.state.ProbePending = h.config.Scenario.DataProbe, true
			h.state.Probe.Body = nil
			if err := h.saveState(); err != nil {
				t.Fatal(err)
			}
			h.probeCall = func(_ context.Context, _ ProbeConfig, method, _ string,
				_ map[string]any) (map[string]any, int, error) {
				if method != http.MethodGet {
					t.Fatal("attempted deletion without establishing unique ownership")
				}
				return map[string]any{"items": tc.items(h)}, http.StatusOK, nil
			}
			if err := h.deleteProbe(t.Context()); err == nil {
				t.Fatal("unresolved probe cleanup passed")
			}
			if !savedReceipt(t, h).ProbePending {
				t.Fatal("lost unresolved probe intent")
			}
		})
	}
}

func TestFinalizationTimeoutPreservesReceiptAndAllowsKubernetesCleanup(t *testing.T) {
	h := probeHarness(t)
	h.state.Probe, h.state.ProbeID = h.config.Scenario.DataProbe, testProbeID
	h.state.Probe.Body = nil
	if err := h.saveState(); err != nil {
		t.Fatal(err)
	}
	h.probeCall = func(_ context.Context, _ ProbeConfig, method, _ string,
		_ map[string]any) (map[string]any, int, error) {
		if method == http.MethodDelete {
			return nil, http.StatusAccepted, nil
		}
		return ownedProbe(h), http.StatusOK, nil // Never finishes deleting.
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := h.cleanup(ctx); err == nil {
		t.Fatal("unfinished API record deletion passed cleanup")
	}
	if state := savedReceipt(t, h); state.ProbeID != testProbeID || state.Cleaned {
		t.Fatal("failed finalization lost its cleanup receipt")
	}
	_, err := h.clients.kube.CoreV1().Namespaces().Get(t.Context(), h.state.Namespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("record polling exhausted the Kubernetes teardown budget", err)
	}
}

func TestProbeCreationPreservesScenarioLabels(t *testing.T) {
	h := probeHarness(t)
	h.probeCall = func(_ context.Context, _ ProbeConfig, method, _ string,
		body map[string]any) (map[string]any, int, error) {
		if method != http.MethodPost {
			t.Fatal("unexpected request")
		}
		body["id"] = testProbeID
		return body, http.StatusCreated, nil
	}
	if _, err := h.createProbe(t.Context()); err != nil {
		t.Fatal("create owned probe", err)
	}
	labels := h.config.Scenario.DataProbe.Body[fieldLabels].(map[string]any)
	if labels["purpose"] != "upgrade-test" || labels[probeOwnerLabel] != nil {
		t.Fatal("probe creation mutated the scenario's labels")
	}
	if savedReceipt(t, h).ProbePending || h.state.ProbeID != testProbeID {
		t.Fatal("confirmed creation retained ambiguous intent")
	}
}

func TestProbeRecoveryDoesNotInterpolateUntrustedSearch(t *testing.T) {
	h := probeHarness(t)
	h.state.RunID = "x' or name!='x"
	if err := h.recoverProbe(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid run identity") {
		t.Fatal("invalid receipt identity reached API query construction", err)
	}
}

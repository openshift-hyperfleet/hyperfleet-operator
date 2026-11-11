// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func testKubeconfig(t *testing.T, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
current-context: test
`, endpoint)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClientDoesNotRetryAmbiguousCreates(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status: "Failure", Code: 500, Reason: metav1.StatusReasonInternalError})
	}))
	defer server.Close()
	client, err := newClients(testKubeconfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = client.kube.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{})
	if calls.Load() != 1 || !apierrors.IsInternalError(err) || createDefinitelyRejected(err) {
		t.Fatal("ambiguous Create was retried or treated as definitive", err)
	}
}

func TestStandaloneCleanupHonorsEarlierDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	h := fakeHarness(t)
	if err := h.saveState(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := Cleanup(ctx, testKubeconfig(t, server.URL), h.config.ArtifactDir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cleanup did not honor caller's deadline", err)
	}
}

func TestInstalledCSVMatchesImmutableBundle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*release)
		wantError bool
	}{
		{"matching", func(*release) {}, false},
		{"version drift", func(r *release) { r.CSV.Spec.Version.Major = 99 }, true},
		{"related image drift", func(r *release) {
			image := testImage(apiComponentName, "c")
			r.CSV.Spec.RelatedImages[0].Image = image
			r.CSV.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0].Spec.Template.Spec.Containers[0].Env[0].Value = image
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := testRelease(t, testTargetVersion, "b")
			installed := testRelease(t, testTargetVersion, "b")
			tc.change(&installed)
			data, err := json.Marshal(installed.CSV)
			if err != nil {
				t.Fatal(err)
			}
			var obj unstructured.Unstructured
			if err = json.Unmarshal(data, &obj.Object); err != nil {
				t.Fatal(err)
			}
			if err = validateInstalledCSV(&obj, expected); (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestFinishRetainsOnlySuccessfulRequestedRuns(t *testing.T) {
	for _, failed := range []bool{false, true} {
		for _, retain := range []bool{false, true} {
			t.Run(fmt.Sprintf("failed=%t/retain=%t", failed, retain), func(t *testing.T) {
				h := fakeHarness(t)
				var input error
				if failed {
					input = errors.New("upgrade failed")
				}
				err := h.finish(input, retain)
				if (err != nil) != failed {
					t.Fatalf("unexpected finish result: %v", err)
				}
				if failed && !errors.Is(err, input) {
					t.Fatal("original failure lost")
				}
				wantRetain := !failed && retain
				if h.state.Cleaned == wantRetain {
					t.Fatal("incorrect cleanup decision")
				}
				if wantRetain {
					assertNoDeletes(t, h)
				} else {
					assertDeletesCarryUIDPreconditions(t, h)
				}
				data, err := os.ReadFile(filepath.Join(h.config.ArtifactDir, "result.json"))
				if err != nil {
					t.Fatal(err)
				}
				var result struct{ Passed, Retained bool }
				if err = json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				if result.Passed == failed || result.Retained != wantRetain {
					t.Fatal("incorrect result artifact")
				}
			})
		}
	}
}

func TestDiagnosticsReportsVersionFailure(t *testing.T) {
	h := fakeHarness(t)
	kube := h.clients.kube.(*kubefake.Clientset)
	kube.PrependReactor("get", "version", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("discovery unavailable")
	})
	if err := h.diagnostics(t.Context()); err == nil || !strings.Contains(err.Error(), "collect cluster version") {
		t.Fatal("missing cluster version was silently accepted", err)
	}
}

func TestDiagnosticsCoversEncodedAndShortSecrets(t *testing.T) {
	secret := "private?value+with/space>"
	for name, encoded := range map[string]string{
		"base64":         base64.StdEncoding.EncodeToString([]byte(secret)),
		"raw base64":     base64.RawStdEncoding.EncodeToString([]byte(secret)),
		"URL base64":     base64.URLEncoding.EncodeToString([]byte(secret)),
		"raw URL base64": base64.RawURLEncoding.EncodeToString([]byte(secret)),
		"hex":            hex.EncodeToString([]byte(secret)),
		"upper hex":      strings.ToUpper(hex.EncodeToString([]byte(secret))),
		"path":           url.PathEscape(secret),
	} {
		t.Run(name, func(t *testing.T) {
			if got := redact(encoded, []string{secret}); got != redactionMarker {
				t.Fatal("encoded credential survived")
			}
		})
	}
	for _, secret := range []string{"1", "true", "admin", "!!!", "A"} {
		t.Run(secret, func(t *testing.T) {
			if got := redact("credential="+secret, []string{secret}); got != "credential=[REDACTED]" {
				t.Fatal("short credential was skipped or marker corrupted")
			}
		})
	}
	if got := redact("private A", []string{"private", "A"}); got != "[REDACTED] [REDACTED]" {
		t.Fatal("redaction recursively rewrote its markers")
	}
	if got := redact("Bearer A-private-token", []string{"A"}); got != redactionMarker {
		t.Fatal("short secret prevented full bearer-token redaction")
	}
	if got := redact("eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1c2VyIn0.signature", []string{"1"}); got != redactionMarker {
		t.Fatal("short secret prevented full JWT redaction")
	}
}

func TestCleanupBeforeClusterMutations(t *testing.T) {
	h := fakeHarness(t)
	h.state = receipt{RunID: testRunID, Namespace: testNamespace}
	if err := h.saveState(); err != nil {
		t.Fatal(err)
	}
	// No kubeconfig is opened for a durable receipt proving no cluster mutation.
	if err := Cleanup(t.Context(), "/missing/kubeconfig", h.config.ArtifactDir); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(t.Context(), "/missing/kubeconfig", t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "no cleanup receipt recorded") {
		t.Fatal("missing receipt was ambiguous", err)
	}
	h.state.LeasePending = true
	if err := h.saveState(); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(t.Context(), "/missing/kubeconfig", h.config.ArtifactDir); err == nil {
		t.Fatal("receipt with mutation but no cluster identity accepted")
	}
}

func TestRetriedCreateIsNotADefinitiveRejection(t *testing.T) {
	err := apierrors.NewBadRequest("rejected")
	if !createDefinitelyRejected(err) {
		t.Fatal("direct API rejection should be definitive")
	}
	if createDefinitelyRejected(fmt.Errorf("earlier attempt may have committed: %w", err)) {
		t.Fatal("wrapped retry error lost its pending creation intent")
	}
}

func TestApprovalIntentPrecedesFailedPatch(t *testing.T) {
	h, plan := csvApprovalHarness(t)
	pair := h.pair
	patchErr := errors.New("approval response lost")
	patched := false
	h.clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("patch", "installplans",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			patched = true
			intent := savedReceipt(t, h)
			approval := intent.CSVApprovals[pair.To.CSV.Name]
			if !intent.Approved || approval.PlanName != plan.GetName() || approval.PlanUID != plan.GetUID() ||
				approval.CSVResourceVersion != "10" {
				t.Fatal("approval patch preceded durable intent")
			}
			return true, nil, patchErr
		})
	if _, err := h.approve(t.Context(), pair.To, ""); !errors.Is(err, patchErr) || !patched {
		t.Fatal("did not exercise lost approval response", err)
	}
	if !savedReceipt(t, h).Approved {
		t.Fatal("lost pending approval")
	}
}

func TestWatchClientDoesNotInheritRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
			"apiVersion": "hyperfleet.redhat.com/v1alpha1", fieldKind: "HyperFleetConfig",
			fieldMetadata: map[string]any{fieldResourceVersion: "2"}}})
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL, Timeout: 10 * time.Millisecond}
	client, err := newWatchClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if config.Timeout != 10*time.Millisecond {
		t.Fatal("ordinary request timeout was mutated")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stream, err := client.Resource(hfcResource).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()
	select {
	case event, ok := <-stream.ResultChan():
		if !ok || event.Type != watch.Bookmark {
			t.Fatal("watch was truncated by ordinary request timeout")
		}
	case <-ctx.Done():
		t.Fatal("watch did not deliver bookmark")
	}
}

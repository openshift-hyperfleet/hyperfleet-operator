// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	cleanupTimeout      = 10 * time.Minute
	defaultPollInterval = 2 * time.Second
)

type harness struct {
	config          Config
	clients         clients
	pair            releasePair
	state           receipt
	subscriptionUID types.UID
	history         *history
	workloads       []*workloadHistory
	probeCall       probeRequestFunc
}

// Run executes the standalone upgrade and always attempts diagnostics and
// cleanup. A successful retained run is cleaned by the outer workflow using
// Cleanup; a failed run is never retained merely because retention was requested.
func Run(ctx context.Context, c Config) (result error) {
	retain := false
	if value := os.Getenv(retainEnv); value != "" {
		var err error
		retain, err = strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("UPGRADE_RETAIN_ON_SUCCESS must be a boolean")
		}
	}
	fixtures, err := loadFixtures(c.Scenario.Fixtures)
	if err != nil {
		return err
	}
	preflightCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	pair, err := preflight(preflightCtx, c, runCommand)
	cancel()
	if err != nil {
		return err
	}
	client, err := newClients(c.Kubeconfig)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(c.ArtifactDir, 0700); err != nil {
		return err
	}
	// O_EXCL prevents reusing an artifact directory and losing an old receipt.
	marker, err := os.OpenFile(filepath.Join(c.ArtifactDir, "run.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("ARTIFACT_DIR must be unique for each run: %w", err)
	}
	if err = marker.Close(); err != nil {
		return err
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	h := &harness{config: c, clients: client, pair: pair,
		state: receipt{RunID: "upgrade-" + hex.EncodeToString(random[:]),
			Namespace: c.Namespace, CSVs: []string{pair.From.CSV.Name, pair.To.CSV.Name}}}
	defer func() { result = h.finish(result, retain) }()
	// Record the no-mutations state before platform checks can fail. Recovery
	// can then distinguish an early failure from a missing or lost receipt.
	if err = h.saveState(); err != nil {
		return err
	}
	if err = h.writeJSON("run.json", map[string]any{"runID": h.state.RunID, "catalog": c.Catalog,
		"fromBundle": c.FromBundle, "toBundle": c.ToBundle, fieldChannel: c.Channel, fieldNamespace: c.Namespace,
		"fromCSV": pair.From.CSV.Name, "toCSV": pair.To.CSV.Name,
		"fromImages": pair.From.Images, "toImages": pair.To.Images,
		"opmVersion": pair.OPMVersion, "goVersion": runtime.Version(),
		"fromComponents": c.Scenario.From, "toComponents": c.Scenario.To}); err != nil {
		return err
	}
	platformCtx, platformCancel := context.WithTimeout(ctx, c.Timeout)
	err = h.validatePlatforms(platformCtx, fixtures, runCommand)
	platformCancel()
	if err != nil {
		return err
	}
	if err = h.prepare(ctx, fixtures); err != nil {
		return err
	}
	if err = h.createOLM(ctx); err != nil {
		return err
	}
	initialPlan, err := h.approve(ctx, pair.From, "")
	if err != nil {
		return err
	}
	if err = h.waitInstalled(ctx, pair.From, initialPlan); err != nil {
		return err
	}
	cr := (&unstructured.Unstructured{Object: c.Scenario.HyperFleetConfig}).DeepCopy()
	cr.SetLabels(map[string]string{runLabel: h.state.RunID})
	created, err := h.clients.dynamic.Resource(hfcResource).Create(ctx, cr, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create HyperFleetConfig: %w", err)
	}
	h.state.HFCUID = created.GetUID()
	if err = h.saveState(); err != nil {
		return err
	}
	before, err := h.healthy(ctx, pair.From, c.Scenario.From)
	if err != nil {
		return err
	}
	fingerprint, err := h.createProbe(ctx)
	if err != nil {
		return err
	}
	if err = h.upgrade(ctx, initialPlan, before); err != nil {
		return err
	}
	if err = h.verifyProbe(ctx, fingerprint); err != nil {
		return err
	}
	if err = h.deleteProbe(ctx); err != nil {
		return err
	}
	return h.checkHashTriggers(ctx)
}

func (h *harness) upgrade(ctx context.Context, initialPlan string, before map[string]workloadState) error {
	for _, gvr := range []schema.GroupVersionResource{deploymentResource, podResource} {
		history, err := startWorkloadHistory(ctx, h.clients.watches.Resource(gvr).Namespace(h.config.Namespace),
			gvr, h.config.Timeout)
		if err != nil {
			return err
		}
		h.workloads = append(h.workloads, history)
	}
	baseline, err := h.clients.dynamic.Resource(hfcResource).Get(ctx, hfcName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	h.history, err = startHistory(ctx, h.clients.watches, baseline, h.config.Timeout)
	if err != nil {
		return err
	}
	targetPlan, err := h.approve(ctx, h.pair.To, initialPlan)
	if err != nil {
		return err
	}
	if err = h.waitInstalled(ctx, h.pair.To, targetPlan); err != nil {
		return err
	}
	after, err := h.healthy(ctx, h.pair.To, h.config.Scenario.To)
	if err != nil {
		return err
	}
	if err = validateHashes(before, after, h.config.Scenario.To); err != nil {
		return err
	}
	exactHashes, err := h.verifyExactTargetHashes(ctx, h.config.Scenario.To.Components)
	if err != nil {
		return err
	}
	if err = h.writeJSON("exact-hashes.json", exactHashes); err != nil {
		return err
	}
	// Allow the watch consumer to catch up to the final GET. Require its own
	// observed False -> True -> False sequence; a healthy GET alone cannot pass.
	var evidenceErr error
	if err = h.poll(ctx, "upgrade condition transition evidence", func(ctx context.Context) (bool, error) {
		current, readErr := h.clients.dynamic.Resource(hfcResource).Get(ctx, hfcName, metav1.GetOptions{})
		if readErr != nil {
			return false, readErr
		}
		currentStatus, readErr := observe(current)
		if readErr != nil {
			return false, readErr
		}
		if !statusHealthy(currentStatus, h.config.Scenario.To) {
			return false, nil
		}
		observations, watchErr := h.history.snapshot()
		if watchErr != nil {
			return false, watchErr
		}
		caughtUp := false
		for _, observation := range observations {
			caughtUp = caughtUp || observation.ResourceVersion == current.GetResourceVersion()
		}
		if !caughtUp {
			return false, nil
		}
		evidenceErr = validateUpgradeHistory(observations, h.config.Scenario)
		return evidenceErr == nil, evidenceErr
	}); err != nil {
		return combineErrors(err, evidenceErr)
	}
	h.history.stop()
	observations, err := h.history.snapshot()
	if err != nil {
		return err
	}
	if err = validateUpgradeHistory(observations, h.config.Scenario); err != nil {
		return err
	}
	for _, history := range h.workloads {
		if err = h.finishWorkloadHistory(ctx, history); err != nil {
			return err
		}
		history.stop()
		records, historyErr := history.snapshot()
		if historyErr != nil {
			return historyErr
		}
		if history.gvr == deploymentResource {
			if err = validateWorkloadAvailability(records, h.config.Scenario.From, before); err != nil {
				return err
			}
		}
	}
	if err = h.writeJSON("upgrade-workloads.json", map[string]any{"before": before, "after": after,
		"initialInstallPlan": initialPlan, "upgradeInstallPlan": targetPlan}); err != nil {
		return err
	}
	return nil
}

func (h *harness) checkHashTriggers(ctx context.Context) error {
	before, err := h.healthy(ctx, h.pair.To, h.config.Scenario.To)
	if err != nil {
		return err
	}
	// A harmless CR annotation must not alter the operand templates.
	cr, err := h.clients.dynamic.Resource(hfcResource).Get(ctx, hfcName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cr.GetUID() != h.state.HFCUID {
		return fmt.Errorf("HyperFleetConfig replaced before hash probe")
	}
	annotations := cr.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[noopAnnotation] = h.state.RunID
	cr.SetAnnotations(annotations)
	if _, err = h.clients.dynamic.Resource(hfcResource).Update(ctx, cr, metav1.UpdateOptions{}); err != nil {
		return err
	}
	settle, _ := h.config.Scenario.settle()
	if err = h.unchangedFor(ctx, before, settle); err != nil {
		return err
	}
	cr, err = h.clients.dynamic.Resource(hfcResource).Get(ctx, hfcName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	secretName := stringField(cr, fieldSpec, apiComponentName, "database", "secretRef", fieldName)
	secret, err := h.clients.kube.CoreV1().Secrets(h.config.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read test database Secret failed")
	}
	if secret.Labels[runLabel] != h.state.RunID {
		return fmt.Errorf("database Secret is not owned by this test")
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[hashProbeAnnotation] = h.state.RunID
	// Update preserves data; resourceVersion is included by the client for CAS.
	if _, err = h.clients.kube.CoreV1().Secrets(h.config.Namespace).Update(ctx, secret,
		metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update test Secret metadata failed")
	}
	if err = h.poll(ctx, "API hash change after Secret metadata update", func(ctx context.Context) (bool, error) {
		dep, err := h.clients.kube.AppsV1().Deployments(h.config.Namespace).Get(ctx,
			h.config.Scenario.To.api().Deployment, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		old := before[apiComponentName]
		return dep.Spec.Template.Annotations[hashAnnotation] != old.Hash && dep.Generation > old.Generation, nil
	}); err != nil {
		return err
	}
	after, err := h.healthy(ctx, h.pair.To, h.config.Scenario.To)
	if err != nil {
		return err
	}
	if err = validateSecretRollout(before[apiComponentName], after[apiComponentName]); err != nil {
		return err
	}
	for name, old := range before {
		if name != apiComponentName && after[name] != old {
			return fmt.Errorf("database Secret change rolled unrelated component %s", name)
		}
	}
	exactAPIHash, err := h.verifyExactTargetHashes(ctx, []Component{h.config.Scenario.To.api()})
	if err != nil {
		return err
	}
	if err = h.unchangedFor(ctx, after, settle); err != nil {
		return err
	}
	return h.writeJSON("hash-probes.json", map[string]any{"before": before, "after": after,
		"noopAnnotationPassed": true, "exactAPIHash": exactAPIHash[apiComponentName]})
}

func validateSecretRollout(before, after workloadState) error {
	if after.UID != before.UID || after.Hash == "" || after.Hash == before.Hash ||
		after.Generation <= before.Generation {
		return fmt.Errorf("database Secret update did not persist a new API config hash and rollout")
	}
	return nil
}

func (h *harness) unchangedFor(ctx context.Context, before map[string]workloadState, duration time.Duration) error {
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(h.config.PollInterval)
	defer ticker.Stop()
	for {
		for _, c := range h.config.Scenario.To.Components {
			dep, err := h.clients.kube.AppsV1().Deployments(h.config.Namespace).Get(ctx, c.Deployment,
				metav1.GetOptions{})
			if err != nil {
				return err
			}
			got := workloadState{UID: dep.UID, Generation: dep.Generation,
				Hash: dep.Spec.Template.Annotations[hashAnnotation]}
			if got != before[c.Name] {
				return fmt.Errorf("unexpected rollout of %s during stable observation window", c.Name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-ticker.C:
		}
	}
}

func (h *harness) finish(result error, retain bool) error {
	for _, history := range h.workloads {
		history.stop()
		records, historyErr := history.snapshot()
		result = combineErrors(result, historyErr)
		result = combineErrors(result, h.writeJSON(history.gvr.Resource+"-history.json", records))
	}
	if h.history != nil {
		h.history.stop()
		observations, historyErr := h.history.snapshot()
		result = combineErrors(result, historyErr)
		result = combineErrors(result, h.writeJSON("conditions.json", observations))
	}
	diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	result = combineErrors(result, h.diagnostics(diagnosticCtx))
	diagnosticCancel()
	if result != nil || !retain {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
		result = combineErrors(result, h.cleanup(cleanupCtx))
		cleanupCancel()
	}
	outcome := map[string]any{"passed": result == nil, "retained": result == nil && retain}
	if writeErr := h.writeJSON("result.json", outcome); writeErr != nil {
		result = combineErrors(result, writeErr)
		if retain && !h.state.Cleaned {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
			result = combineErrors(result, h.cleanup(cleanupCtx))
			cleanupCancel()
		}
	}
	return result
}

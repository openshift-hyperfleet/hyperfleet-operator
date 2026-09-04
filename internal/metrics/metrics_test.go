/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// TestObserveReconcileRecordsASample verifies ObserveReconcile records a
// duration observation into the reconcile histogram.
func TestObserveReconcileRecordsASample(t *testing.T) {
	ObserveReconcile(150 * time.Millisecond)

	var m dto.Metric
	if err := ReconcileDuration.Write(&m); err != nil {
		t.Fatalf("writing histogram: %v", err)
	}
	if got := m.GetHistogram().GetSampleCount(); got < 1 {
		t.Errorf("reconcile duration sample count = %d, want >= 1", got)
	}
}

// TestIncReconcileErrorCountsByReason verifies IncReconcileError increments the
// error counter for the series identified by the given reason label.
func TestIncReconcileErrorCountsByReason(t *testing.T) {
	// Unique reason so the assertion is independent of other tests in this package.
	const reason = "test-reason"
	IncReconcileError(reason)

	if got := testutil.ToFloat64(ReconcileErrors.WithLabelValues(reason)); got != 1 {
		t.Errorf("reconcile errors{reason=%q} = %v, want 1", reason, got)
	}
}

// TestSetOperandReadyTogglesGauge verifies SetOperandReady drives the operand
// readiness gauge to 1 when ready and back to 0 when not.
func TestSetOperandReadyTogglesGauge(t *testing.T) {
	const operand = "test-ready"

	SetOperandReady(operand, true)
	if got := testutil.ToFloat64(OperandReady.WithLabelValues(operand)); got != 1 {
		t.Errorf("operand_ready{operand=%q} = %v, want 1", operand, got)
	}

	SetOperandReady(operand, false)
	if got := testutil.ToFloat64(OperandReady.WithLabelValues(operand)); got != 0 {
		t.Errorf("operand_ready{operand=%q} = %v, want 0", operand, got)
	}
}

// TestIncOperandRolloutCountsByTrigger verifies IncOperandRollout increments the
// rollout counter for the series keyed by operand and trigger.
func TestIncOperandRolloutCountsByTrigger(t *testing.T) {
	const operand = "test-rollout"
	IncOperandRollout(operand, TriggerImage)

	if got := testutil.ToFloat64(OperandRollouts.WithLabelValues(operand, TriggerImage)); got != 1 {
		t.Errorf("operand_rollouts{operand=%q,trigger=%q} = %v, want 1", operand, TriggerImage, got)
	}
}

// TestSetAppliedConfigHashKeepsASingleSeries verifies SetAppliedConfigHash resets
// the info gauge so only the latest hash's series exists at any time.
func TestSetAppliedConfigHashKeepsASingleSeries(t *testing.T) {
	SetAppliedConfigHash("hash-one")
	SetAppliedConfigHash("hash-two")

	// The reset in SetAppliedConfigHash must drop the previous hash's series so the
	// gauge never accumulates stale series over the operator's lifetime.
	if got := testutil.CollectAndCount(AppliedConfig); got != 1 {
		t.Errorf("applied_config_info series count = %d, want 1", got)
	}
	if got := testutil.ToFloat64(AppliedConfig.WithLabelValues("hash-two")); got != 1 {
		t.Errorf("applied_config_info{hash=hash-two} = %v, want 1", got)
	}
}

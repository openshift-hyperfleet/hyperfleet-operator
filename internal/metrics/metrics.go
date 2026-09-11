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

// Package metrics defines the operator's custom Prometheus collectors and the
// helpers the reconciler uses to record them. It follows the HyperFleet metrics
// standard:
//
//   - names are hyperfleet_operator_<name>_<unit> (Namespace "hyperfleet",
//     Subsystem "operator");
//   - every series carries the standard component/version const labels;
//   - durations are histograms in seconds, counters end in _total.
//
// Collectors register into controller-runtime's global registry, so they are
// served on the same /metrics endpoint (and through the same ServiceMonitor) as
// the built-in controller_runtime_* metrics — no second metrics server.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/version"
)

const (
	// namespace and subsystem compose the hyperfleet_operator_ prefix mandated by
	// the metrics standard for this component.
	namespace = "hyperfleet"
	subsystem = "operator"

	// Component is the value of the standard "component" label for the operator.
	Component = "operator"
)

// Rollout trigger label values for OperandRollouts. Kept as constants so the set
// stays closed and low-cardinality.
const (
	// TriggerCreate is recorded the first time an operand's workload is created.
	TriggerCreate = "create"
	// TriggerImage is recorded when a rollout is caused by an image change.
	TriggerImage = "image"
	// TriggerConfig is recorded when a rollout is caused by any other pod-template
	// change (config, env, resources, ...).
	TriggerConfig = "config"
)

// commonLabels are the standard labels every HyperFleet metric must carry. They
// are constant for the lifetime of the process, so they are attached as const
// labels rather than passed at each observation.
func commonLabels() prometheus.Labels {
	return prometheus.Labels{
		"component": Component,
		"version":   version.Version(),
	}
}

// reconcileBuckets covers the expected reconcile latency spread: sub-millisecond
// server-side-apply no-ops up to multi-second reconciles that touch the API
// server repeatedly. Matches the standard's general-purpose bucket guidance.
var reconcileBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Collectors. Exported so tests can assert on them via prometheus/testutil and so
// call sites read explicitly; prefer the helper functions below for recording.
var (
	// ReconcileDuration measures wall-clock time of a full reconcile, regardless
	// of outcome.
	ReconcileDuration = promauto.With(ctrlmetrics.Registry).NewHistogram(prometheus.HistogramOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "reconcile_duration_seconds",
		Help:        "Duration of HyperFleetConfig reconciles in seconds.",
		Buckets:     reconcileBuckets,
		ConstLabels: commonLabels(),
	})

	// ReconcileErrors counts reconcile failures by the stage that failed
	// (get/render/apply/...), so error rate can be broken down by reason.
	ReconcileErrors = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "reconcile_errors_total",
		Help:        "Total number of reconcile errors, labeled by the failing stage.",
		ConstLabels: commonLabels(),
	}, []string{"reason"})

	// OperandReady reports, per operand component, whether its workload is
	// currently Available (1) or not (0).
	OperandReady = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "operand_ready",
		Help:        "Readiness of each operand workload (1 = Available, 0 = not).",
		ConstLabels: commonLabels(),
	}, []string{"operand"})

	// OperandRollouts counts operand workload rollouts by component and trigger.
	OperandRollouts = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "operand_rollouts_total",
		Help:        "Total operand workload rollouts, labeled by operand and trigger.",
		ConstLabels: commonLabels(),
	}, []string{"operand", "trigger"})

	// AppliedConfig is an info-style gauge whose "hash" label carries the digest
	// of the currently applied HyperFleetConfig spec. Only ever one series exists
	// at a time (see SetAppliedConfigHash).
	AppliedConfig = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "applied_config_info",
		Help:        "Info metric whose hash label is the digest of the applied HyperFleetConfig spec.",
		ConstLabels: commonLabels(),
	}, []string{"hash"})

	// buildInfo is set once at startup; its labels carry the build identity.
	buildInfo = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "build_info",
		Help:        "Build information; always 1, identity carried in labels.",
		ConstLabels: commonLabels(),
	}, []string{"commit", "go_version"})

	// up is 1 while the operator process is running.
	up = promauto.With(ctrlmetrics.Registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "up",
		Help:        "1 while the operator is running.",
		ConstLabels: commonLabels(),
	})
)

// Init records the process-lifetime metrics (build info and up). Call it once at
// startup, after flags are parsed.
func Init() {
	buildInfo.WithLabelValues(version.Commit(), version.GoVersion()).Set(1)
	up.Set(1)
}

// ObserveReconcile records the duration of one reconcile.
func ObserveReconcile(d time.Duration) {
	ReconcileDuration.Observe(d.Seconds())
}

// IncReconcileError increments the error counter for the given failing stage.
func IncReconcileError(reason string) {
	ReconcileErrors.WithLabelValues(reason).Inc()
}

// SetOperandReady sets the readiness gauge for an operand component.
func SetOperandReady(operand string, ready bool) {
	v := 0.0
	if ready {
		v = 1.0
	}
	OperandReady.WithLabelValues(operand).Set(v)
}

// IncOperandRollout records a rollout of an operand's workload.
func IncOperandRollout(operand, trigger string) {
	OperandRollouts.WithLabelValues(operand, trigger).Inc()
}

// SetAppliedConfigHash publishes the applied-config digest as the only series of
// the AppliedConfig gauge. It resets first so the previous hash's series does not
// linger and inflate cardinality over the operator's lifetime.
func SetAppliedConfigHash(hash string) {
	AppliedConfig.Reset()
	AppliedConfig.WithLabelValues(hash).Set(1)
}

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/metrics"
)

// depWithImages builds a Deployment whose pod template carries the given
// container images, in order, so the rollout-trigger classification can be
// exercised without an API server.
func depWithImages(images ...string) *appsv1.Deployment {
	containers := make([]corev1.Container, len(images))
	for i, img := range images {
		containers[i] = corev1.Container{Image: img}
	}
	dep := &appsv1.Deployment{}
	dep.Spec.Template.Spec.Containers = containers
	return dep
}

// TestRolloutTrigger verifies rolloutTrigger classifies an image change as
// TriggerImage and any other pod-template change as TriggerConfig.
func TestRolloutTrigger(t *testing.T) {
	g := NewWithT(t)

	g.Expect(rolloutTrigger(depWithImages("api:v1"), depWithImages("api:v2"))).
		To(Equal(metrics.TriggerImage), "an image change is an image-triggered rollout")

	g.Expect(rolloutTrigger(depWithImages("api:v1"), depWithImages("api:v1"))).
		To(Equal(metrics.TriggerConfig), "same images means a config-triggered rollout")

	// A change in the number of containers is an image-set change, not a config one.
	g.Expect(rolloutTrigger(depWithImages("api:v1"), depWithImages("api:v1", "sidecar:v1"))).
		To(Equal(metrics.TriggerImage), "a container-count change is treated as an image change")
}

// TestSameContainerImages verifies sameContainerImages compares images by value
// and order, and treats a differing container count as not-equal.
func TestSameContainerImages(t *testing.T) {
	g := NewWithT(t)

	g.Expect(sameContainerImages(depWithImages("a:1", "b:1"), depWithImages("a:1", "b:1"))).
		To(BeTrue(), "identical image lists are equal")

	g.Expect(sameContainerImages(depWithImages("a:1"), depWithImages("a:2"))).
		To(BeFalse(), "a differing image is not equal")

	g.Expect(sameContainerImages(depWithImages("a:1", "b:1"), depWithImages("b:1", "a:1"))).
		To(BeFalse(), "order matters")

	g.Expect(sameContainerImages(depWithImages("a:1"), depWithImages("a:1", "b:1"))).
		To(BeFalse(), "a differing container count is not equal")
}

// TestDetectRolloutsCatchesOutOfBandDrift verifies that a Deployment edited
// out-of-band (e.g. kubectl edit, HPA, a mutating webhook) is still detected as
// a rollout even when the stamped annotation claims the live object already
// matches the desired template. Comparing only the annotation to the desired
// hash would miss this: the annotation reflects what the operator itself last
// applied, not necessarily what is actually live. See the PR review this
// addresses.
func TestDetectRolloutsCatchesOutOfBandDrift(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())

	dep := depWithImages("app:v2")
	dep.Name, dep.Namespace = "drifted", "ns"
	desired := hashPodTemplate(dep)

	// The annotation says the live object already matches "desired", but its
	// actual containers were changed out-of-band afterward without going
	// through the operator.
	live := depWithImages("app:v1-drifted")
	live.Name, live.Namespace = "drifted", "ns"
	live.Annotations = map[string]string{templateHashAnnotation: desired}

	r := &HyperFleetConfigReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()}

	events := r.detectRollouts(context.Background(), "test-drift", []client.Object{dep})

	g.Expect(events).To(HaveLen(1),
		"drift the annotation doesn't reflect must still be detected as a rollout")
	g.Expect(events[0].trigger).To(Equal(metrics.TriggerImage))
}

// TestRecordReadinessMissingDeploymentSetsNotReady verifies that when a
// component's Deployment no longer exists (deleted externally, or the
// component was dropped from the bundle spec), recordReadiness sets the gauge
// to not-ready instead of leaving it at its last-reported value. See the PR
// review this addresses: a stale "ready" reading would otherwise persist
// forever once the workload is gone.
func TestRecordReadinessMissingDeploymentSetsNotReady(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: "ns"},
	}
	r := &HyperFleetConfigReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	const component = "test-missing-deployment"
	metrics.SetOperandReady(component, true)

	r.recordReadiness(context.Background(), component, []client.Object{dep})

	g.Expect(testutil.ToFloat64(metrics.OperandReady.WithLabelValues(component))).To(Equal(0.0),
		"a Deployment that can no longer be read as NotFound must report not-ready, not the stale previous value")
}

// TestHashConfigCoversComponentHashesNotJustSpec verifies the applied-config
// digest changes when a component's config-rollout hash changes (e.g. a
// referenced Secret rotation or resolved-value drift), even though the CR spec
// itself is unchanged. Spec-only hashing would miss exactly this case — see the
// PR review this addresses.
func TestHashConfigCoversComponentHashesNotJustSpec(t *testing.T) {
	g := NewWithT(t)

	spec := hyperfleetv1alpha1.HyperFleetConfigSpec{Bundle: hyperfleetv1alpha1.BundleCloudCAPI}

	same := hashConfig(spec, []string{"component-hash-v1"})
	g.Expect(hashConfig(spec, []string{"component-hash-v1"})).To(Equal(same),
		"identical spec and component hashes must hash equally")

	g.Expect(hashConfig(spec, []string{"component-hash-v2"})).NotTo(Equal(same),
		"a changed component config hash (e.g. from a Secret rotation) must change the digest even though the spec did not")

	g.Expect(hashConfig(spec, nil)).NotTo(Equal(same),
		"a missing component hash must not collide with a present one")
}

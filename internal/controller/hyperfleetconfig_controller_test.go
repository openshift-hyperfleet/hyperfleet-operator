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

package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/bundle"
	apicomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
	sentinelcomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/sentinel"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/metrics"
)

// sentinelResourceTypes is the set of entity plurals for the cloud-capi bundle,
// derived from bundle.CloudCAPIEntities so the specs track the same entity set
// the operator renders sentinel instances for. deleteOperands and the
// operand-set spec iterate over it.
var sentinelResourceTypes = func() []string {
	types := make([]string, 0,
		len(bundle.CloudCAPIEntities))
	for _, e := range bundle.CloudCAPIEntities {
		types = append(types, e.Plural)
	}
	return types
}()

// sentinelName is the status.components key / operand identity of the Sentinel
// instance watching the given resource type — the same format
// sentinel.Component.Name() builds from.
func sentinelName(rt string) string {
	return fmt.Sprintf("%s-%s", sentinelcomponent.ComponentName, rt)
}

// Reconciler behavior specs (HYPERFLEET-1407). These run against envtest, which
// is apiserver + etcd only: there is NO garbage-collection controller and NO
// running manager. Two consequences shape the assertions below and must not be
// "fixed":
//   - GC is verified structurally (owner references are set with controller:true
//     and a matching UID); real cascade deletion is proven in the kind e2e.
//   - Self-healing is verified by deleting an operand and calling Reconcile again
//     directly; true Owns()-driven wake-ups also belong to e2e.

// deleteOperands removes all operands (API + sentinel) so each spec starts
// clean. envtest has no GC, so deleting the CR would not cascade; specs delete
// operands explicitly. NotFound is a valid "already clean" outcome.
func deleteOperands(ctx context.Context, namespace string) {
	GinkgoHelper()

	operands := make([]client.Object, 0, 6+5*len(sentinelResourceTypes))
	operands = append(operands,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ConfigMapName, Namespace: namespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
	)
	for _, rt := range sentinelResourceTypes {
		name := sentinelcomponent.ResourceName(rt)
		operands = append(operands,
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: sentinelcomponent.ConfigMapName(rt), Namespace: namespace}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: sentinelcomponent.BrokerConfigMapName(rt), Namespace: namespace}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		)
	}
	for _, o := range operands {
		if err := k8sClient.Delete(ctx, o); err != nil && !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}
}

var _ = Describe("HyperFleetConfig Controller", func() {
	const (
		operatorNamespace = "hyperfleet-system"
		apiImage          = "example.com/hyperfleet-api:test"
		sentinelImage     = "example.com/hyperfleet-sentinel:test"
	)

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: hyperfleetv1alpha1.SingletonName}

	// operandKey builds the lookup key for an operand in the operator namespace.
	operandKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: operatorNamespace}
	}

	var reconciler *HyperFleetConfigReconciler

	BeforeEach(func() {
		By("ensuring the operator namespace exists (envtest does not create it)")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNamespace}}
		if err := k8sClient.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		By("creating the HyperFleetConfig singleton")
		existing := &hyperfleetv1alpha1.HyperFleetConfig{}
		if err := k8sClient.Get(ctx, typeNamespacedName, existing); errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, validHyperFleetConfig())).To(Succeed())
		} else {
			// Any error other than NotFound leaves fixture state unknown: fail
			// loudly rather than let a spec run without its resource.
			Expect(err).NotTo(HaveOccurred())
		}

		// Both the singleton and the operands force fixed names, so tear them down
		// after every spec. deleteSingletonAndWait lives in suite_test.go.
		DeferCleanup(deleteSingletonAndWait, ctx)
		DeferCleanup(deleteOperands, ctx, operatorNamespace)

		reconciler = &HyperFleetConfigReconciler{
			Client:            k8sClient,
			Scheme:            k8sClient.Scheme(),
			OperatorNamespace: operatorNamespace,
			APIImage:          apiImage,
			SentinelImage:     sentinelImage,
			BrokerInfo:        &sentinelcomponent.BrokerInfo{Type: sentinelcomponent.BrokerTypeRabbitMQ},
		}
	})

	// doReconcile runs one reconcile of the singleton and asserts it succeeds.
	doReconcile := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
	}

	// expectOwnedBy asserts the operand carries exactly one controller owner
	// reference pointing at the CR — the structural stand-in for GC in envtest.
	expectOwnedBy := func(obj client.Object, cr *hyperfleetv1alpha1.HyperFleetConfig) {
		GinkgoHelper()
		owners := obj.GetOwnerReferences()
		Expect(owners).To(HaveLen(1))
		Expect(owners[0].Kind).To(Equal("HyperFleetConfig"))
		Expect(owners[0].Name).To(Equal(cr.Name))
		Expect(owners[0].UID).To(Equal(cr.UID))
		Expect(owners[0].Controller).To(HaveValue(BeTrue()))
		Expect(owners[0].BlockOwnerDeletion).To(HaveValue(BeTrue()))
	}

	It("creates the full operand set, each owned by the CR", func() {
		cr := &hyperfleetv1alpha1.HyperFleetConfig{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, cr)).To(Succeed())

		By("reconciling the created resource")
		doReconcile()

		By("verifying each operand exists and is owned by the CR")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		expectOwnedBy(dep, cr)
		Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(apiImage))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), svc)).To(Succeed())
		expectOwnedBy(svc, cr)
		Expect(svc.Spec.Ports).To(HaveLen(3))

		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), sa)).To(Succeed())
		expectOwnedBy(sa, cr)

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ConfigMapName), cm)).To(Succeed())
		expectOwnedBy(cm, cr)
		Expect(cm.Data).To(HaveKey("config.yaml"))

		role := &rbacv1.Role{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), role)).To(Succeed())
		expectOwnedBy(role, cr)
		Expect(role.Rules).To(BeEmpty())

		rb := &rbacv1.RoleBinding{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), rb)).To(Succeed())
		expectOwnedBy(rb, cr)

		By("spot-checking sentinel operands for one resource type")
		sentinelName := sentinelcomponent.ResourceName("clusters")
		sDep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, operandKey(sentinelName), sDep)).To(Succeed())
		expectOwnedBy(sDep, cr)
		Expect(sDep.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(sDep.Spec.Template.Spec.Containers[0].Image).To(Equal(sentinelImage))

		sSA := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, operandKey(sentinelName), sSA)).To(Succeed())
		expectOwnedBy(sSA, cr)

		sCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, operandKey(sentinelcomponent.ConfigMapName("clusters")), sCM)).To(Succeed())
		expectOwnedBy(sCM, cr)
		Expect(sCM.Data).To(HaveKey(sentinelcomponent.ConfigFileKey))

		sBCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, operandKey(sentinelcomponent.BrokerConfigMapName("clusters")), sBCM)).To(Succeed())
		expectOwnedBy(sBCM, cr)
		Expect(sBCM.Data).To(HaveKey(sentinelcomponent.BrokerConfigFileKey))

		sNP := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, operandKey(sentinelName), sNP)).To(Succeed())
		expectOwnedBy(sNP, cr)

		By("verifying all sentinel resource types have Deployments")
		for _, rt := range sentinelResourceTypes {
			Expect(k8sClient.Get(ctx, operandKey(sentinelcomponent.ResourceName(rt)), &appsv1.Deployment{})).To(Succeed())
		}
	})

	It("recreates an operand after out-of-band deletion (drift self-heals)", func() {
		By("reconciling to create the operands")
		doReconcile()

		By("deleting the Deployment out of band")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		Eventually(func() bool {
			return errors.IsNotFound(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), &appsv1.Deployment{}))
		}).Should(BeTrue())

		By("reconciling again and asserting the Deployment reappears")
		doReconcile()
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), &appsv1.Deployment{})).To(Succeed())
	})

	It("is idempotent: a no-op reconcile does not rewrite operands", func() {
		By("reconciling to create the operands")
		doReconcile()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		depRV := dep.ResourceVersion
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ConfigMapName), cm)).To(Succeed())
		cmRV := cm.ResourceVersion

		By("reconciling again with no spec change")
		doReconcile()

		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		Expect(dep.ResourceVersion).To(Equal(depRV), "server-side apply of identical state must be a no-op")
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ConfigMapName), cm)).To(Succeed())
		Expect(cm.ResourceVersion).To(Equal(cmRV), "server-side apply of identical state must be a no-op")
	})

	It("does not record a spurious rollout on a no-op reconcile against a real API server", func() {
		// detectRollouts hashes the live Deployment's pod template directly (to catch
		// out-of-band drift a stamped annotation could miss — see its doc comment).
		// A fake client would never expose the bug this guards against: only a real
		// API server injects defaults (DNSPolicy, RestartPolicy, SchedulerName, the
		// container's TerminationMessagePath/Policy, etc.) into the pod template that
		// component.Render never sets. If hashPodTemplate is not robust to those
		// additions, every reconcile — forever — looks like a config-triggered
		// rollout, because live is never equal to desired again after the first apply.
		By("reconciling to create the operands")
		doReconcile()

		configBefore := testutil.ToFloat64(
			metrics.OperandRollouts.WithLabelValues(apicomponent.ComponentName, metrics.TriggerConfig))
		imageBefore := testutil.ToFloat64(
			metrics.OperandRollouts.WithLabelValues(apicomponent.ComponentName, metrics.TriggerImage))

		By("reconciling twice more with no spec change")
		doReconcile()
		doReconcile()

		Expect(testutil.ToFloat64(
			metrics.OperandRollouts.WithLabelValues(apicomponent.ComponentName, metrics.TriggerConfig))).
			To(Equal(configBefore), "a no-op reconcile must not count a config-triggered rollout")
		Expect(testutil.ToFloat64(
			metrics.OperandRollouts.WithLabelValues(apicomponent.ComponentName, metrics.TriggerImage))).
			To(Equal(imageBefore), "a no-op reconcile must not count an image-triggered rollout")
	})

	It("stamps a config-hash on the Deployment and rolls it when a referenced secret rotates", func() {
		By("creating the referenced database secret")
		dbSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testDBSecretName, Namespace: operatorNamespace},
			Data: map[string][]byte{
				apicomponent.SecretKeyDBHost:     []byte("db.example.com"),
				apicomponent.SecretKeyDBPort:     []byte("5432"),
				apicomponent.SecretKeyDBName:     []byte("hyperfleet"),
				apicomponent.SecretKeyDBUser:     []byte("hyperfleet"),
				apicomponent.SecretKeyDBPassword: []byte("original"),
			},
		}
		Expect(k8sClient.Create(ctx, dbSecret)).To(Succeed())
		DeferCleanup(func(ctx context.Context) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, dbSecret))).To(Succeed())
		}, ctx)

		By("reconciling and reading the stamped hash")
		doReconcile()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		firstHash := dep.Spec.Template.Annotations[configHashAnnotation]
		Expect(firstHash).NotTo(BeEmpty())

		By("reconciling again with no change: the hash (and pod template) is stable")
		doReconcile()
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations[configHashAnnotation]).To(Equal(firstHash))

		By("rotating the secret value and reconciling: the hash changes")
		Expect(k8sClient.Get(ctx, operandKey(testDBSecretName), dbSecret)).To(Succeed())
		dbSecret.Data[apicomponent.SecretKeyDBPassword] = []byte("rotated")
		Expect(k8sClient.Update(ctx, dbSecret)).To(Succeed())

		doReconcile()
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations[configHashAnnotation]).NotTo(Equal(firstHash))
	})

	It("records observability metrics for the reconcile and its operands", func() {
		By("reconciling to create the operands")
		doReconcile()

		By("publishing the applied-config digest as a single info series")
		// SetAppliedConfigHash resets before setting, so exactly one series exists
		// regardless of how many times the suite has reconciled.
		Expect(testutil.CollectAndCount(metrics.AppliedConfig)).To(Equal(1))

		By("publishing operand readiness for the api component")
		// envtest is apiserver + etcd only: no deployment controller runs, so the
		// operand never reports Available. The gauge must still be published, at 0.
		Expect(testutil.ToFloat64(
			metrics.OperandReady.WithLabelValues(apicomponent.ComponentName))).To(Equal(0.0))

		By("counting a create-triggered rollout for the api operand")
		// The Deployment did not exist before this reconcile (BeforeEach starts
		// clean), so the reconcile records at least one create rollout.
		Expect(testutil.ToFloat64(
			metrics.OperandRollouts.WithLabelValues(apicomponent.ComponentName, metrics.TriggerCreate))).
			To(BeNumerically(">=", 1))
	})

	It("returns without error when the CR is absent (deletion path)", func() {
		By("deleting the singleton before it is reconciled")
		deleteSingletonAndWait(ctx)

		By("reconciling a now-absent CR")
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
	})

	// Status-condition transition matrix (HYPERFLEET-1409). envtest is apiserver +
	// etcd only — there is no Deployment controller, so nothing ever auto-populates
	// a Deployment's .status. Each scenario below patches the Deployment's status
	// subresource directly to simulate what the real Deployment controller would
	// have written, then reconciles again and asserts the CR's rolled-up status.

	// conditionByType is defined package-level in hyperfleetconfig_status_test.go.

	// getCR re-fetches the singleton's current state.
	getCR := func() *hyperfleetv1alpha1.HyperFleetConfig {
		GinkgoHelper()
		cr := &hyperfleetv1alpha1.HyperFleetConfig{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, cr)).To(Succeed())
		return cr
	}

	// patchDeploymentStatus fetches a named Deployment and overwrites its status
	// subresource, simulating what the built-in Deployment controller would write.
	patchDeploymentStatus := func(name string, mutate func(*appsv1.Deployment)) {
		GinkgoHelper()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, operandKey(name), dep)).To(Succeed())
		mutate(dep)
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
	}

	// markAllDeploymentsReady patches every component Deployment (API + sentinels)
	// as fully ready, so the rolled-up Available condition becomes True.
	markAllDeploymentsReady := func() {
		GinkgoHelper()
		ready := func(dep *appsv1.Deployment) {
			dep.Status.Replicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.ObservedGeneration = dep.Generation
		}
		patchDeploymentStatus(apicomponent.ResourceName, ready)
		for _, rt := range sentinelResourceTypes {
			patchDeploymentStatus(sentinelcomponent.ResourceName(rt), ready)
		}
	}

	// createReferencedSecrets creates every Secret the fixture CR (validHyperFleetConfig)
	// references — the database Secret, the pinned JWKS Secret, and the broker
	// credential Secret — so scenarios about Deployment/rollout health are not
	// incidentally also Degraded on a missing Secret.
	createReferencedSecrets := func() {
		GinkgoHelper()
		dbSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testDBSecretName, Namespace: operatorNamespace},
			Data: map[string][]byte{
				apicomponent.SecretKeyDBHost:     []byte("db.example.com"),
				apicomponent.SecretKeyDBPort:     []byte("5432"),
				apicomponent.SecretKeyDBName:     []byte("hyperfleet"),
				apicomponent.SecretKeyDBUser:     []byte("hyperfleet"),
				apicomponent.SecretKeyDBPassword: []byte("password"),
			},
		}
		jwksSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testJWKSSecretName, Namespace: operatorNamespace},
			Data:       map[string][]byte{apicomponent.SecretKeyJWKS: []byte(`{"keys":[]}`)},
		}
		brokerSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sentinelcomponent.BrokerSecretName, Namespace: operatorNamespace},
			Data:       map[string][]byte{sentinelcomponent.BrokerSecretKey: []byte("amqp://guest:guest@rabbitmq:5672/")},
		}
		for _, s := range []*corev1.Secret{dbSecret, jwksSecret, brokerSecret} {
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
			DeferCleanup(func(ctx context.Context, obj client.Object) {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}, ctx, s)
		}
	}

	It("reports Available=False and Progressing=True on install, before the Deployment is ready", func() {
		By("creating the referenced secrets so Degraded is not also triggered")
		createReferencedSecrets()

		By("reconciling the freshly-created singleton")
		doReconcile()

		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionFalse))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonAsExpected))
		Expect(cr.Status.ObservedGeneration).To(Equal(cr.Generation))
	})

	It("reports Available=True and Progressing=False once all Deployments are fully ready", func() {
		By("reconciling to create the operands")
		doReconcile()

		By("marking all Deployments fully ready, as the real Deployment controller would")
		markAllDeploymentsReady()

		By("reconciling again and reading the rolled-up status")
		doReconcile()

		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutComplete))
	})

	It("populates status.components with one entry per component on a healthy reconcile", func() {
		By("reconciling to create the operands")
		doReconcile()

		By("marking all Deployments fully ready, as the real Deployment controller would")
		markAllDeploymentsReady()

		By("reconciling again and reading the per-component status")
		doReconcile()

		cr := getCR()
		expected := make([]string, 0, 1+len(sentinelResourceTypes))
		expected = append(expected, apicomponent.ComponentName)
		for _, rt := range sentinelResourceTypes {
			expected = append(expected, sentinelName(rt))
		}

		Expect(cr.Status.Components).To(HaveLen(len(expected)))
		for _, name := range expected {
			entry := componentEntry(cr, name)
			Expect(entry).NotTo(BeNil(), "expected a status.components entry for %s", name)
			available := meta.FindStatusCondition(entry.Conditions, hyperfleetv1alpha1.ConditionAvailable)
			Expect(available).NotTo(BeNil())
			Expect(available.Status).To(Equal(metav1.ConditionTrue), "component %s should be available", name)
			Expect(available.ObservedGeneration).To(Equal(cr.Generation),
				"component %s's conditions must be stamped with the evaluated generation", name)
		}
	})

	It("writes partial per-component status when a later component's apply fails mid-loop", func() {
		By("pre-creating a Deployment whose immutable selector blocks the first Sentinel's apply")
		// The rendered Sentinel Deployment's selector differs from this one, and
		// Deployment selectors are immutable, so the operator's server-side
		// apply of sentinel-clusters fails with a field-is-immutable error —
		// after the API (the first component) has already applied and reported.
		blockerLabels := map[string]string{"app": "immutable-selector-blocker"}
		blocker := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sentinelcomponent.ResourceName("clusters"),
				Namespace: operatorNamespace,
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: blockerLabels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: blockerLabels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "blocker", Image: "example.com/blocker:test"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

		By("reconciling: the API applies and reports in, then the Sentinel apply fails")
		createReferencedSecrets()
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("apply component"))

		cr := getCR()
		By("verifying only the already-observed API entry was written, freshly stamped")
		Expect(cr.Status.Components).To(HaveLen(1),
			"only the API, which reconciles before the blocked Sentinel, should have an entry")
		apiEntry := componentEntry(cr, apicomponent.ComponentName)
		Expect(apiEntry).NotTo(BeNil())
		for _, c := range apiEntry.Conditions {
			Expect(c.ObservedGeneration).To(Equal(cr.Generation),
				"the API was observed this reconcile and its entry must say so")
		}

		By("verifying the rollup was not fabricated and Degraded reports the error")
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable)).To(Equal(metav1.Condition{}),
			"Available must not be fabricated when the loop aborted mid-way")
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing)).To(Equal(metav1.Condition{}),
			"Progressing must not be fabricated when the loop aborted mid-way")
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonReconcileError))
	})

	It("rolls only the components whose rendered ConfigMap changed", func() {
		// api.auth.audience feeds only the API's config.yaml; sentinel config
		// renders from the resource type, API URL, broker topic and the auth
		// on/off switch, so changing the audience must leave every sentinel alone.
		podHashes := func() (apiHash string, sentinelHashes map[string]string) {
			GinkgoHelper()
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
			apiHash = dep.Spec.Template.Annotations[configHashAnnotation]
			Expect(apiHash).NotTo(BeEmpty())

			sentinelHashes = map[string]string{}
			for _, rt := range sentinelResourceTypes {
				sDep := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, operandKey(sentinelcomponent.ResourceName(rt)), sDep)).To(Succeed())
				sentinelHashes[rt] = sDep.Spec.Template.Annotations[configHashAnnotation]
				Expect(sentinelHashes[rt]).NotTo(BeEmpty())
			}
			return apiHash, sentinelHashes
		}
		configMapRVs := func() map[string]string {
			GinkgoHelper()
			rvs := map[string]string{}
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, operandKey(apicomponent.ConfigMapName), cm)).To(Succeed())
			rvs[apicomponent.ConfigMapName] = cm.ResourceVersion
			for _, rt := range sentinelResourceTypes {
				for _, name := range []string{sentinelcomponent.ConfigMapName(rt), sentinelcomponent.BrokerConfigMapName(rt)} {
					Expect(k8sClient.Get(ctx, operandKey(name), cm)).To(Succeed())
					rvs[name] = cm.ResourceVersion
				}
			}
			return rvs
		}

		By("reconciling to stamp the initial hashes")
		doReconcile()
		apiBefore, sentinelsBefore := podHashes()
		rvsBefore := configMapRVs()

		By("changing the API auth audience and reconciling")
		cr := getCR()
		cr.Spec.API.Auth.Audience = testAudience + "-changed"
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		doReconcile()

		By("verifying only the API ConfigMap was rewritten and only the API rolled")
		apiAfter, sentinelsAfter := podHashes()
		rvsAfter := configMapRVs()
		Expect(apiAfter).NotTo(Equal(apiBefore), "the API config changed and must roll")
		Expect(sentinelsAfter).To(Equal(sentinelsBefore), "no sentinel config changed, so none may roll")
		for name, rv := range rvsBefore {
			if name == apicomponent.ConfigMapName {
				Expect(rvsAfter[name]).NotTo(Equal(rv), "the API ConfigMap must be rewritten")
			} else {
				Expect(rvsAfter[name]).To(Equal(rv), "ConfigMap %q is unaffected and must not be rewritten", name)
			}
		}

		By("reconciling with nothing changed: nothing rolls")
		doReconcile()
		apiFinal, sentinelsFinal := podHashes()
		Expect(apiFinal).To(Equal(apiAfter))
		Expect(sentinelsFinal).To(Equal(sentinelsAfter))
	})

	It("rolls only the components that reference a rotated secret", func() {
		By("creating the API database secret and the sentinel broker secret")
		dbSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testDBSecretName, Namespace: operatorNamespace},
			Data: map[string][]byte{
				apicomponent.SecretKeyDBHost:     []byte("db.example.com"),
				apicomponent.SecretKeyDBPort:     []byte("5432"),
				apicomponent.SecretKeyDBName:     []byte("hyperfleet"),
				apicomponent.SecretKeyDBUser:     []byte("hyperfleet"),
				apicomponent.SecretKeyDBPassword: []byte("original"),
			},
		}
		brokerSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sentinelcomponent.BrokerSecretName, Namespace: operatorNamespace},
			Data:       map[string][]byte{"password": []byte("original")},
		}
		for _, s := range []*corev1.Secret{dbSecret, brokerSecret} {
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
			DeferCleanup(func(ctx context.Context) {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, s))).To(Succeed())
			}, ctx)
		}

		// podHashes returns the config-hash pod-template annotation of the API
		// Deployment and of every sentinel Deployment, keyed by component.
		podHashes := func() (apiHash string, sentinelHashes map[string]string) {
			GinkgoHelper()
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
			apiHash = dep.Spec.Template.Annotations[configHashAnnotation]
			Expect(apiHash).NotTo(BeEmpty())

			sentinelHashes = map[string]string{}
			for _, rt := range sentinelResourceTypes {
				sDep := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, operandKey(sentinelcomponent.ResourceName(rt)), sDep)).To(Succeed())
				sentinelHashes[rt] = sDep.Spec.Template.Annotations[configHashAnnotation]
				Expect(sentinelHashes[rt]).NotTo(BeEmpty())
			}
			return apiHash, sentinelHashes
		}

		By("reconciling to stamp the initial hashes")
		doReconcile()
		apiBefore, sentinelsBefore := podHashes()

		By("rotating only the broker secret: every sentinel rolls, the API does not")
		Expect(k8sClient.Get(ctx, operandKey(sentinelcomponent.BrokerSecretName), brokerSecret)).To(Succeed())
		brokerSecret.Data["password"] = []byte("rotated")
		Expect(k8sClient.Update(ctx, brokerSecret)).To(Succeed())

		doReconcile()
		apiAfterBroker, sentinelsAfterBroker := podHashes()
		Expect(apiAfterBroker).To(Equal(apiBefore), "the API does not consume the broker secret")
		for _, rt := range sentinelResourceTypes {
			Expect(sentinelsAfterBroker[rt]).NotTo(Equal(sentinelsBefore[rt]),
				"sentinel %q consumes the broker secret and must roll", rt)
		}

		By("rotating only the database secret: the API rolls, no sentinel does")
		Expect(k8sClient.Get(ctx, operandKey(testDBSecretName), dbSecret)).To(Succeed())
		dbSecret.Data[apicomponent.SecretKeyDBPassword] = []byte("rotated")
		Expect(k8sClient.Update(ctx, dbSecret)).To(Succeed())

		doReconcile()
		apiAfterDB, sentinelsAfterDB := podHashes()
		Expect(apiAfterDB).NotTo(Equal(apiAfterBroker), "the API consumes the database secret and must roll")
		Expect(sentinelsAfterDB).To(Equal(sentinelsAfterBroker), "sentinels do not consume the database secret")

		By("reconciling with nothing rotated: nothing rolls")
		doReconcile()
		apiFinal, sentinelsFinal := podHashes()
		Expect(apiFinal).To(Equal(apiAfterDB))
		Expect(sentinelsFinal).To(Equal(sentinelsAfterDB))
	})

	It("reports Progressing=True while a rollout to a new generation has not completed", func() {
		doReconcile()

		By("marking all Deployments healthy at their current generation")
		markAllDeploymentsReady()
		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))

		By("simulating a lagging rollout: the API Deployment has not caught up to its latest generation")
		patchDeploymentStatus(apicomponent.ResourceName, func(dep *appsv1.Deployment) {
			dep.Status.ObservedGeneration = dep.Generation - 1
		})

		doReconcile()
		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
	})

	It("reports Available=False when the operand Deployment goes down after having been healthy", func() {
		doReconcile()
		markAllDeploymentsReady()
		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))

		By("simulating the API operand going down")
		patchDeploymentStatus(apicomponent.ResourceName, func(dep *appsv1.Deployment) {
			dep.Status.AvailableReplicas = 0
			dep.Status.ReadyReplicas = 0
		})

		doReconcile()
		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
	})

	It("reports Available=False naming the Sentinel when only one Sentinel Deployment goes down, and recovers", func() {
		// nodepools is deliberately neither the first nor the API component, so the
		// surfaced message can only come from that Sentinel's own condition.
		const downType = "nodepools"

		doReconcile()
		markAllDeploymentsReady()
		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))

		By("taking down one Sentinel while the API and the other Sentinels stay healthy")
		patchDeploymentStatus(sentinelcomponent.ResourceName(downType), func(dep *appsv1.Deployment) {
			dep.Status.AvailableReplicas = 0
			dep.Status.ReadyReplicas = 0
		})

		doReconcile()
		available := conditionByType(getCR(), hyperfleetv1alpha1.ConditionAvailable)
		Expect(available.Status).To(Equal(metav1.ConditionFalse))
		Expect(available.Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
		// The rollup no longer names the component in its message; the failing
		// Sentinel is identified by its own status.components entry instead.
		down := componentEntry(getCR(), sentinelName(downType))
		Expect(down).NotTo(BeNil())
		downAvailable := meta.FindStatusCondition(down.Conditions, hyperfleetv1alpha1.ConditionAvailable)
		Expect(downAvailable).NotTo(BeNil())
		Expect(downAvailable.Status).To(Equal(metav1.ConditionFalse))

		By("restoring the Sentinel: Available recovers")
		patchDeploymentStatus(sentinelcomponent.ResourceName(downType), func(dep *appsv1.Deployment) {
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
		})

		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))
	})

	It("reports Progressing=True naming the Sentinel while only one Sentinel rollout has not completed", func() {
		const laggingType = "nodepools"

		doReconcile()
		markAllDeploymentsReady()
		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))

		By("simulating a lagging rollout on one Sentinel while the API and the other Sentinels are caught up")
		patchDeploymentStatus(sentinelcomponent.ResourceName(laggingType), func(dep *appsv1.Deployment) {
			dep.Status.ObservedGeneration = dep.Generation - 1
		})

		doReconcile()
		progressing := conditionByType(getCR(), hyperfleetv1alpha1.ConditionProgressing)
		Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
		Expect(progressing.Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
		lagging := componentEntry(getCR(), sentinelName(laggingType))
		Expect(lagging).NotTo(BeNil())
		laggingProgressing := meta.FindStatusCondition(lagging.Conditions, hyperfleetv1alpha1.ConditionProgressing)
		Expect(laggingProgressing).NotTo(BeNil())
		Expect(laggingProgressing.Status).To(Equal(metav1.ConditionTrue))

		By("catching the Sentinel up: Progressing clears")
		patchDeploymentStatus(sentinelcomponent.ResourceName(laggingType), func(dep *appsv1.Deployment) {
			dep.Status.ObservedGeneration = dep.Generation
		})

		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))
	})

	It("reports Degraded=True when a referenced Secret is missing", func() {
		By("reconciling without creating the fixture's referenced secrets")
		doReconcile()

		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonReferencedSecretMissing))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Message).To(ContainSubstring("database"))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Message).To(ContainSubstring("broker"))
	})

	It("lists missing secrets in sorted, deduplicated order in the Degraded message", func() {
		const prefix = "referenced secret(s) missing in the operator's namespace: "

		// Components resolve in order: API (database, jwks) first, then one sentinel
		// per entity, each referencing the same broker secret. Gathered in that order
		// the roles are "database, jwks, broker, broker, ...", so only sorting puts
		// broker first and only deduplication keeps it from repeating.
		By("reconciling with none of the referenced secrets present")
		doReconcile()
		degraded := conditionByType(getCR(), hyperfleetv1alpha1.ConditionDegraded)
		Expect(degraded.Reason).To(Equal(hyperfleetv1alpha1.ReasonReferencedSecretMissing))
		Expect(degraded.Message).To(Equal(prefix + "broker, database, jwks"))

		By("reconciling again: the message is identical, not reshuffled")
		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionDegraded).Message).To(Equal(degraded.Message))

		By("creating the broker secret: it drops out and the remaining names stay sorted")
		brokerSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sentinelcomponent.BrokerSecretName, Namespace: operatorNamespace},
			Data:       map[string][]byte{"password": []byte("x")},
		}
		Expect(k8sClient.Create(ctx, brokerSecret)).To(Succeed())
		DeferCleanup(func(ctx context.Context) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, brokerSecret))).To(Succeed())
		}, ctx)

		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionDegraded).Message).
			To(Equal(prefix + "database, jwks"))
	})

	It("reports Degraded=True with ReferencedSecretMissing, not ReconcileError, when OIDC discovery also fails with an empty cache", func() {
		By("pointing auth at an issuer whose discovery endpoint fails, on a fresh install with an empty discovery cache")
		badDiscovery := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		DeferCleanup(badDiscovery.Close)
		reconciler.HTTPClient = badDiscovery.Client()

		cr := getCR()
		cr.Spec.API.Auth.JWKCertSecretRef = nil
		cr.Spec.API.Auth.Issuer = badDiscovery.URL
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())

		By("reconciling without creating the fixture's referenced database Secret, so both discovery and the Secret read fail")
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("resolve JWKS URL"))

		By("verifying the missing Secret takes precedence over the discovery failure")
		cr = getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonReferencedSecretMissing))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Message).To(ContainSubstring("database"))
	})

	It("does not bump lastTransitionTime on a no-op reconcile", func() {
		By("reconciling once to establish a baseline status")
		doReconcile()
		firstTransition := conditionByType(getCR(), hyperfleetv1alpha1.ConditionAvailable).LastTransitionTime

		By("reconciling again with no state change")
		doReconcile()

		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionAvailable).LastTransitionTime).To(Equal(firstTransition))
	})

	It("reports Degraded=True but leaves Available/Progressing untouched when reconcile fails before any component reports", func() {
		By("reconciling once so a real Available/Progressing value is already recorded")
		createReferencedSecrets()
		doReconcile()
		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
		progressingBeforeFailure := conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing)
		observedGenerationBeforeFailure := cr.Status.ObservedGeneration

		By("pointing auth at an issuer whose discovery endpoint fails, so OIDC discovery errors before any component ever runs")
		badDiscovery := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		DeferCleanup(badDiscovery.Close)
		reconciler.HTTPClient = badDiscovery.Client()

		cr.Spec.API.Auth.JWKCertSecretRef = nil
		cr.Spec.API.Auth.Issuer = badDiscovery.URL
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("resolve JWKS URL"))

		By("verifying Degraded reflects the failure while Available is left at its last-recorded value, not fabricated healthy")
		cr = getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Reason).To(Equal(hyperfleetv1alpha1.ReasonReconcileError))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionDegraded).Message).NotTo(ContainSubstring("500"),
			"the Degraded message must not echo the raw wrapped error")
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse),
			"Available must remain at its last-recorded value, not be fabricated True")
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(progressingBeforeFailure.Status),
			"Progressing must remain at its last-recorded value when reconcile fails before any component reports")
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Reason).To(Equal(progressingBeforeFailure.Reason))
		Expect(cr.Status.ObservedGeneration).To(Equal(observedGenerationBeforeFailure),
			"top-level observedGeneration must stay pinned to the last fully processed generation")
	})

	It("returns a wrapped error when the operand namespace is absent", func() {
		// The reconciler wraps apply failures as "apply component %q: %w". Point it
		// at a namespace that does not exist so the first apply (the ServiceAccount)
		// is rejected by the NamespaceLifecycle admission plugin, and assert the
		// wrapping contract holds. The singleton from BeforeEach is required so
		// Reconcile reaches the apply step; no operands are created in the bogus
		// namespace, so no extra cleanup is needed.
		By("reconciling with an operator namespace that does not exist")
		badReconciler := &HyperFleetConfigReconciler{
			Client:            k8sClient,
			Scheme:            k8sClient.Scheme(),
			OperatorNamespace: "does-not-exist",
			APIImage:          apiImage,
			SentinelImage:     sentinelImage,
			BrokerInfo:        &sentinelcomponent.BrokerInfo{Type: sentinelcomponent.BrokerTypeRabbitMQ},
		}

		// The Deployment among the rendered objects would detect as a create-triggered
		// rollout, but the apply above fails before anything is persisted. The rollout
		// counter must not advance for a rollout that never happened — otherwise a
		// reconcile retried on every failed apply would keep double-counting it.
		before := testutil.ToFloat64(
			metrics.OperandRollouts.WithLabelValues(apicomponent.ComponentName, metrics.TriggerCreate))

		_, err := badReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("apply component"))

		By("not counting a rollout for the failed apply")
		Expect(testutil.ToFloat64(
			metrics.OperandRollouts.WithLabelValues(apicomponent.ComponentName, metrics.TriggerCreate))).
			To(Equal(before))
	})
})

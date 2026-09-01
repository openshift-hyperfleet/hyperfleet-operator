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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	apicomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/metrics"
)

// Reconciler behavior specs (HYPERFLEET-1407). These run against envtest, which
// is apiserver + etcd only: there is NO garbage-collection controller and NO
// running manager. Two consequences shape the assertions below and must not be
// "fixed":
//   - GC is verified structurally (owner references are set with controller:true
//     and a matching UID); real cascade deletion is proven in the kind e2e.
//   - Self-healing is verified by deleting an operand and calling Reconcile again
//     directly; true Owns()-driven wake-ups also belong to e2e.

// deleteOperands removes the API operands so each spec starts clean. envtest has
// no GC, so deleting the CR would not cascade; specs delete operands explicitly.
// NotFound is a valid "already clean" outcome; any other error fails the spec.
func deleteOperands(ctx context.Context, namespace string) {
	GinkgoHelper()
	operands := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ConfigMapName, Namespace: namespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: apicomponent.ResourceName, Namespace: namespace}},
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
		}
		_, err := badReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("apply component"))
	})
})

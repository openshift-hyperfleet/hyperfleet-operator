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
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

	// patchDeploymentStatus fetches the API Deployment and overwrites its status
	// subresource, simulating what the built-in Deployment controller would write.
	patchDeploymentStatus := func(mutate func(*appsv1.Deployment)) {
		GinkgoHelper()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, operandKey(apicomponent.ResourceName), dep)).To(Succeed())
		mutate(dep)
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
	}

	// createReferencedSecrets creates every Secret the fixture CR (validHyperFleetConfig)
	// references — the database Secret and the pinned JWKS Secret — so scenarios
	// about Deployment/rollout health are not incidentally also Degraded on a
	// missing Secret (that is its own dedicated scenario below).
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
		for _, s := range []*corev1.Secret{dbSecret, jwksSecret} {
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

	It("reports Available=True and Progressing=False once the Deployment is fully ready", func() {
		By("reconciling to create the operands")
		doReconcile()

		By("marking the Deployment fully ready, as the real Deployment controller would")
		patchDeploymentStatus(func(dep *appsv1.Deployment) {
			dep.Status.Replicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.ObservedGeneration = dep.Generation
		})

		By("reconciling again and reading the rolled-up status")
		doReconcile()

		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutComplete))
	})

	It("reports Progressing=True while a rollout to a new generation has not completed", func() {
		doReconcile()

		By("marking the Deployment healthy at its current generation")
		patchDeploymentStatus(func(dep *appsv1.Deployment) {
			dep.Status.Replicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.ObservedGeneration = dep.Generation
		})
		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))

		By("simulating a lagging rollout: the Deployment has not caught up to its latest generation")
		patchDeploymentStatus(func(dep *appsv1.Deployment) {
			dep.Status.ObservedGeneration = dep.Generation - 1
		})

		doReconcile()
		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Status).To(Equal(metav1.ConditionTrue))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionProgressing).Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
	})

	It("reports Available=False when the operand Deployment goes down after having been healthy", func() {
		doReconcile()
		patchDeploymentStatus(func(dep *appsv1.Deployment) {
			dep.Status.Replicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.ObservedGeneration = dep.Generation
		})
		doReconcile()
		Expect(conditionByType(getCR(), hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionTrue))

		By("simulating the operand going down")
		patchDeploymentStatus(func(dep *appsv1.Deployment) {
			dep.Status.AvailableReplicas = 0
			dep.Status.ReadyReplicas = 0
		})

		doReconcile()
		cr := getCR()
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
		Expect(conditionByType(cr, hyperfleetv1alpha1.ConditionAvailable).Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
	})

	It("reports Degraded=True when a referenced Secret is missing", func() {
		By("reconciling without creating the fixture's referenced database Secret")
		// The fixture's HyperFleetConfig references a database Secret that is never
		// created in this spec, so referencedSecretData reports it absent.
		doReconcile()

		cr := getCR()
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

		By("pointing auth at an issuer whose discovery endpoint fails, so OIDC discovery errors before referencedSecretData or any component ever runs")
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

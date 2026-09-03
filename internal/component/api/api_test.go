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

package api

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

const (
	testNamespace  = "hyperfleet-system"
	testDBSecret   = "hyperfleet-db"
	testTLSSecret  = "hyperfleet-tls"
	testJWKSSecret = "hyperfleet-jwks"
	testIssuer     = "https://issuer.example.com"
	testAudience   = "hyperfleet-api"
	testJWKCertURL = "https://issuer.example.com/certs"
)

// testCR returns a well-formed singleton in the shape Render consumes. Since
// HYPERFLEET-1408, Render derives config.yaml, database env and JWKS wiring from
// the spec, so the fixture carries a complete api spec (auth is enabled with
// jwkCertSecretRef pinned so Render needs no controller-supplied discovery
// result).
func testCR() *hyperfleetv1alpha1.HyperFleetConfig {
	return &hyperfleetv1alpha1.HyperFleetConfig{
		ObjectMeta: metav1.ObjectMeta{Name: hyperfleetv1alpha1.SingletonName},
		Spec: hyperfleetv1alpha1.HyperFleetConfigSpec{
			Bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			API: hyperfleetv1alpha1.APISpec{
				Database: hyperfleetv1alpha1.DatabaseSpec{
					SecretRef: hyperfleetv1alpha1.SecretReference{Name: testDBSecret},
				},
				Auth: hyperfleetv1alpha1.AuthSpec{
					Enabled:          ptr.To(true),
					Issuer:           testIssuer,
					Audience:         testAudience,
					JWKCertSecretRef: &hyperfleetv1alpha1.SecretReference{Name: testJWKSSecret},
				},
			},
		},
	}
}

// byKind indexes rendered objects by their GVK Kind for assertion. Render sets
// TypeMeta explicitly (required for server-side apply), so Kind is populated.
func byKind(objs []client.Object) map[string]client.Object {
	out := make(map[string]client.Object, len(objs))
	for _, o := range objs {
		out[o.GetObjectKind().GroupVersionKind().Kind] = o
	}
	return out
}

func TestRenderProducesTheOperandSet(t *testing.T) {
	g := NewWithT(t)
	const image = "example.com/hyperfleet-api:test"

	objs, err := New(image, testNamespace, Options{}).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(6))

	kinds := byKind(objs)
	g.Expect(kinds).To(HaveKey("ServiceAccount"))
	g.Expect(kinds).To(HaveKey("Role"))
	g.Expect(kinds).To(HaveKey("RoleBinding"))
	g.Expect(kinds).To(HaveKey("ConfigMap"))
	g.Expect(kinds).To(HaveKey("Service"))
	g.Expect(kinds).To(HaveKey("Deployment"))

	// Every operand lives in the operator namespace, carries the common labels
	// (including the component marker) and a non-empty GVK for SSA.
	for _, o := range objs {
		g.Expect(o.GetNamespace()).To(Equal(testNamespace))
		g.Expect(o.GetLabels()).To(HaveKeyWithValue(labelComponent, ComponentName))
		g.Expect(o.GetLabels()).To(HaveKeyWithValue(labelManagedBy, managedByOperator))
		g.Expect(o.GetObjectKind().GroupVersionKind().Kind).NotTo(BeEmpty())
		g.Expect(o.GetObjectKind().GroupVersionKind().Version).NotTo(BeEmpty())
	}
}

func TestRenderDeployment(t *testing.T) {
	g := NewWithT(t)
	const image = "example.com/hyperfleet-api:test"

	objs, err := New(image, testNamespace, Options{}).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	dep, ok := byKind(objs)["Deployment"].(*appsv1.Deployment)
	g.Expect(ok).To(BeTrue())
	g.Expect(dep.Name).To(Equal(ResourceName))

	spec := dep.Spec
	g.Expect(spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
	// The selector is the immutable subset; the pod template carries the full
	// label set, so the selector must be a subset of the template labels (a
	// selector that is not a subset would make the Deployment adopt no pods).
	for k, v := range spec.Selector.MatchLabels {
		g.Expect(spec.Template.Labels).To(HaveKeyWithValue(k, v))
	}
	g.Expect(spec.Template.Labels).To(HaveKeyWithValue(labelComponent, ComponentName))

	g.Expect(spec.Template.Spec.ServiceAccountName).To(Equal(ResourceName))
	// The API does not use the Kubernetes API, so the SA token is not mounted.
	g.Expect(spec.Template.Spec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
	g.Expect(spec.Template.Spec.Containers).To(HaveLen(1))

	c := spec.Template.Spec.Containers[0]
	g.Expect(c.Image).To(Equal(image))
	g.Expect(c.Args).To(Equal([]string{"serve"}))

	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	g.Expect(ports).To(Equal(map[string]int32{
		portNameHTTP:    portHTTP,
		portNameHealth:  portHealth,
		portNameMetrics: portMetrics,
	}))

	g.Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/healthz"))
	g.Expect(c.LivenessProbe.HTTPGet.Port.StrVal).To(Equal(portNameHealth))
	g.Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
	g.Expect(c.ReadinessProbe.HTTPGet.Port.StrVal).To(Equal(portNameHealth))

	// Hardened container per the chart's securityContext.
	g.Expect(c.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
	g.Expect(c.SecurityContext.AllowPrivilegeEscalation).To(HaveValue(BeFalse()))
	g.Expect(dep.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))

	// The config ConfigMap is mounted read-only at the expected path.
	var mountedConfig bool
	for _, v := range spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == ConfigMapName {
			mountedConfig = true
		}
	}
	g.Expect(mountedConfig).To(BeTrue(), "expected a volume backed by the API ConfigMap")
}

// deploymentFrom renders the CR and returns the Deployment operand.
func deploymentFrom(t *testing.T, cr *hyperfleetv1alpha1.HyperFleetConfig) *appsv1.Deployment {
	t.Helper()
	g := NewWithT(t)
	objs, err := New("img", testNamespace, Options{}).Render(context.Background(), cr)
	g.Expect(err).NotTo(HaveOccurred())
	dep, ok := byKind(objs)["Deployment"].(*appsv1.Deployment)
	g.Expect(ok).To(BeTrue())
	return dep
}

func TestRenderDeploymentDatabaseEnv(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentFrom(t, testCR())

	// The database Secret is mounted read-only, unconditionally (unlike the
	// optional TLS/JWKS mounts), so its keys surface as files.
	g.Expect(volumeSecretName(dep, dbVolume)).To(Equal(testDBSecret))
	g.Expect(mountReadOnlyAt(dep, dbVolume)).To(Equal(dbMountPath))

	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		g.Expect(e.ValueFrom).To(BeNil(), "expected %s as a literal path, not a secretKeyRef", e.Name)
		env[e.Name] = e.Value
	}

	// HYPERFLEET_CONFIG and the five database *_FILE vars are all literal paths
	// into read-only mounts — never a credential value, and never a secretKeyRef
	// (HYPERFLEET-1603: the API reads these files itself at startup).
	g.Expect(env).To(HaveKeyWithValue("HYPERFLEET_CONFIG", configFilePath))
	cases := map[string]string{
		envDBHostFile:     dbHostFilePath,
		envDBPortFile:     dbPortFilePath,
		envDBNameFile:     dbNameFilePath,
		envDBUsernameFile: dbUsernameFilePath,
		envDBPasswordFile: dbPasswordFilePath,
	}
	for envName, path := range cases {
		g.Expect(env).To(HaveKeyWithValue(envName, path))
	}
}

func TestRenderTLSMountOnlyWhenConfigured(t *testing.T) {
	g := NewWithT(t)

	// Absent by default.
	dep := deploymentFrom(t, testCR())
	g.Expect(volumeNames(dep)).NotTo(ContainElement(tlsVolume))

	// Present when spec.api.tls is set, mounted read-only at the TLS path.
	cr := testCR()
	cr.Spec.API.TLS = &hyperfleetv1alpha1.TLSSpec{
		SecretRef: hyperfleetv1alpha1.SecretReference{Name: testTLSSecret},
	}
	dep = deploymentFrom(t, cr)
	g.Expect(volumeSecretName(dep, tlsVolume)).To(Equal(testTLSSecret))
	g.Expect(mountReadOnlyAt(dep, tlsVolume)).To(Equal(tlsMountPath))
}

func TestRenderJWKSMountOnlyWhenSecretRef(t *testing.T) {
	g := NewWithT(t)

	// Secret-based auth (the fixture default): JWKS Secret mounted read-only.
	dep := deploymentFrom(t, testCR())
	g.Expect(volumeSecretName(dep, jwksVolume)).To(Equal(testJWKSSecret))
	g.Expect(mountReadOnlyAt(dep, jwksVolume)).To(Equal(jwksMountPath))

	// No Secret pinned: falls through to the controller-supplied discovered URL,
	// so no JWKS mount (config.yaml assertions are in
	// TestRenderDiscoveredJWKSURLLandsInConfig).
	cr := testCR()
	cr.Spec.API.Auth.JWKCertSecretRef = nil
	objs, err := New("img", testNamespace, Options{ResolvedJWKSURL: "https://issuer.example.com/discovered"}).Render(context.Background(), cr)
	g.Expect(err).NotTo(HaveOccurred())
	dep2, ok := byKind(objs)["Deployment"].(*appsv1.Deployment)
	g.Expect(ok).To(BeTrue())
	g.Expect(volumeNames(dep2)).NotTo(ContainElement(jwksVolume))
}

func TestRenderJWKSMountRequiresAuthEnabled(t *testing.T) {
	g := NewWithT(t)

	// Auth disabled but a JWKS Secret is pinned (the fixture default). The API
	// never reads the JWKS file when auth is off, so the Secret must NOT be
	// mounted — otherwise an unused (and possibly absent, hence
	// pod-start-blocking) Secret is injected. The mount gate must match
	// resolveJWKSource/referencedSecretData, which both require auth to be
	// enabled.
	cr := testCR()
	cr.Spec.API.Auth.Enabled = ptr.To(false)

	dep := deploymentFrom(t, cr)
	g.Expect(volumeNames(dep)).NotTo(ContainElement(jwksVolume))
}

func TestRenderDiscoveredJWKSURLLandsInConfig(t *testing.T) {
	g := NewWithT(t)

	// Auth on with no pinned Secret: resolveJWKSource falls through to the
	// controller-supplied OIDC-discovered URL (Options.ResolvedJWKSURL), which
	// must be written into config.yaml as jwk_cert_url with no JWKS mount.
	cr := testCR()
	cr.Spec.API.Auth.JWKCertSecretRef = nil
	const discovered = "https://issuer.example.com/discovered/keys"

	objs, err := New("img", testNamespace, Options{ResolvedJWKSURL: discovered}).Render(context.Background(), cr)
	g.Expect(err).NotTo(HaveOccurred())

	dep, ok := byKind(objs)["Deployment"].(*appsv1.Deployment)
	g.Expect(ok).To(BeTrue())
	g.Expect(volumeNames(dep)).NotTo(ContainElement(jwksVolume))

	cm, ok := byKind(objs)["ConfigMap"].(*corev1.ConfigMap)
	g.Expect(ok).To(BeTrue())
	c0 := parseConfig(t, cm.Data[ConfigFileKey])["server"].(map[string]any)["jwt"].(map[string]any)["configs"].([]any)[0].(map[string]any)
	g.Expect(c0["jwk_cert_url"]).To(Equal(discovered))
	g.Expect(c0).NotTo(HaveKey("jwk_cert_file"))
}

// volumeNames returns the pod's volume names.
func volumeNames(dep *appsv1.Deployment) []string {
	names := make([]string, 0, len(dep.Spec.Template.Spec.Volumes))
	for _, v := range dep.Spec.Template.Spec.Volumes {
		names = append(names, v.Name)
	}
	return names
}

// volumeSecretName returns the SecretName of the named volume (empty if the
// volume is missing or not Secret-backed).
func volumeSecretName(dep *appsv1.Deployment, name string) string {
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == name && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}

// mountReadOnlyAt returns the mount path of the named volume mount, asserting it
// is read-only (empty string if not found).
func mountReadOnlyAt(dep *appsv1.Deployment, name string) string {
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == name && m.ReadOnly {
			return m.MountPath
		}
	}
	return ""
}

func TestRenderEmptyImageFallsBackToDefault(t *testing.T) {
	g := NewWithT(t)

	objs, err := New("", testNamespace, Options{}).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	dep, ok := byKind(objs)["Deployment"].(*appsv1.Deployment)
	g.Expect(ok).To(BeTrue())
	g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(DefaultImage))
}

func TestRenderRoleHasNoRules(t *testing.T) {
	g := NewWithT(t)

	objs, err := New("img", testNamespace, Options{}).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	role, ok := byKind(objs)["Role"].(*rbacv1.Role)
	g.Expect(ok).To(BeTrue())
	// The API needs no in-cluster permissions today; the Role exists only to
	// satisfy the RBAC operand and pre-wire the pattern (see render.go).
	g.Expect(role.Rules).To(BeEmpty())

	rb, ok := byKind(objs)["RoleBinding"].(*rbacv1.RoleBinding)
	g.Expect(ok).To(BeTrue())
	g.Expect(rb.RoleRef.Name).To(Equal(ResourceName))
	g.Expect(rb.RoleRef.Kind).To(Equal("Role"))
	g.Expect(rb.Subjects).To(HaveLen(1))
	g.Expect(rb.Subjects[0].Name).To(Equal(ResourceName))
	g.Expect(rb.Subjects[0].Namespace).To(Equal(testNamespace))
}

// deploymentWithStatus returns a Deployment named ResourceName with the given
// spec.replicas and status fields set, for feeding directly into Conditions
// without any cluster/client dependency. ObservedGeneration matches generation
// (the Deployment controller has caught up); the mid-rollout test overrides it
// after construction to simulate a lag.
func deploymentWithStatus(replicas, availableReplicas, updatedReplicas int32, generation int64) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: ResourceName, Generation: generation},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas:  availableReplicas,
			UpdatedReplicas:    updatedReplicas,
			ObservedGeneration: generation,
		},
	}
}

func condMapByType(conds []metav1.Condition) map[string]metav1.Condition {
	out := make(map[string]metav1.Condition, len(conds))
	for _, c := range conds {
		out[c.Type] = c
	}
	return out
}

func TestConditionsNoDeployment(t *testing.T) {
	g := NewWithT(t)

	conds, err := New("img", testNamespace, Options{}).Conditions(context.Background(), testCR(), nil)
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
}

func TestConditionsZeroAvailableReplicas(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 0, 0, 1)
	conds, err := New("img", testNamespace, Options{}).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
}

func TestConditionsPartiallyReady(t *testing.T) {
	g := NewWithT(t)

	// 2 desired, only 1 available: not fully Available, and still Progressing
	// since not all replicas are updated.
	dep := deploymentWithStatus(2, 1, 1, 1)
	conds, err := New("img", testNamespace, Options{}).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentNotReady))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
}

func TestConditionsFullyReadyAndStable(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 1, 1, 1)
	conds, err := New("img", testNamespace, Options{}).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutComplete))
}

func TestConditionsMidRollout(t *testing.T) {
	g := NewWithT(t)

	// Fully available at the old generation, but a new generation has not yet
	// been observed by the Deployment controller: still Progressing.
	dep := deploymentWithStatus(1, 1, 1, 2)
	dep.Status.ObservedGeneration = 1
	conds, err := New("img", testNamespace, Options{}).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
}

func TestConditionsFindsDeploymentInMixedKindSlice(t *testing.T) {
	g := NewWithT(t)

	// applied carries every rendered operand kind, in Render's actual order —
	// proves findDeployment picks the Deployment out by kind+name rather than
	// assuming it is the only or first element.
	dep := deploymentWithStatus(1, 1, 1, 1)
	applied := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: ResourceName}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: ResourceName}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: ResourceName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ResourceName}},
		dep,
	}

	conds, err := New("img", testNamespace, Options{}).Conditions(context.Background(), testCR(), applied)
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
}

func TestConditionsNilSpecReplicasDefaultsToOne(t *testing.T) {
	g := NewWithT(t)

	// Spec.Replicas == nil (never set) must fall back to the same default of 1
	// that render.go's deployment() currently hardcodes, not a zero-value 0
	// (which would make every replica count vacuously "desired").
	dep := deploymentWithStatus(1, 1, 1, 1)
	dep.Spec.Replicas = nil

	conds, err := New("img", testNamespace, Options{}).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionFalse))
}

func TestRenderService(t *testing.T) {
	g := NewWithT(t)

	objs, err := New("img", testNamespace, Options{}).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	svc, ok := byKind(objs)["Service"].(*corev1.Service)
	g.Expect(ok).To(BeTrue())
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
	g.Expect(svc.Spec.Ports).To(HaveLen(3))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(labelComponent, ComponentName))
}

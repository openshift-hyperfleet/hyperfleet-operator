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

package sentinel

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

const (
	testNamespace       = "hyperfleet-system"
	testImage           = "example.com/hyperfleet-sentinel:test"
	testClustersRt      = "clusters"
	testNodepoolsRt     = "nodepools"
	testVersionsRt      = "versions"
	testWifconfigsRt    = "wifconfigs"
	testChannelsRt      = "channels"
	testHttpAPIBaseURL  = "http://hyperfleet-api:8000"
	testHttpsAPIBaseURL = "https://hyperfleet-api:8000"
	testBrokerTopic     = "hyperfleet-system-clusters"
)

func testOptions(brokerType BrokerType) Options {
	info := &BrokerInfo{Type: brokerType}
	if brokerType == BrokerTypeGooglePubSub {
		info.ProjectID = "test-project"
	}
	return Options{
		Image:        testImage,
		Namespace:    testNamespace,
		ResourceType: testClustersRt,
		APIBaseURL:   testHttpAPIBaseURL,
		BrokerInfo:   info,
	}
}

func testCR() *hyperfleetv1alpha1.HyperFleetConfig {
	return &hyperfleetv1alpha1.HyperFleetConfig{
		ObjectMeta: metav1.ObjectMeta{Name: hyperfleetv1alpha1.SingletonName},
		Spec: hyperfleetv1alpha1.HyperFleetConfigSpec{
			Bundle: hyperfleetv1alpha1.BundleCloudCAPI,
		},
	}
}

// byKind indexes rendered objects by their GVK Kind for assertion.
func byKind(objs []client.Object) map[string]client.Object {
	out := make(map[string]client.Object, len(objs))
	for _, o := range objs {
		out[o.GetObjectKind().GroupVersionKind().Kind] = o
	}
	return out
}

func findConfigMap(objs []client.Object, name string) *corev1.ConfigMap {
	for _, o := range objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == name {
			return cm
		}
	}
	return nil
}

func condMapByType(conds []metav1.Condition) map[string]metav1.Condition {
	out := make(map[string]metav1.Condition, len(conds))
	for _, c := range conds {
		out[c.Type] = c
	}
	return out
}

// --- Name ---

func TestName(t *testing.T) {
	g := NewWithT(t)
	c := New(testOptions(BrokerTypeRabbitMQ))
	g.Expect(c.Name()).To(Equal("sentinel-clusters"))
}

func TestNameVariesByResourceType(t *testing.T) {
	g := NewWithT(t)
	opts := testOptions(BrokerTypeRabbitMQ)
	opts.ResourceType = "nodepools"
	c := New(opts)
	g.Expect(c.Name()).To(Equal("sentinel-nodepools"))
}

// --- Rollout ---

func TestRollout(t *testing.T) {
	g := NewWithT(t)
	c := New(testOptions(BrokerTypeRabbitMQ))
	rt := c.Rollout()

	g.Expect(rt.DeploymentName).To(Equal(ResourceName(testClustersRt)))
	g.Expect(rt.ConfigEntries).To(HaveLen(2))
	g.Expect(rt.ConfigEntries[0].ConfigMapName).To(Equal(ConfigMapName(testClustersRt)))
	g.Expect(rt.ConfigEntries[0].ConfigFileKey).To(Equal(ConfigFileKey))
	g.Expect(rt.ConfigEntries[1].ConfigMapName).To(Equal(BrokerConfigMapName(testClustersRt)))
	g.Expect(rt.ConfigEntries[1].ConfigFileKey).To(Equal(BrokerConfigFileKey))
}

// --- ReferencedSecrets ---

func TestReferencedSecretsRabbitMQ(t *testing.T) {
	g := NewWithT(t)
	c := New(testOptions(BrokerTypeRabbitMQ))
	refs := c.ReferencedSecrets(testCR())
	g.Expect(refs).To(HaveLen(1))
	g.Expect(refs[0].Role).To(Equal("broker"))
	g.Expect(refs[0].SecretName).To(Equal(BrokerSecretName))
}

func TestReferencedSecretsGooglePubSubReturnsNone(t *testing.T) {
	g := NewWithT(t)
	c := New(testOptions(BrokerTypeGooglePubSub))
	refs := c.ReferencedSecrets(testCR())
	g.Expect(refs).To(BeEmpty())
}

// --- Render ---

func TestRenderProducesTheOperandSet(t *testing.T) {
	g := NewWithT(t)

	objs, err := New(testOptions(BrokerTypeRabbitMQ)).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(5))

	kinds := byKind(objs)
	g.Expect(kinds).To(HaveKey("ServiceAccount"))
	g.Expect(kinds).To(HaveKey("ConfigMap"))
	g.Expect(kinds).To(HaveKey("NetworkPolicy"))
	g.Expect(kinds).To(HaveKey("Deployment"))

	// Two ConfigMaps (config + broker-config) share the Kind key; count them
	// explicitly.
	var cmCount int
	for _, o := range objs {
		if _, ok := o.(*corev1.ConfigMap); ok {
			cmCount++
		}
	}
	g.Expect(cmCount).To(Equal(2))

	for _, o := range objs {
		g.Expect(o.GetNamespace()).To(Equal(testNamespace))
		g.Expect(o.GetLabels()).To(HaveKeyWithValue(labelComponent, componentName(testClustersRt)))
		g.Expect(o.GetLabels()).To(HaveKeyWithValue(labelManagedBy, managedByOperator))
		g.Expect(o.GetObjectKind().GroupVersionKind().Kind).NotTo(BeEmpty())
		g.Expect(o.GetObjectKind().GroupVersionKind().Version).NotTo(BeEmpty())
	}
}

func TestRenderErrorsOnEmptyImage(t *testing.T) {
	g := NewWithT(t)
	opts := testOptions(BrokerTypeRabbitMQ)
	opts.Image = ""
	_, err := New(opts).Render(context.Background(), testCR())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("sentinel image is required"))
}

// assertDeploymentCommon verifies the deployment properties shared across all
// broker types: replicas, selector, service account, image, args, ports,
// probes, security context, and config volume mounts.
func assertDeploymentCommon(g Gomega, dep *appsv1.Deployment) {
	g.Expect(dep).NotTo(BeNil())
	g.Expect(dep.Name).To(Equal(ResourceName(testClustersRt)))

	spec := dep.Spec
	g.Expect(spec.Replicas).To(HaveValue(BeEquivalentTo(1)))

	for k, v := range spec.Selector.MatchLabels {
		g.Expect(spec.Template.Labels).To(HaveKeyWithValue(k, v))
	}

	g.Expect(spec.Template.Spec.ServiceAccountName).To(Equal(ResourceName(testClustersRt)))
	g.Expect(spec.Template.Spec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
	g.Expect(spec.Template.Spec.Containers).To(HaveLen(1))

	c := spec.Template.Spec.Containers[0]
	g.Expect(c.Image).To(Equal(testImage))
	g.Expect(c.Args).To(Equal([]string{"serve", "--config=" + configMountPath}))

	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	g.Expect(ports).To(Equal(map[string]int32{
		portNameHTTP:    portHTTP,
		portNameMetrics: portMetrics,
	}))

	g.Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/healthz"))
	g.Expect(c.LivenessProbe.HTTPGet.Port.StrVal).To(Equal(portNameHTTP))
	g.Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
	g.Expect(c.ReadinessProbe.HTTPGet.Port.StrVal).To(Equal(portNameHTTP))

	g.Expect(c.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
	g.Expect(c.SecurityContext.AllowPrivilegeEscalation).To(HaveValue(BeFalse()))
	g.Expect(dep.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))

	var mountedConfig, mountedBroker bool
	for _, v := range spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == ConfigMapName(testClustersRt) {
			mountedConfig = true
		}
		if v.ConfigMap != nil && v.ConfigMap.Name == BrokerConfigMapName(testClustersRt) {
			mountedBroker = true
		}
	}
	g.Expect(mountedConfig).To(BeTrue(), "expected a volume backed by the config ConfigMap")
	g.Expect(mountedBroker).To(BeTrue(), "expected a volume backed by the broker-config ConfigMap")
}

func TestRenderDeploymentRabbitMQ(t *testing.T) {
	g := NewWithT(t)

	objs, err := New(testOptions(BrokerTypeRabbitMQ)).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	dep := findDeployment(objs, ResourceName(testClustersRt))
	assertDeploymentCommon(g, dep)

	// RabbitMQ: broker Secret env var is injected and references the correct Secret.
	c := dep.Spec.Template.Spec.Containers[0]
	envNames := map[string]bool{}
	for _, e := range c.Env {
		envNames[e.Name] = true
		if e.Name == brokerSecretEnv {
			g.Expect(e.ValueFrom).NotTo(BeNil())
			g.Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil())
			g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal(BrokerSecretName))
			g.Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal(BrokerSecretKey))
		}
	}
	g.Expect(envNames).To(HaveKey(brokerSecretEnv))
	g.Expect(envNames).To(HaveKey(brokerConfigEnv))

	// Config ConfigMap
	cm := findConfigMap(objs, ConfigMapName(testClustersRt))
	g.Expect(cm).NotTo(BeNil())
	g.Expect(cm.Data).To(HaveKey(ConfigFileKey))
	g.Expect(cm.Data[ConfigFileKey]).To(ContainSubstring("resource_type: clusters"))
	g.Expect(cm.Data[ConfigFileKey]).To(ContainSubstring(testHttpAPIBaseURL))

	// Broker ConfigMap
	cm = findConfigMap(objs, BrokerConfigMapName(testClustersRt))
	g.Expect(cm).NotTo(BeNil())
	g.Expect(cm.Data).To(HaveKey(BrokerConfigFileKey))
	g.Expect(cm.Data[BrokerConfigFileKey]).To(ContainSubstring("type: rabbitmq"))
}

func TestRenderDeploymentGooglePubSub(t *testing.T) {
	g := NewWithT(t)

	objs, err := New(testOptions(BrokerTypeGooglePubSub)).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	dep := findDeployment(objs, ResourceName(testClustersRt))
	assertDeploymentCommon(g, dep)

	// Google Pub/Sub: broker config env is present, but no broker Secret env
	// and no SecretKeyRef on any env var.
	c := dep.Spec.Template.Spec.Containers[0]
	envNames := map[string]bool{}
	for _, e := range c.Env {
		envNames[e.Name] = true
		g.Expect(e.ValueFrom).To(BeNil(), "Google Pub/Sub env %s should not reference a Secret", e.Name)
	}
	g.Expect(envNames).To(HaveKey(brokerConfigEnv))
	g.Expect(envNames).NotTo(HaveKey(brokerSecretEnv))

	// Config ConfigMap
	cm := findConfigMap(objs, ConfigMapName(testClustersRt))
	g.Expect(cm).NotTo(BeNil())
	g.Expect(cm.Data).To(HaveKey(ConfigFileKey))
	g.Expect(cm.Data[ConfigFileKey]).To(ContainSubstring("resource_type: clusters"))
	g.Expect(cm.Data[ConfigFileKey]).To(ContainSubstring(testHttpAPIBaseURL))

	// Broker ConfigMap
	cm = findConfigMap(objs, BrokerConfigMapName(testClustersRt))
	g.Expect(cm).NotTo(BeNil())
	g.Expect(cm.Data[BrokerConfigFileKey]).To(ContainSubstring("type: googlepubsub"))
	g.Expect(cm.Data[BrokerConfigFileKey]).To(ContainSubstring("project_id: test-project"))
	g.Expect(cm.Data[BrokerConfigFileKey]).NotTo(ContainSubstring("rabbitmq"))
}

func TestRenderErrorsOnUnknownBrokerType(t *testing.T) {
	g := NewWithT(t)

	opts := testOptions(BrokerTypeRabbitMQ)
	opts.BrokerInfo = &BrokerInfo{Type: "not-a-broker"}
	_, err := New(opts).Render(context.Background(), testCR())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("unknown"))
}

func TestRenderErrorsOnGooglePubSubWithoutProjectID(t *testing.T) {
	g := NewWithT(t)

	opts := testOptions(BrokerTypeGooglePubSub)
	opts.BrokerInfo = &BrokerInfo{Type: BrokerTypeGooglePubSub}
	_, err := New(opts).Render(context.Background(), testCR())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(BrokerGooglePubSubProjectIDEnv))
}

func TestRenderNetworkPolicyDeniesIngress(t *testing.T) {
	g := NewWithT(t)

	objs, err := New(testOptions(BrokerTypeRabbitMQ)).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	np, ok := byKind(objs)["NetworkPolicy"].(*networkingv1.NetworkPolicy)
	g.Expect(ok).To(BeTrue())
	g.Expect(np.Name).To(Equal(ResourceName(testClustersRt)))
	g.Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
	g.Expect(np.Spec.Ingress).To(BeEmpty())
}

func TestRenderServiceAccount(t *testing.T) {
	g := NewWithT(t)

	objs, err := New(testOptions(BrokerTypeRabbitMQ)).Render(context.Background(), testCR())
	g.Expect(err).NotTo(HaveOccurred())

	sa, ok := byKind(objs)["ServiceAccount"].(*corev1.ServiceAccount)
	g.Expect(ok).To(BeTrue())
	g.Expect(sa.Name).To(Equal(ResourceName(testClustersRt)))
	g.Expect(sa.Namespace).To(Equal(testNamespace))
}

// --- Conditions ---

func deploymentWithStatus(specReplicas, statusReplicas, availableReplicas, updatedReplicas int32, generation int64) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: ResourceName(testClustersRt), Generation: generation},
		Spec:       appsv1.DeploymentSpec{Replicas: new(specReplicas)},
		Status: appsv1.DeploymentStatus{
			Replicas:           statusReplicas,
			AvailableReplicas:  availableReplicas,
			UpdatedReplicas:    updatedReplicas,
			ObservedGeneration: generation,
		},
	}
}

func TestConditionsNoDeployment(t *testing.T) {
	g := NewWithT(t)

	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), nil)
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Message).To(ContainSubstring("does not exist"))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
}

func TestConditionsZeroAvailableReplicas(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 1, 0, 0, 1)
	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentUnavailable))
}

func TestConditionsPartiallyReady(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(2, 2, 1, 1, 1)
	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentNotReady))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
}

func TestConditionsFullyReadyAndStable(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 1, 1, 1, 1)
	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionFalse))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutComplete))
}

func TestConditionsScaledToZero(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(0, 0, 0, 0, 1)
	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
}

func TestConditionsMidRollout(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 1, 1, 1, 2)
	dep.Status.ObservedGeneration = 1
	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
}

func TestConditionsRolloutStillProgressingWhileOldReplicasRemain(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 2, 1, 1, 1)
	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
}

func TestConditionsRolloutStillProgressingWhileUpdatedReplicasAreUnavailable(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 1, 0, 1, 1)
	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Reason).To(Equal(hyperfleetv1alpha1.ReasonRolloutInProgress))
}

func TestConditionsFindsDeploymentInMixedKindSlice(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 1, 1, 1, 1)
	applied := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: ResourceName(testClustersRt)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName(testClustersRt)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: BrokerConfigMapName(testClustersRt)}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: ResourceName(testClustersRt)}},
		dep,
	}

	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), applied)
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Reason).To(Equal(hyperfleetv1alpha1.ReasonDeploymentAvailable))
}

func TestConditionsNilSpecReplicasDefaultsToOne(t *testing.T) {
	g := NewWithT(t)

	dep := deploymentWithStatus(1, 1, 1, 1, 1)
	dep.Spec.Replicas = nil

	conds, err := New(testOptions(BrokerTypeRabbitMQ)).Conditions(context.Background(), testCR(), []client.Object{dep})
	g.Expect(err).NotTo(HaveOccurred())

	byType := condMapByType(conds)
	g.Expect(byType[hyperfleetv1alpha1.ConditionAvailable].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(byType[hyperfleetv1alpha1.ConditionProgressing].Status).To(Equal(metav1.ConditionFalse))
}

// TestComponentNameIncludesResourceType covers the component identity contract:
// condition messages are generic ("the Sentinel Deployment ..."), so the
// resource type travels on the component name — the status.components key and
// metrics operand label partners read (HYPERFLEET-1653).
func TestComponentNameIncludesResourceType(t *testing.T) {
	g := NewWithT(t)

	opts := testOptions(BrokerTypeRabbitMQ)
	opts.ResourceType = testNodepoolsRt

	g.Expect(New(opts).Name()).To(Equal(fmt.Sprintf("%s-%s", ComponentName, testNodepoolsRt)))
}

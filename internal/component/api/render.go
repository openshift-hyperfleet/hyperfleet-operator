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

// Package api renders and reports on the HyperFleet API component — the single
// component in the shared tier of every bundle (see internal/bundle). The
// builders here are pure functions of (CR, image, namespace): they never read or
// write the cluster. The controller does the applying.
//
// Operand shapes are translated from the source-of-truth Helm chart
// (hyperfleet-api/charts). Three deliberate departures from the chart, per the
// 1407 design:
//   - Naming is uniform. The chart names the Service by chart-name and the rest
//     by release-fullname; here every operand shares ResourceName so ownership
//     and selectors are obvious. The chart's naming asymmetry is not replicated.
//   - Role/RoleBinding are synthesized. The chart ships none (the API talks only
//     to PostgreSQL, never the Kubernetes API), so the Role carries no rules; we
//     still render it to satisfy the story's explicit RBAC operand and pre-wire
//     the pattern for future components.
//   - The service-account token is not auto-mounted. Because the API never calls
//     the Kubernetes API, the pod opts out of the token mount (the chart leaves
//     the Kubernetes default, which mounts it).
//
// Fields that depend on the CR spec — the config.yaml content, database
// credential env, and conditional TLS/JWKS Secret mounts — are wired here as of
// HYPERFLEET-1408. Sizing (replicas and resource requests/limits) remains the
// fixed 1407 baseline: profile→resources mapping is out of 1408's scope.
package api

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

const (
	// ResourceName is the shared metadata.name for the API operands that are not
	// the ConfigMap (Deployment, Service, ServiceAccount, Role, RoleBinding).
	ResourceName = "hyperfleet-api"
	// ConfigMapName is the metadata.name of the API's configuration ConfigMap.
	ConfigMapName = "hyperfleet-api-config"
	// ConfigFileKey is the ConfigMap data key holding the rendered config.yaml. It
	// is the shared contract between the renderer (configMap) and the controller's
	// rollout hash (stampConfigHash), so both must reference this constant rather
	// than the bare string.
	ConfigFileKey = "config.yaml"
	// ComponentName is both the component's Name() and its
	// app.kubernetes.io/component label value.
	ComponentName = "api"

	// managedByOperator marks operands as operator-managed (the chart uses Helm
	// here; we do not go through Helm).
	managedByOperator = "hyperfleet-operator"
	partOfHyperfleet  = "hyperfleet"

	// Container/Service port names and numbers, matching the chart defaults.
	portNameHTTP    = "http"
	portNameHealth  = "health"
	portNameMetrics = "metrics"
	portHTTP        = 8000
	portHealth      = 8080
	portMetrics     = 9090

	// configMountPath is where the config ConfigMap is mounted and where the
	// HYPERFLEET_CONFIG env var points.
	configMountPath = "/etc/hyperfleet"
	configFilePath  = "/etc/hyperfleet/config.yaml"
	configVolume    = "config"
	tmpVolume       = "tmp"
)

// Label keys are declared once to keep them in sync between the common and
// selector label sets.
const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelComponent = "app.kubernetes.io/component"
	labelPartOf    = "app.kubernetes.io/part-of"
	labelManagedBy = "app.kubernetes.io/managed-by"
)

// labels returns the common label set stamped on every operand's metadata. Only
// the instance name varies per CR; every other value is a package constant, so
// the helper takes just the name rather than the whole CR.
func labels(name string) map[string]string {
	return map[string]string{
		labelName:      ResourceName,
		labelInstance:  name,
		labelComponent: ComponentName,
		labelPartOf:    partOfHyperfleet,
		labelManagedBy: managedByOperator,
	}
}

// selectorLabels returns the immutable subset used for the Deployment selector,
// the pod template labels and the Service selector. It must stay stable: a
// Deployment's selector cannot be changed after creation. Every value here is
// immutable (ResourceName and ComponentName are constants; the CR name is pinned
// to the singleton "cluster").
func selectorLabels(name string) map[string]string {
	return map[string]string{
		labelName:      ResourceName,
		labelInstance:  name,
		labelComponent: ComponentName,
	}
}

// deployment builds the API Deployment. Image is injected by the operator.
// Database credentials are delivered via a read-only Secret mount plus
// HYPERFLEET_DATABASE_*_FILE env vars (HYPERFLEET-1603, api.DefaultImage
// v0.4.0+), and TLS/JWKS Secrets are mounted read-only when the corresponding
// spec fields are set (HYPERFLEET-1408). Replicas and resources remain the
// fixed 1407 baseline (profile→resources is out of scope for 1408). The
// config-hash pod-template annotation is added later by the controller, not
// here.
func deployment(cr *hyperfleetv1alpha1.HyperFleetConfig, image, namespace string) *appsv1.Deployment {
	dbSecret := cr.Spec.API.Database.SecretRef.Name

	// Base env: config file location plus the five database credential file
	// paths. Values are never inlined — each var is a literal path into the
	// read-only mount below, resolved by the API at startup (ResolveFileOverrides).
	env := []corev1.EnvVar{
		{Name: envConfig, Value: configFilePath},
		{Name: envDBHostFile, Value: dbHostFilePath},
		{Name: envDBPortFile, Value: dbPortFilePath},
		{Name: envDBNameFile, Value: dbNameFilePath},
		{Name: envDBUsernameFile, Value: dbUsernameFilePath},
		{Name: envDBPasswordFile, Value: dbPasswordFilePath},
	}

	// Base volumes/mounts: the config ConfigMap and the database Secret
	// (both read-only) plus writable /tmp. The database mount is unconditional —
	// spec.api.database.secretRef is required, unlike the TLS/JWKS mounts
	// appended below only when their optional spec fields are set.
	volumeMounts := []corev1.VolumeMount{
		{Name: configVolume, MountPath: configMountPath, ReadOnly: true},
		{Name: dbVolume, MountPath: dbMountPath, ReadOnly: true},
		{Name: tmpVolume, MountPath: "/tmp"},
	}
	volumes := []corev1.Volume{
		{
			Name: configVolume,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: ConfigMapName},
				},
			},
		},
		{
			Name: dbVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: dbSecret},
			},
		},
		{
			Name: tmpVolume,
			// Writable scratch: the container runs with ReadOnlyRootFilesystem: true
			// (see the container SecurityContext), so /tmp must be backed by an
			// ephemeral EmptyDir for any process that writes temporary files.
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	// TLS: mount the kubernetes.io/tls Secret read-only; its tls.crt/tls.key
	// surface as files that config.yaml's server.tls.{cert,key}_file point at.
	if cr.Spec.API.TLS != nil {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: tlsVolume, MountPath: tlsMountPath, ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: tlsVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: cr.Spec.API.TLS.SecretRef.Name},
			},
		})
	}

	// JWKS: mount the JWKS Secret read-only only when auth is enabled AND the CR
	// pins a Secret source (jwkCertSecretRef); the jwks.json key surfaces as the
	// file config.yaml's jwk_cert_file points at. The URL and OIDC-discovery paths
	// need no mount. The AuthEnabled guard matches resolveJWKSource (api.go) and
	// referencedSecretData (rollout.go): with auth off the API never reads the JWKS
	// file, so mounting it would inject an unused Secret and, if that Secret is
	// absent, fail pod start despite auth being disabled.
	if AuthEnabled(cr) && cr.Spec.API.Auth.JWKCertSecretRef != nil {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: jwksVolume, MountPath: jwksMountPath, ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: jwksVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: cr.Spec.API.Auth.JWKCertSecretRef.Name},
			},
		})
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourceName,
			Namespace: namespace,
			Labels:    labels(cr.Name),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(cr.Name)},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       ptr.To(intstr.FromInt32(1)),
					MaxUnavailable: ptr.To(intstr.FromInt32(0)),
				},
			},
			Template: corev1.PodTemplateSpec{
				// Pods carry the full recommended label set; the Deployment selector
				// (above) is the immutable subset of these, which stays valid because
				// selectorLabels ⊆ labels.
				ObjectMeta: metav1.ObjectMeta{Labels: labels(cr.Name)},
				Spec: corev1.PodSpec{
					// The API never calls the Kubernetes API (its Role is empty),
					// so it needs no service-account token. Opting out of the
					// automatic mount removes an unused credential from every pod
					// and shrinks the attack surface.
					AutomountServiceAccountToken:  ptr.To(false),
					ServiceAccountName:            ResourceName,
					TerminationGracePeriodSeconds: ptr.To[int64](70),
					SecurityContext: &corev1.PodSecurityContext{
						// Do not set a fixed UID or fsGroup. OpenShift's restricted SCC
						// assigns values from the project's allocated range, while the
						// image remains required to run as non-root on every platform.
						RunAsNonRoot: ptr.To(true),
					},
					Containers: []corev1.Container{{
						Name:            ResourceName,
						Image:           image,
						ImagePullPolicy: corev1.PullAlways, // chart: values.image.pullPolicy (default Always)
						WorkingDir:      "/app",
						Args:            []string{"serve"},
						Ports: []corev1.ContainerPort{
							{Name: portNameHTTP, ContainerPort: portHTTP, Protocol: corev1.ProtocolTCP},
							{Name: portNameHealth, ContainerPort: portHealth, Protocol: corev1.ProtocolTCP},
							{Name: portNameMetrics, ContainerPort: portMetrics, Protocol: corev1.ProtocolTCP},
						},
						Env: env,
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString(portNameHealth)},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
							TimeoutSeconds:      5,
							FailureThreshold:    3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString(portNameHealth)},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       5,
							TimeoutSeconds:      3,
							FailureThreshold:    3,
						},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: volumeMounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// service builds the API's ClusterIP Service. Ports target the container ports
// by name so they stay correct even if numbers change.
func service(cr *hyperfleetv1alpha1.HyperFleetConfig, namespace string) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourceName,
			Namespace: namespace,
			Labels:    labels(cr.Name),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(cr.Name),
			Ports: []corev1.ServicePort{
				{Name: portNameHTTP, Port: portHTTP, TargetPort: intstr.FromString(portNameHTTP), Protocol: corev1.ProtocolTCP},
				{Name: portNameHealth, Port: portHealth, TargetPort: intstr.FromString(portNameHealth), Protocol: corev1.ProtocolTCP},
				{Name: portNameMetrics, Port: portMetrics, TargetPort: intstr.FromString(portNameMetrics), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// serviceAccount builds the API's identity.
func serviceAccount(cr *hyperfleetv1alpha1.HyperFleetConfig, namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourceName,
			Namespace: namespace,
			Labels:    labels(cr.Name),
		},
	}
}

// configMap builds the API's configuration ConfigMap. The config.yaml content is
// rendered from the CR spec by renderConfig (see config.go) and passed in, so
// this builder stays a pure object constructor.
func configMap(cr *hyperfleetv1alpha1.HyperFleetConfig, namespace, configYAML string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: namespace,
			Labels:    labels(cr.Name),
		},
		Data: map[string]string{ConfigFileKey: configYAML},
	}
}

// role builds the API's Role. The rules are intentionally empty: the API talks
// only to PostgreSQL and never to the Kubernetes API, so it needs no in-cluster
// permissions. The Role (and its binding) exist to satisfy the story's explicit
// RBAC operand and to pre-wire the pattern for future components that may need
// rules.
func role(cr *hyperfleetv1alpha1.HyperFleetConfig, namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourceName,
			Namespace: namespace,
			Labels:    labels(cr.Name),
		},
		Rules: []rbacv1.PolicyRule{},
	}
}

// roleBinding binds the API's Role to its ServiceAccount. The Role is empty, so
// creating this binding needs no bind/escalate permission today (the operator
// trivially "holds" the zero permissions it grants). When a future component
// ships a Role WITH rules (HYPERFLEET-1408+), the operator's own ClusterRole
// must either hold those permissions or gain `bind`/`escalate` on them, or the
// apply will fail on a real (RBAC-enforcing) cluster — note that envtest does
// not enforce RBAC, so such a regression would not surface in the suite.
func roleBinding(cr *hyperfleetv1alpha1.HyperFleetConfig, namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourceName,
			Namespace: namespace,
			Labels:    labels(cr.Name),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     ResourceName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      ResourceName,
			Namespace: namespace,
		}},
	}
}

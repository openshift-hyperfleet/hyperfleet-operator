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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required. Any new fields you add must have json tags for
// the fields to be serialized. Run "make generate manifests" after editing.

// SingletonName is the only permitted name for a HyperFleetConfig. The resource
// is a cluster-scoped singleton: a CEL validation rule pins the name to
// "cluster", and cluster-scoped name uniqueness then guarantees at most one
// instance (a second create is rejected by the API server as AlreadyExists, not
// by CEL, since CEL cannot see other objects). No admission webhooks are used
// (see architecture ADR-0019).
const SingletonName = "cluster"

// BundleType selects one of the operator-internal bundle definitions. It is a
// selector only: the CR carries the choice of a bundle, never its contents. The
// bundle controller (HYPERFLEET-1407) resolves the selected bundle into a
// concrete component set.
//
// These constants are the single source of truth for the valid bundle values
// and MUST stay in lockstep with the bundle definitions shipped inside the
// operator. Adding a bundle means adding a constant here and the matching enum
// value in the +kubebuilder:validation:Enum marker below.
//
// +kubebuilder:validation:Enum=cloud-capi;onprem-agent
type BundleType string

const (
	// BundleCloudCAPI is the cloud, CAPI-based provisioning deployment
	// (managed OpenShift on a public cloud).
	BundleCloudCAPI BundleType = "cloud-capi"
	// BundleOnPremAgent is the on-premise, air-gapped (agent-based) deployment.
	BundleOnPremAgent BundleType = "onprem-agent"
)

// AllBundleTypes is the Go-level source of truth for the valid BundleType
// values. The +kubebuilder:validation:Enum marker on BundleType must list
// exactly these values; the lockstep guard test creates a HyperFleetConfig with
// each entry and fails if any is not accepted by the CRD, catching drift between
// the constants and the enum marker.
var AllBundleTypes = []BundleType{BundleCloudCAPI, BundleOnPremAgent}

// SizingProfile expresses sizing intent, not replica engineering. The operator
// maps each profile to concrete replicas, resource requests/limits and HPA/PDB
// defaults for the operand.
//
// +kubebuilder:validation:Enum=small;medium;large
type SizingProfile string

const (
	// SizingProfileSmall is the default, lowest-footprint sizing profile.
	SizingProfileSmall SizingProfile = "small"
	// SizingProfileMedium is a mid-range sizing profile.
	SizingProfileMedium SizingProfile = "medium"
	// SizingProfileLarge is the highest-footprint sizing profile.
	SizingProfileLarge SizingProfile = "large"
)

// AllSizingProfiles is the Go-level source of truth for the valid SizingProfile
// values; it must match the +kubebuilder:validation:Enum marker on
// SizingProfile (asserted by the lockstep guard test).
var AllSizingProfiles = []SizingProfile{SizingProfileSmall, SizingProfileMedium, SizingProfileLarge}

// Condition types reported on HyperFleetConfig status. This is deliberate
// operator-layer vocabulary describing installation health, and is distinct from
// the HyperFleet API's own resource-condition vocabulary (Available/Ready/
// Reconciled/LastKnownReconciled/per-adapter; see architecture ADR-0007 and
// ADR-0008). Populated by the bundle controller as of HYPERFLEET-1409.
const (
	// ConditionAvailable is True when the installed operand (the API) is
	// deployed and healthy.
	ConditionAvailable = "Available"
	// ConditionProgressing is True while the operator is actively rolling out a
	// change to the operand.
	ConditionProgressing = "Progressing"
	// ConditionDegraded is True when the operator cannot reach or maintain the
	// desired state.
	ConditionDegraded = "Degraded"
)

// Reason strings for the operator-layer conditions above (HYPERFLEET-1409).
// These are published API vocabulary — partners may read status.conditions[].reason
// — so every writer of a condition must use one of these constants rather than an
// ad hoc string, and the set must stay documented in docs/status-conditions.md.
const (
	// ReasonDeploymentAvailable: Available=True — the operand Deployment reports
	// Available and all desired replicas are ready.
	ReasonDeploymentAvailable = "DeploymentAvailable"
	// ReasonDeploymentUnavailable: Available=False — the operand Deployment is
	// missing or has zero available replicas.
	ReasonDeploymentUnavailable = "DeploymentUnavailable"
	// ReasonDeploymentNotReady: Available=False — the operand Deployment exists
	// with some, but not all, replicas ready.
	ReasonDeploymentNotReady = "DeploymentNotReady"
	// ReasonRolloutInProgress: Progressing=True — the operand Deployment has not
	// finished rolling out its current generation.
	ReasonRolloutInProgress = "RolloutInProgress"
	// ReasonRolloutComplete: Progressing=False — the operand Deployment is fully
	// rolled out and stable.
	ReasonRolloutComplete = "RolloutComplete"
	// ReasonAsExpected: Degraded=False — no failure signal (ClusterOperator
	// convention default).
	ReasonAsExpected = "AsExpected"
	// ReasonReferencedSecretMissing: Degraded=True — a Secret referenced by the
	// spec (database, TLS, or JWKS) does not exist in the operator's namespace.
	ReasonReferencedSecretMissing = "ReferencedSecretMissing"
	// ReasonReconcileError: Degraded=True — a component failed to render or
	// apply, or JWKS discovery failed with no cached fallback available, during
	// the most recent reconcile.
	ReasonReconcileError = "ReconcileError"
)

// SecretReference references a Secret by name. Referenced Secrets must live in
// the operator's own namespace: because HyperFleetConfig is cluster-scoped, no
// namespace field is exposed (name-only + operator-namespace convention, decided
// in the HYPERFLEET-1406 API review).
//
// The operator-namespace constraint is convention-only at the schema level —
// CEL sees no cross-object/namespace state, and ADR-0019 rules out webhooks —
// but the reconciler does resolve every reference in its own namespace
// (referencedSecretData) and surfaces ConditionDegraded /
// ReasonReferencedSecretMissing when one is absent (HYPERFLEET-1409).
// TODO(HYPERFLEET-1512): decide whether any further enforcement (e.g. rejecting
// reconciliation outright) belongs here.
type SecretReference struct {
	// name is the name of the Secret in the operator's namespace. It must be a
	// valid DNS-1123 subdomain, matching what k8s.io/apimachinery/pkg/util/validation
	// enforces for Secret names (IsDNS1123Subdomain, max length 253), so an
	// unresolvable reference is rejected at admission rather than failing opaquely
	// when the reference is later resolved.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Maintainer note (free-standing so it stays out of the generated CRD
	// description): the MaxLength and Pattern markers on name are literal copies of
	// apimachinery's DNS1123SubdomainMaxLength (253) and dns1123SubdomainFmt. They
	// can't reference those symbols — controller-gen markers accept only literal
	// values, and the format constant is unexported — so keep them in sync by hand
	// if k8s.io/apimachinery/pkg/util/validation ever changes.
}

// DatabaseSpec configures the HyperFleet API's connection to its external
// PostgreSQL database. The database is partner-provided; the operator never
// provisions it.
type DatabaseSpec struct {
	// secretRef references a Secret holding the database connection credentials.
	// The Secret must provide the keys db.host, db.port, db.name, db.user and
	// db.password.
	//
	// +kubebuilder:validation:Required
	SecretRef SecretReference `json:"secretRef"`
}

// AuthSpec configures partner-facing JWT authentication intent for the API.
// Machinery details (JWKS rotation, public-path allowlist) remain
// operator-internal defaults and are not exposed here.
//
// The JWKS source is optional partner intent: for air-gapped or private
// environments, a partner may supply the key set from a Secret
// (jwkCertSecretRef). When unset, the operator derives the JWKS URL from the
// issuer via OIDC discovery — see HYPERFLEET-1408. A pinned-URL override
// (jwkCertURL) was considered and deliberately left out of v1alpha1: every
// field here is a long-term compatibility commitment (ADR-0019's CR-minimalism
// principle), discovery already covers any standards-compliant issuer, and
// in-app JWT validation is itself defense-in-depth behind the API gateway
// (ADR-0020) — not a case that obviously needs a third configuration knob.
// Adding it later is compatible with existing CRs; removing it would not be.
//
// +kubebuilder:validation:XValidation:rule="!self.enabled || (has(self.issuer) && has(self.audience))",message="issuer and audience are required when auth is enabled"
type AuthSpec struct {
	// enabled turns JWT authentication on for the API endpoint. It defaults to
	// true, so a config that omits it gets authentication ON. It is a pointer to
	// distinguish "unset" (apply the default, true) from an explicit false
	// (disable auth), which a non-pointer bool cannot express: with omitempty a
	// plain false is dropped and re-defaulted to true, so auth could never be
	// turned off via the typed client; without omitempty an unset field serializes
	// as false and suppresses the default. Only *bool avoids both traps.
	//
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// issuer is the OIDC issuer URL that mints accepted tokens. Required when
	// enabled is true. Whenever it is set (regardless of enabled) it must be a
	// valid https URL with a host, so a malformed issuer is rejected at admission
	// rather than surfacing later at token-validation time.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() == 'https' && url(self).getHostname() != ''",message="issuer must be a valid https URL"
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// audience is the token audience the API requires. Required and non-empty
	// when enabled is true.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Audience string `json:"audience,omitempty"`

	// jwkCertSecretRef optionally references a Secret holding the JWKS document,
	// for air-gapped or private environments where the API cannot reach a JWKS
	// URL. The Secret must provide the key "jwks.json" containing a JSON Web Key
	// Set (the format the API parses; see HYPERFLEET-1408). When unset, the
	// operator derives the JWKS URL from the issuer via OIDC discovery
	// ({issuer}/.well-known/openid-configuration → jwks_uri).
	//
	// +optional
	JWKCertSecretRef *SecretReference `json:"jwkCertSecretRef,omitempty"`
}

// TLSSpec configures TLS for the API endpoint. The certificate material is
// referenced, not described.
type TLSSpec struct {
	// secretRef references a kubernetes.io/tls Secret (providing tls.crt and
	// tls.key) used to serve the API endpoint.
	//
	// +kubebuilder:validation:Required
	SecretRef SecretReference `json:"secretRef"`
}

// APISpec is the partner-facing configuration for the HyperFleet API component,
// which lives in the shared tier of every bundle.
type APISpec struct {
	// database configures the external PostgreSQL connection.
	//
	// +kubebuilder:validation:Required
	Database DatabaseSpec `json:"database"`

	// auth configures partner-facing JWT authentication intent.
	//
	// +kubebuilder:validation:Required
	Auth AuthSpec `json:"auth"`

	// tls optionally configures TLS for the API endpoint. When omitted, the
	// operator applies its default serving configuration.
	//
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// profile selects a sizing profile for the API. Defaults to "small".
	//
	// +kubebuilder:default=small
	// +optional
	Profile SizingProfile `json:"profile,omitempty"`
}

// HyperFleetConfigSpec defines the desired state of HyperFleetConfig. It captures
// partner intent only; internal machinery (broker, adapters, sentinel) is never
// expressed here.
type HyperFleetConfigSpec struct {
	// bundle selects one of the operator-internal bundle definitions. It is
	// immutable after creation: switching deployments requires recreating the
	// resource.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bundle is immutable"
	Bundle BundleType `json:"bundle"`

	// api is the partner-facing configuration for the HyperFleet API component.
	//
	// +kubebuilder:validation:Required
	API APISpec `json:"api"`
}

// HyperFleetConfigStatus defines the observed state of HyperFleetConfig. It is
// populated by the bundle controller after every reconcile (HYPERFLEET-1409),
// rolling up each component's health into the Available/Progressing/Degraded
// conditions.
type HyperFleetConfigStatus struct {
	// observedGeneration is the .metadata.generation the operator last acted on.
	//
	// +kubebuilder:validation:Minimum=0
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current installation health of the operand.
	// Recognized types are Available, Progressing and Degraded.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hfc
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="the only permitted name is 'cluster'; HyperFleetConfig is a cluster-scoped singleton"
// +kubebuilder:printcolumn:name="Bundle",type=string,JSONPath=`.spec.bundle`
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.api.profile`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Progressing",type=string,JSONPath=`.status.conditions[?(@.type=="Progressing")].status`
// +kubebuilder:printcolumn:name="Degraded",type=string,JSONPath=`.status.conditions[?(@.type=="Degraded")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HyperFleetConfig is the Schema for the hyperfleetconfigs API. It is a
// cluster-scoped singleton: exactly one instance, named "cluster", is permitted.
type HyperFleetConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   HyperFleetConfigSpec   `json:"spec"`
	Status HyperFleetConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HyperFleetConfigList contains a list of HyperFleetConfig.
type HyperFleetConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HyperFleetConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HyperFleetConfig{}, &HyperFleetConfigList{})
}

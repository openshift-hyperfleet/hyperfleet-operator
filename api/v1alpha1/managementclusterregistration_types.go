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
	"k8s.io/apimachinery/pkg/runtime"
)

// RemoteApplierCallerClass is the only caller class represented by a
// ManagementClusterRegistration. It is not selectable by the registering user.
const RemoteApplierCallerClass = "remote-applier"

// ManagementClusterRegistrationSpec binds one remote Applier identity to one
// Desire Store partition. All fields are required and mutable. Values are
// matched exactly; admission does not normalize issuer URLs or identities.
type ManagementClusterRegistrationSpec struct {
	// issuerURL is the exact OIDC issuer URL of the management cluster. HTTPS
	// is required; user information, query strings and fragments are forbidden.
	// Private hosts are allowed: issuer publication uses an approved private path.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^/?#@[:space:]]+(/[^?#[:space:]]*)?$`
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() == 'https' && url(self).getHostname() != ''",message="issuerURL must be a valid HTTPS issuer URL"
	IssuerURL string `json:"issuerURL"`

	// subject is the exact Kubernetes service-account subject accepted for the
	// remote Applier, not a wildcard or a group of service accounts.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=339
	// +kubebuilder:validation:Pattern=`^system:serviceaccount:[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?:[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self.split(':').size() == 4 && self.split(':')[3].size() <= 253",message="subject must contain a service-account name of at most 253 characters"
	Subject string `json:"subject"`

	// audience is the exact audience required in the management-cluster token.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]]+$`
	Audience string `json:"audience"`

	// partition is the only Desire Store partition authorized for this identity.
	// It is a DNS label because it also selects the per-partition gateway host.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Partition string `json:"partition"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Partition",type=string,JSONPath=`.spec.partition`
// +kubebuilder:printcolumn:name="Issuer",type=string,JSONPath=`.spec.issuerURL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ManagementClusterRegistration is a Hub-administrator-owned registration of a
// remote Applier identity and its authorized Desire Store partition. Management
// clusters must never be granted permission to create or modify these records.
type ManagementClusterRegistration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec ManagementClusterRegistrationSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ManagementClusterRegistrationList contains management-cluster registrations.
type ManagementClusterRegistrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagementClusterRegistration `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ManagementClusterRegistration{}, &ManagementClusterRegistrationList{})
		return nil
	})
}

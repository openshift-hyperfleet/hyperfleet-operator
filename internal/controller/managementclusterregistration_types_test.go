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
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

const (
	registrationAudienceField = "audience"
	registrationSpecField     = "spec"
)

func validManagementClusterRegistration() *hyperfleetv1alpha1.ManagementClusterRegistration {
	return &hyperfleetv1alpha1.ManagementClusterRegistration{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "registration-"},
		Spec: hyperfleetv1alpha1.ManagementClusterRegistrationSpec{
			IssuerURL: testIssuerURL,
			Subject:   "system:serviceaccount:hyperfleet:applier",
			Audience:  "hyperfleet-desire-store",
			Partition: "mc-01",
		},
	}
}

var _ = Describe("ManagementClusterRegistration CRD validation", func() {
	It("preserves exact bindings, lists registrations and permits identity and partition changes", func() {
		obj := validManagementClusterRegistration()
		obj.Spec.IssuerURL = "https://issuer.mc-01.internal:6443/oidc/"
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, obj)).To(Succeed()) })

		stored := &hyperfleetv1alpha1.ManagementClusterRegistration{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), stored)).To(Succeed())
		Expect(stored.Spec).To(Equal(obj.Spec))
		Expect(stored.Namespace).To(BeEmpty())

		list := &hyperfleetv1alpha1.ManagementClusterRegistrationList{}
		Expect(k8sClient.List(ctx, list)).To(Succeed())
		Expect(list.Items).To(ContainElement(HaveField("Name", obj.Name)))

		stored.Spec = hyperfleetv1alpha1.ManagementClusterRegistrationSpec{
			IssuerURL: "https://replacement.internal/issuer",
			Subject:   "system:serviceaccount:other:replacement.applier",
			Audience:  "replacement-audience",
			Partition: "mc-02",
		}
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		updated := &hyperfleetv1alpha1.ManagementClusterRegistration{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), updated)).To(Succeed())
		Expect(updated.Spec).To(Equal(stored.Spec))
	})

	DescribeTable("accepts valid boundary values", func(field, value string) {
		obj := registrationWithField(field, value)
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, obj)).To(Succeed()) })
	},
		Entry("single-character partition", "partition", "1"),
		Entry("63-character partition", "partition", strings.Repeat("a", 63)),
		Entry("URI audience", registrationAudienceField, "https://desire-store.internal"),
		Entry("253-character audience", registrationAudienceField, strings.Repeat("a", 253)),
		Entry("2048-character issuer", "issuerURL", testIssuerURL+"/"+strings.Repeat("a", 2048-len(testIssuerURL)-1)),
		Entry("maximum service-account subject", "subject",
			"system:serviceaccount:"+strings.Repeat("n", 63)+":"+
				strings.Repeat("a", 63)+"."+strings.Repeat("b", 63)+"."+
				strings.Repeat("c", 63)+"."+strings.Repeat("d", 61)),
	)

	DescribeTable("rejects invalid binding fields on create and update", func(field, value string) {
		invalid := registrationWithField(field, value)
		err := k8sClient.Create(ctx, invalid)
		Expect(errors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec." + field))

		obj := validManagementClusterRegistration()
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, obj)).To(Succeed()) })
		data, err := json.Marshal(map[string]any{registrationSpecField: map[string]any{field: value}})
		Expect(err).NotTo(HaveOccurred())
		patch := client.RawPatch(types.MergePatchType, data)
		err = k8sClient.Patch(ctx, obj, patch)
		Expect(errors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
	},
		Entry("empty issuer", "issuerURL", ""),
		Entry("HTTP issuer", "issuerURL", "http://issuer.example.com"),
		Entry("missing issuer host", "issuerURL", "https:///issuer"),
		Entry("issuer credentials", "issuerURL", "https://user:password@issuer.example.com"),
		Entry("issuer query", "issuerURL", testIssuerURL+"?key=value"),
		Entry("empty issuer query", "issuerURL", testIssuerURL+"?"),
		Entry("issuer fragment", "issuerURL", testIssuerURL+"#fragment"),
		Entry("issuer whitespace", "issuerURL", testIssuerURL+"/bad path"),
		Entry("issuer bad escape", "issuerURL", testIssuerURL+"/%zz"),
		Entry("long issuer", "issuerURL", testIssuerURL+"/"+strings.Repeat("a", 2048)),
		Entry("empty subject", "subject", ""),
		Entry("non-service-account subject", "subject", "applier"),
		Entry("wildcard subject", "subject", "system:serviceaccount:hyperfleet:*"),
		Entry("empty namespace", "subject", "system:serviceaccount::applier"),
		Entry("long namespace", "subject", "system:serviceaccount:"+strings.Repeat("n", 64)+":applier"),
		Entry("invalid account name", "subject", "system:serviceaccount:hyperfleet:Applier"),
		Entry("empty account label", "subject", "system:serviceaccount:hyperfleet:applier..other"),
		Entry("oversized account name", "subject",
			"system:serviceaccount:h:"+strings.Repeat("abc.", 63)+"ab"),
		Entry("empty audience", registrationAudienceField, ""),
		Entry("whitespace audience", registrationAudienceField, "desire store"),
		Entry("control character audience", registrationAudienceField, "desire\nstore"),
		Entry("long audience", registrationAudienceField, strings.Repeat("a", 254)),
		Entry("empty partition", "partition", ""),
		Entry("wildcard partition", "partition", "*"),
		Entry("path partition", "partition", "mc/01"),
		Entry("dot partition", "partition", "mc.01"),
		Entry("uppercase partition", "partition", "MC-01"),
		Entry("leading hyphen", "partition", "-mc"),
		Entry("trailing hyphen", "partition", "mc-"),
		Entry("long partition", "partition", strings.Repeat("a", 64)),
	)

	DescribeTable("requires the spec and every binding field", func(field string) {
		obj := registrationWithField("partition", "mc-01")
		if field == registrationSpecField {
			delete(obj.Object, registrationSpecField)
		} else {
			unstructured.RemoveNestedField(obj.Object, registrationSpecField, field)
		}
		err := k8sClient.Create(ctx, obj)
		Expect(errors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
	}, Entry(registrationSpecField, registrationSpecField), Entry("issuerURL", "issuerURL"), Entry("subject", "subject"),
		Entry(registrationAudienceField, registrationAudienceField), Entry("partition", "partition"))
})

func registrationWithField(field, value string) *unstructured.Unstructured {
	obj := validManagementClusterRegistration()
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": hyperfleetv1alpha1.GroupVersion.String(),
		"kind":       "ManagementClusterRegistration",
		"metadata":   map[string]any{"generateName": obj.GenerateName},
		registrationSpecField: map[string]any{
			"issuerURL":               obj.Spec.IssuerURL,
			"subject":                 obj.Spec.Subject,
			registrationAudienceField: obj.Spec.Audience,
			"partition":               obj.Spec.Partition,
			field:                     value,
		},
	}}
}

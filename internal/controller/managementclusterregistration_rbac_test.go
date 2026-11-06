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
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

var _ = Describe("ManagementClusterRegistration administrative RBAC", func() {
	It("allows an explicitly delegated Hub administrator and forbids remote self-registration", func() {
		const adminUser = "registration-test-admin"
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac",
			"managementclusterregistration_admin_role.yaml"))
		Expect(err).NotTo(HaveOccurred())
		role := &rbacv1.ClusterRole{}
		Expect(yaml.Unmarshal(data, role)).To(Succeed())
		Expect(role.AggregationRule).To(BeNil())
		Expect(role.Labels).NotTo(HaveKey("rbac.authorization.k8s.io/aggregate-to-admin"))
		Expect(role.Labels).NotTo(HaveKey("rbac.authorization.k8s.io/aggregate-to-edit"))
		Expect(role.Labels).NotTo(HaveKey("rbac.authorization.k8s.io/aggregate-to-view"))
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, role)).To(Succeed()) })

		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: adminUser},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name,
			},
			Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: adminUser}},
		}
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, binding)).To(Succeed()) })

		adminConfig := rest.CopyConfig(cfg)
		adminConfig.Impersonate = rest.ImpersonationConfig{UserName: adminUser}
		admin, err := client.New(adminConfig, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		remoteConfig := rest.CopyConfig(cfg)
		remoteConfig.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:management-cluster:applier",
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:management-cluster", "system:authenticated"},
		}
		remote, err := client.New(remoteConfig, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		Expect(errors.IsForbidden(remote.Create(ctx, validManagementClusterRegistration()))).To(BeTrue())

		obj := validManagementClusterRegistration()
		Eventually(func() error { return admin.Create(ctx, obj) }, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		DeferCleanup(func() {
			err := k8sClient.Delete(ctx, obj)
			Expect(err == nil || errors.IsNotFound(err)).To(BeTrue())
		})
		Expect(admin.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		Expect(admin.List(ctx, &hyperfleetv1alpha1.ManagementClusterRegistrationList{})).To(Succeed())
		obj.Spec.Partition = "mc-updated"
		Expect(errors.IsForbidden(remote.Update(ctx, obj))).To(BeTrue())
		Expect(errors.IsForbidden(remote.Patch(ctx, obj, client.MergeFrom(obj.DeepCopy())))).To(BeTrue())
		Expect(errors.IsForbidden(remote.Delete(ctx, obj))).To(BeTrue())
		Expect(admin.Update(ctx, obj)).To(Succeed())
		before := obj.DeepCopy()
		obj.Spec.Audience = "updated-audience"
		Expect(admin.Patch(ctx, obj, client.MergeFrom(before))).To(Succeed())
		Expect(admin.Delete(ctx, obj)).To(Succeed())
	})
})

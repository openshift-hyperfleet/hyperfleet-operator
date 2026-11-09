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

package e2e

import (
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openshift-hyperfleet/hyperfleet-operator/test/utils"
)

// checkRegistrationAdministration exercises the manifest installation's RBAC.
// This suite does not install OLM, whose owned-API roles grant additional access.
func checkRegistrationAdministration() {
	const (
		resource    = "managementclusterregistrations.hyperfleet.redhat.com"
		adminUser   = "registration-e2e-admin"
		editUser    = "registration-e2e-editor"
		viewUser    = "registration-e2e-viewer"
		roleName    = "hyperfleet-operator-managementclusterregistration-admin-role"
		createVerb  = "create"
		asFlag      = "--as"
		authCommand = "auth"
		canICommand = "can-i"
		patchVerb   = "patch"
		updateVerb  = "update"
	)
	for _, binding := range []struct{ name, role, user string }{
		{"registration-e2e-admin", roleName, adminUser},
		{"registration-e2e-editor", "edit", editUser},
		{"registration-e2e-generic-admin", "admin", editUser},
		{"registration-e2e-viewer", "view", viewUser},
	} {
		_, err := utils.Run(exec.Command("kubectl", createVerb, "clusterrolebinding", binding.name,
			"--clusterrole", binding.role, "--user", binding.user))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, err := utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding", binding.name,
				"--ignore-not-found=true"))
			Expect(err).NotTo(HaveOccurred())
		})
	}

	By("rejecting remote self-registration and generic roles without OLM aggregation")
	for _, subject := range []string{editUser, viewUser, "system:serviceaccount:management-cluster:applier"} {
		identityArgs := []string{asFlag, subject, asGroupFlag, authenticatedGroup}
		if strings.HasPrefix(subject, "system:serviceaccount:") {
			identityArgs = append(identityArgs, asGroupFlag, serviceAccountsGroup,
				asGroupFlag, serviceAccountsGroup+":"+strings.Split(subject, ":")[2])
		}
		for _, verb := range []string{getVerb, listVerb, watchVerb, createVerb, updateVerb, patchVerb, "delete"} {
			args := append([]string{authCommand, canICommand, verb, resource}, identityArgs...)
			Expect(managerCanIWithArgs(args, "")).To(BeFalse())
		}
		args := append([]string{createVerb}, identityArgs...)
		args = append(args, "-f", "config/samples/hyperfleet_v1alpha1_managementclusterregistration.yaml")
		cmd := exec.Command("kubectl", args...)
		output, err := utils.Run(cmd)
		Expect(err).To(HaveOccurred())
		Expect(output).To(ContainSubstring("Forbidden"))
	}

	By("allowing explicit registration administration")
	_, err := utils.Run(exec.Command("kubectl", createVerb, asFlag, adminUser,
		"-f", "config/samples/hyperfleet_v1alpha1_managementclusterregistration.yaml"))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_, err := utils.Run(exec.Command("kubectl", "delete", resource, "mc-01", "--ignore-not-found=true"))
		Expect(err).NotTo(HaveOccurred())
	})
	_, err = utils.Run(exec.Command("kubectl", patchVerb, resource, "mc-01", asFlag, adminUser,
		"--type=merge", "-p", `{"spec":{"partition":"mc-02"}}`))
	Expect(err).NotTo(HaveOccurred())
	output, err := utils.Run(exec.Command("kubectl", "get", resource, "mc-01", asFlag, adminUser,
		"-o", "jsonpath={.spec.partition}"))
	Expect(err).NotTo(HaveOccurred())
	Expect(strings.TrimSpace(output)).To(Equal("mc-02"))
	_, err = utils.Run(exec.Command("kubectl", "delete", resource, "mc-01", asFlag, adminUser))
	Expect(err).NotTo(HaveOccurred())
}

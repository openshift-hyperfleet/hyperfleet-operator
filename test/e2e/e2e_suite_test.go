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
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openshift-hyperfleet/hyperfleet-operator/test/utils"
)

var (
	kindMode                      bool
	skipCertManagerInstall        bool
	kindOperatorImg               string
	isCertManagerAlreadyInstalled bool = false
	namespace                     string
)

func init() {
	flag.BoolVar(&kindMode, "kind-mode", false,
		"build the manager image and load it into the Kind cluster before running the suite")
	flag.StringVar(&kindOperatorImg, "kind-operator-img", "localhost/hyperfleet-operator:dev-local",
		"name of the operator image to use for testing")
	flag.BoolVar(&skipCertManagerInstall, "skip-cert-manager-install", false,
		"skip CertManager installation during test setup")
	flag.StringVar(&namespace, "namespace", "hyperfleet-system", "namespace of hyperfleet components")
}

// TestE2E runs the end-to-end test suite for the HyperFleet Operator.
//
// In kind mode (--kind-mode=true) the suite builds the operator image, installs
// CRDs, and deploys the manager into the target cluster before running specs.
//
// In CI mode (--kind-mode=false) the suite expects the operator to be fully
// installed and running already (namespace, CRDs, RBAC, and deployment).
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting hyperfleet-operator integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if kindMode {
		By("building the operator image")
		provider := os.Getenv("KIND_EXPERIMENTAL_PROVIDER")
		cmd := exec.Command("make", managerImageBuildArgs(kindOperatorImg, runtime.GOARCH, provider)...)
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the operator image")

		By("loading the operator image into Kind cluster")
		err = utils.LoadImageToKindClusterWithName(kindOperatorImg)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the operator image into Kind")
	}

	// Install CertManager unless already present or explicitly skipped.
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}
})

func managerImageBuildArgs(operatorImg string, arch string, containerTool string) []string {
	args := []string{"image", fmt.Sprintf("OPERATOR_IMG=%s", operatorImg), fmt.Sprintf("PLATFORM=linux/%s", arch)}
	// if KIND_EXPERIMENTAL_PROVIDER = podman, override CONTAINER_TOOL, otherwise set to docker
	if containerTool == "podman" {
		args = append(args, "CONTAINER_TOOL=podman")
	} else {
		args = append(args, "CONTAINER_TOOL=docker")
	}
	return args
}

var _ = AfterSuite(func() {
	// Uninstall CertManager only if this suite installed it.
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}
})

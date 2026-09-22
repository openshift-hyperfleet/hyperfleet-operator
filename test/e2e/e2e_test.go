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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openshift-hyperfleet/hyperfleet-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "hyperfleet-system"

// serviceAccountName created for the project
const serviceAccountName = "hyperfleet-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "hyperfleet-operator-controller-manager-metrics-service"

// metricsPort is the plain-HTTP port the operator serves /metrics on, per the
// HyperFleet metrics standard.
const metricsPort = "9090"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the HyperFleetConfig singleton")
		cmd := exec.Command("kubectl", "delete", "hyperfleetconfig", "cluster", "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("cleaning up the curl pod for metrics")
		cmd = exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("validating that the metrics service is available")
			cmd := exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(metricsPort), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the plain-HTTP metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v http://%s.%s.svc.cluster.local:%s/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, metricsServiceName, namespace, metricsPort, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			By("verifying the built-in controller-runtime metrics are exposed")
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
			By("verifying the operator's custom HyperFleet metrics are exposed")
			Expect(metricsOutput).To(ContainSubstring(
				"hyperfleet_operator_up",
			))
		})

		It("should scope operand authorization to the operator namespace", func() {
			By("verifying the manager can manage operands in its own namespace")
			operandResources := []string{
				"deployments.apps",
				"services",
				"serviceaccounts",
				"configmaps",
				"roles.rbac.authorization.k8s.io",
				"rolebindings.rbac.authorization.k8s.io",
			}
			operandVerbs := []string{"get", "list", "watch", "create", "update", "patch"}
			for _, resource := range operandResources {
				for _, verb := range operandVerbs {
					Expect(managerCanI(verb, resource, namespace)).To(Equal("yes"),
						"manager should be allowed to %s %s in %s", verb, resource, namespace)
				}
			}

			By("verifying the manager cannot manage operands in another namespace")
			for _, resource := range operandResources {
				for _, verb := range operandVerbs {
					Expect(managerCanI(verb, resource, "default")).To(Equal("no"),
						"manager should not be allowed to %s %s in default", verb, resource)
				}
				Expect(managerCanI("delete", resource, namespace)).To(Equal("no"),
					"manager should not be allowed to delete %s; owner-reference GC handles cleanup", resource)
			}

			By("verifying cluster-scoped HyperFleetConfig access remains available")
			for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
				Expect(managerCanI(verb, "hyperfleetconfigs.hyperfleet.redhat.com", "")).To(Equal("yes"),
					"manager should retain cluster-scoped HyperFleetConfig %s access", verb)
			}
			Expect(managerCanISubresource("update", "hyperfleetconfigs", "status")).To(Equal("yes"))
			Expect(managerCanISubresource("update", "hyperfleetconfigs", "finalizers")).To(Equal("yes"))
		})

		It("should reconcile all operands in the operator namespace", func() {
			By("creating the referenced database Secret")
			cmd := exec.Command("kubectl", "create", "secret", "generic", "hyperfleet-db",
				"-n", namespace,
				"--from-literal=db.host=db.example.com",
				"--from-literal=db.port=5432",
				"--from-literal=db.name=hyperfleet",
				"--from-literal=db.user=hyperfleet",
				"--from-literal=db.password=password",
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating the referenced JWKS Secret")
			cmd = exec.Command("kubectl", "create", "secret", "generic", "hyperfleet-jwks",
				"-n", namespace,
				`--from-literal=jwks.json={"keys":[]}`,
			)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating the cluster-scoped HyperFleetConfig")
			cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/hyperfleetconfig.yaml")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the controller to create every operand")
			operands := [][2]string{
				{"deployment", "hyperfleet-api"},
				{"service", "hyperfleet-api"},
				{"serviceaccount", "hyperfleet-api"},
				{"configmap", "hyperfleet-api-config"},
				{"role", "hyperfleet-api"},
				{"rolebinding", "hyperfleet-api"},
			}
			for _, operand := range operands {
				resourceType, resourceName := operand[0], operand[1]
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", resourceType, resourceName, "-n", namespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "expected %s/%s to be created", resourceType, resourceName)
				}).Should(Succeed())
			}
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput := getMetricsOutput()
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

const managerSubject = "system:serviceaccount:" + namespace + ":" + serviceAccountName

func managerCanI(verb, resource, resourceNamespace string) string {
	args := []string{"auth", "can-i", verb, resource, "--as", managerSubject}
	return managerCanIWithArgs(args, resourceNamespace)
}

func managerCanISubresource(verb, resource, subresource string) string {
	args := []string{"auth", "can-i", verb, resource, "--subresource", subresource, "--as", managerSubject}
	return managerCanIWithArgs(args, "")
}

func managerCanIWithArgs(args []string, resourceNamespace string) string {
	if resourceNamespace != "" {
		args = append(args, "-n", resourceNamespace)
	}
	output, err := utils.Run(exec.Command("kubectl", args...))
	Expect(err).NotTo(HaveOccurred(), "kubectl auth can-i failed: %v", args)
	return strings.TrimSpace(output)
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

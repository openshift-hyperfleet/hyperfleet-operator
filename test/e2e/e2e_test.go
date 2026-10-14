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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
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

const (
	apiOperandName      = "hyperfleet-api"
	postgresOperandName = "hyperfleet-postgres"
)

const (
	getVerb     = "get"
	asGroupFlag = "--as-group"
)

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "hyperfleet-operator-controller-manager-metrics-service"

// metricsPort is the plain-HTTP port the operator serves /metrics on, per the
// HyperFleet metrics standard.
const metricsPort = "9090"

const (
	metricsServerCertificateSecret = "metrics-server-cert"
	metricsReaderBinding           = "hyperfleet-metrics-e2e-reader"
	metricsReaderRole              = "hyperfleet-operator-metrics-reader"
	metricsCertificateMountPath    = "/tmp/k8s-metrics-server/metrics-certs"
	plainMetricsCurlPod            = "curl-metrics"
	secureMetricsCurlPod           = "curl-secure-metrics"
	metricsCurlImage               = "curlimages/curl:8.16.0"
	invalidBearerTokenLogPattern   = `(?m)^.*Authentication failed[^\n]*"error":\s*"invalid bearer token"`
	activeManagerPodsTemplate      = `go-template={{ range .items }}` +
		`{{ if not .metadata.deletionTimestamp }}` +
		`{{ .metadata.name }}{{ "\n" }}{{ end }}{{ end }}`
	secureMetricsCurlCommand = `set -eu
url=%q
token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
curl --fail --show-error --silent --connect-timeout 5 --max-time 20 \
  --retry 3 --retry-delay 2 --retry-all-errors --cacert /var/run/metrics/ca.crt \
  --header "Authorization: Bearer $token" "$url"
status=$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
  --retry 3 --retry-delay 2 --output /dev/null --write-out '%%{http_code}' \
  --cacert /var/run/metrics/ca.crt "$url")
if [ "$status" != "401" ]; then
  echo "expected unauthenticated metrics request to return HTTP 401, got $status" >&2
  exit 1
fi
echo "unauthenticated metrics request rejected (HTTP 401)"
# Tamper with the signed token to check that merely presenting a bearer token is insufficient.
invalid_response_file=$(mktemp)
trap 'rm -f "$invalid_response_file"' EXIT
invalid_status=$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
  --retry 3 --retry-delay 2 --output "$invalid_response_file" --write-out '%%{http_code}' \
  --cacert /var/run/metrics/ca.crt \
  --header "Authorization: Bearer ${token}invalid" "$url")
case "$invalid_status" in
  4[0-9][0-9]|5[0-9][0-9]) ;;
  *) echo "expected invalid bearer token to be rejected, got HTTP $invalid_status" >&2; exit 1 ;;
esac
# controller-runtime v0.25.2 returns 500 for the invalid-bearer-token error.
# Do not accept a status alone: check the auth response here and the precise
# authentication error in the fresh manager Pod's logs in the Go assertion.
case "$(cat "$invalid_response_file")" in
  "Authentication failed"|"Unauthorized") ;;
  *) echo "invalid-token response was not an authentication failure" >&2; exit 1 ;;
esac
echo "invalid bearer token rejected (HTTP $invalid_status)"
untrusted_exit=0
curl --silent --connect-timeout 5 --max-time 20 --retry 3 --retry-delay 2 \
  --output /dev/null \
  --cacert /var/run/metrics/untrusted-ca.crt \
  --header "Authorization: Bearer $token" "$url" || untrusted_exit=$?
if [ "$untrusted_exit" -ne 60 ]; then
  echo "expected untrusted metrics CA to fail TLS verification (curl exit 60), got $untrusted_exit" >&2
  exit 1
fi
echo "untrusted metrics CA rejected (curl exit 60)"
# Confirm that the endpoint still serves authenticated requests after the failures.
curl --fail --show-error --silent --connect-timeout 5 --max-time 20 \
  --retry 3 --retry-delay 2 --retry-all-errors --cacert /var/run/metrics/ca.crt \
  --header "Authorization: Bearer $token" "$url"`
)

const cleanupTimeout = 30 * time.Second

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
		cmd = exec.Command("make", "deploy", fmt.Sprintf("OPERATOR_IMG=%s", projectImage),
			fmt.Sprintf("RELATED_IMAGE_HYPERFLEET_OPERATOR=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		var cleanupErrors []error
		runCleanup := func(step, command string, args ...string) {
			By(step)
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			if _, err := utils.Run(exec.CommandContext(ctx, command, args...)); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("%s: %w", step, err))
			}
		}

		runCleanup("cleaning up the HyperFleetConfig singleton",
			"kubectl", "delete", "hyperfleetconfig", "cluster", "--ignore-not-found=true", "--wait=false")
		runCleanup("cleaning up the curl pod for metrics",
			"kubectl", "delete", "pod", plainMetricsCurlPod, "-n", namespace, "--ignore-not-found=true", "--wait=false")
		runCleanup("cleaning up the secure curl pod for metrics",
			"kubectl", "delete", "pod", secureMetricsCurlPod, "-n", namespace, "--ignore-not-found=true", "--wait=false")
		runCleanup("cleaning up the metrics server certificate",
			"kubectl", "delete", "secret", metricsServerCertificateSecret, "-n", namespace,
			"--ignore-not-found=true", "--wait=false")
		runCleanup("cleaning up the e2e metrics reader binding",
			"kubectl", "delete", "clusterrolebinding", metricsReaderBinding, "--ignore-not-found=true", "--wait=false")
		runCleanup("undeploying the controller-manager",
			"make", "undeploy", "ignore-not-found=true")
		runCleanup("uninstalling CRDs",
			"make", "uninstall", "ignore-not-found=true")
		runCleanup("removing manager namespace",
			"kubectl", "delete", "ns", namespace, "--ignore-not-found=true", "--wait=false")

		Expect(errors.Join(cleanupErrors...)).NotTo(HaveOccurred(), "e2e cleanup failed")
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

			for _, podName := range []string{plainMetricsCurlPod, secureMetricsCurlPod} {
				// A failing spec may not have created its curl pod yet. Only ask for
				// logs when it exists, so diagnostics retain the original failure.
				pod, getErr := utils.Run(exec.Command("kubectl", "get", "pod", podName,
					"-n", namespace, "--ignore-not-found=true", "-o", "name"))
				if getErr != nil || strings.TrimSpace(pod) == "" {
					continue
				}
				metricsOutput, logsErr := utils.Run(exec.Command("kubectl", "logs", podName, "-n", namespace))
				if logsErr == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "%s logs:\n%s", podName, metricsOutput)
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get %s logs: %s", podName, logsErr)
				}
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
					"-o", activeManagerPodsTemplate,
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
			cmd = exec.Command("kubectl", "run", plainMetricsCurlPod, "--restart=Never",
				"--namespace", namespace,
				"--image="+metricsCurlImage,
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": %q,
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
				}`, metricsCurlImage, metricsServiceName, namespace, metricsPort, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", plainMetricsCurlPod,
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

		It("should serve metrics over verified TLS with bearer authentication", func() {
			// The secure profile needs a stable serving certificate. Generate a
			// short-lived test CA and a service-DNS certificate here rather than
			// relying on cert-manager being installed in the e2e environment.
			dnsNames := []string{
				fmt.Sprintf("%s.%s.svc", metricsServiceName, namespace),
				fmt.Sprintf("%s.%s.svc.cluster.local", metricsServiceName, namespace),
			}
			caPEM, certPEM, keyPEM, err := generateMetricsServingCertificate(dnsNames, "hyperfleet-e2e-metrics-ca")
			Expect(err).NotTo(HaveOccurred())
			untrustedCAPEM, _, _, err := generateMetricsServingCertificate(dnsNames, "hyperfleet-e2e-untrusted-ca")
			Expect(err).NotTo(HaveOccurred())

			certDir := GinkgoT().TempDir()
			caPath := filepath.Join(certDir, "ca.crt")
			untrustedCAPath := filepath.Join(certDir, "untrusted-ca.crt")
			certPath := filepath.Join(certDir, "tls.crt")
			keyPath := filepath.Join(certDir, "tls.key")
			Expect(os.WriteFile(caPath, caPEM, 0o600)).To(Succeed())
			Expect(os.WriteFile(untrustedCAPath, untrustedCAPEM, 0o600)).To(Succeed())
			Expect(os.WriteFile(certPath, certPEM, 0o600)).To(Succeed())
			Expect(os.WriteFile(keyPath, keyPEM, 0o600)).To(Succeed())

			By("creating the metrics serving-certificate Secret")
			cmd := exec.Command("kubectl", "create", "secret", "generic", metricsServerCertificateSecret,
				"--namespace", namespace,
				"--from-file=ca.crt="+caPath,
				"--from-file=untrusted-ca.crt="+untrustedCAPath,
				"--from-file=tls.crt="+certPath,
				"--from-file=tls.key="+keyPath,
			)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("granting the scrape ServiceAccount access to the protected metrics URL")
			cmd = exec.Command("kubectl", "create", "clusterrolebinding", metricsReaderBinding,
				"--clusterrole="+metricsReaderRole,
				"--serviceaccount="+namespace+":"+serviceAccountName,
			)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("reconfiguring the manager with secure metrics and the mounted serving certificate")
			securePatch := fmt.Sprintf(`[
				{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--metrics-secure=true"},
				{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--metrics-service-monitor-enabled=false"},
				{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--metrics-cert-path=%s"},
				{"op":"add","path":"/spec/template/spec/containers/0/volumeMounts",
				 "value":[{"name":"metrics-certs","mountPath":"%s","readOnly":true}]},
				{"op":"add","path":"/spec/template/spec/volumes",
				 "value":[{"name":"metrics-certs","secret":{"secretName":"%s","optional":false,
				 "items":[{"key":"ca.crt","path":"ca.crt"},{"key":"tls.crt","path":"tls.crt"},
				 {"key":"tls.key","path":"tls.key"}]}}]}
			]`, metricsCertificateMountPath, metricsCertificateMountPath, metricsServerCertificateSecret)
			cmd = exec.Command("kubectl", "patch", "deployment", "hyperfleet-operator-controller-manager",
				"--namespace", namespace, "--type=json", "--patch", securePatch)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "rollout", "status", "deployment/hyperfleet-operator-controller-manager",
				"--namespace", namespace, "--timeout=2m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the replacement manager Pod")
			Eventually(func(g Gomega) {
				cmd = exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-o", activeManagerPodsTemplate, "--namespace", namespace)
				output, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				podNames := utils.GetNonEmptyLines(output)
				g.Expect(podNames).To(HaveLen(1), "expected exactly one non-terminating manager Pod")
				controllerPodName = podNames[0]
			}).Should(Succeed())

			By("verifying secure scrape, token rejection, and CA validation")
			secureMetricsURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:%s/metrics",
				metricsServiceName, namespace, metricsPort)
			cmd = exec.Command("kubectl", "run", secureMetricsCurlPod, "--restart=Never", "--namespace", namespace,
				"--image="+metricsCurlImage, "--overrides", fmt.Sprintf(`{
					"spec": {
						"serviceAccount": %q,
						"containers": [{
							"name": "curl",
							"image": %q,
							"command": ["/bin/sh", "-c"],
							"args": [%q],
							"volumeMounts": [{"name": "metrics-ca", "mountPath": "/var/run/metrics", "readOnly": true}],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {"drop": ["ALL"]},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {"type": "RuntimeDefault"}
							}
						}],
						"volumes": [{
							"name": "metrics-ca",
							"secret": {
								"secretName": %q,
								"items": [
									{"key": "ca.crt", "path": "ca.crt"},
									{"key": "untrusted-ca.crt", "path": "untrusted-ca.crt"}
								]
							}
						}]
					}
				}`, serviceAccountName, metricsCurlImage,
					fmt.Sprintf(secureMetricsCurlCommand, secureMetricsURL), metricsServerCertificateSecret))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd = exec.Command("kubectl", "get", "pod", secureMetricsCurlPod, "--namespace", namespace,
					"-o", "jsonpath={.status.phase}")
				output, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				if output == "Failed" {
					StopTrying("secure metrics curl Pod failed; inspect its logs for the scrape error").Now()
				}
				g.Expect(output).To(Equal("Succeeded"))
			}, 5*time.Minute).Should(Succeed())

			metricsOutput, err := utils.Run(exec.Command("kubectl", "logs", secureMetricsCurlPod, "--namespace", namespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(metricsOutput).To(ContainSubstring("hyperfleet_operator_up"))
			Expect(metricsOutput).To(ContainSubstring("unauthenticated metrics request rejected (HTTP 401)"))
			Expect(metricsOutput).To(ContainSubstring("invalid bearer token rejected (HTTP "))
			Expect(metricsOutput).To(ContainSubstring("untrusted metrics CA rejected (curl exit 60)"))

			// A generic HTTP 500 (including a TokenReview outage) is not proof
			// of token rejection. This isolated, freshly rolled-out manager must
			// report the specific invalid-token error from the authentication layer.
			managerOutput, err := utils.Run(exec.Command("kubectl", "logs", controllerPodName,
				"--namespace", namespace, "--container", "manager"))
			Expect(err).NotTo(HaveOccurred())
			Expect(managerOutput).To(MatchRegexp(invalidBearerTokenLogPattern),
				"expected the authentication layer to reject the tampered bearer token")

			// The remaining specs in this Ordered suite must see the original
			// manager Deployment. On a failure, AfterAll tears down the entire
			// test install instead of attempting to restore a deleted Deployment.
			By("restoring the manager Deployment after the secure scrape")
			_, err = utils.Run(exec.Command("kubectl", "rollout", "undo",
				"deployment/hyperfleet-operator-controller-manager", "--namespace", namespace))
			Expect(err).NotTo(HaveOccurred(), "Failed to restore the manager Deployment")
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status",
				"deployment/hyperfleet-operator-controller-manager", "--namespace", namespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred(), "Restored manager Deployment did not become ready")
			Eventually(func(g Gomega) {
				output, getErr := utils.Run(exec.Command("kubectl", "get", "pods",
					"-l", "control-plane=controller-manager", "-o", activeManagerPodsTemplate,
					"--namespace", namespace))
				g.Expect(getErr).NotTo(HaveOccurred())
				podNames := utils.GetNonEmptyLines(output)
				g.Expect(podNames).To(HaveLen(1), "expected exactly one restored manager Pod")
				controllerPodName = podNames[0]
			}).Should(Succeed())

			By("removing the secure scrape test resources")
			_, err = utils.Run(exec.Command("kubectl", "delete", "pod", secureMetricsCurlPod,
				"--namespace", namespace, "--ignore-not-found=true", "--wait=false"))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "delete", "secret", metricsServerCertificateSecret,
				"--namespace", namespace, "--ignore-not-found=true", "--wait=false"))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding", metricsReaderBinding,
				"--ignore-not-found=true", "--wait=false"))
			Expect(err).NotTo(HaveOccurred())
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
			operandVerbs := []string{getVerb, "list", "watch", "create", "update", "patch"}
			for _, resource := range operandResources {
				for _, verb := range operandVerbs {
					Expect(managerCanI(verb, resource, namespace)).To(BeTrue(),
						"manager should be allowed to %s %s in %s", verb, resource, namespace)
				}
			}

			By("verifying the manager cannot manage operands in another namespace")
			for _, resource := range operandResources {
				for _, verb := range operandVerbs {
					Expect(managerCanI(verb, resource, "default")).To(BeFalse(),
						"manager should not be allowed to %s %s in default", verb, resource)
				}
				Expect(managerCanI("delete", resource, namespace)).To(BeFalse(),
					"manager should not be allowed to delete %s; owner-reference GC handles cleanup", resource)
			}

			By("verifying cluster-scoped HyperFleetConfig access remains available")
			for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
				Expect(managerCanI(verb, "hyperfleetconfigs.hyperfleet.redhat.com", "")).To(BeTrue(),
					"manager should retain cluster-scoped HyperFleetConfig %s access", verb)
			}
			Expect(managerCanISubresource("update", "hyperfleetconfigs", "status")).To(BeTrue())
			Expect(managerCanISubresource("update", "hyperfleetconfigs", "finalizers")).To(BeTrue())
		})

		It("should migrate a fresh database before the API becomes ready", func() {
			By("deploying a disposable PostgreSQL database in the restricted namespace")
			cmd := exec.Command("kubectl", "apply", "-f", "test/e2e/postgres.yaml", "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for PostgreSQL to accept the migration connection")
			cmd = exec.Command("kubectl", "rollout", "status", "deployment", postgresOperandName,
				"-n", namespace, "--timeout=2m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating the referenced database Secret")
			cmd = exec.Command("kubectl", "create", "secret", "generic", "hyperfleet-db",
				"-n", namespace,
				"--from-literal=db.host="+postgresOperandName,
				"--from-literal=db.port=5432",
				"--from-literal=db.name=hyperfleet",
				"--from-literal=db.user=hyperfleet",
				"--from-literal=db.password=password",
			)
			_, err = utils.Run(cmd)
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
				{"deployment", apiOperandName},
				{"service", apiOperandName},
				{"serviceaccount", apiOperandName},
				{"configmap", "hyperfleet-api-config"},
				{"role", apiOperandName},
				{"rolebinding", apiOperandName},
			}
			for _, operand := range operands {
				resourceType, resourceName := operand[0], operand[1]
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", resourceType, resourceName, "-n", namespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "expected %s/%s to be created", resourceType, resourceName)
				}).Should(Succeed())
			}

			By("waiting for the API Deployment to complete its first migration and rollout")
			cmd = exec.Command("kubectl", "rollout", "status", "deployment", apiOperandName,
				"-n", namespace, "--timeout=2m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			apiPodName := waitForMigratedAPIPod("")

			By("restarting the API Pod so its migration runs again")
			cmd = exec.Command("kubectl", "delete", "pod", apiPodName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			waitForMigratedAPIPod(apiPodName)

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

func generateMetricsServingCertificate(
	dnsNames []string, caCommonName string,
) (caPEM, certPEM, keyPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate server key: %w", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create server certificate: %w", err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal server key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}), nil
}

const (
	managerSubject                 = "system:serviceaccount:" + namespace + ":" + serviceAccountName
	serviceAccountsGroup           = "system:serviceaccounts"
	namespacedServiceAccountsGroup = serviceAccountsGroup + ":" + namespace
	authenticatedGroup             = "system:authenticated"
)

// managerCanI reports whether the controller manager is authorized for a resource action.
func managerCanI(verb, resource, resourceNamespace string) bool {
	args := managerCanIArgs(verb, resource)
	return managerCanIWithArgs(args, resourceNamespace)
}

// managerCanISubresource reports whether the controller manager is authorized for a subresource action.
func managerCanISubresource(verb, resource, subresource string) bool {
	args := managerCanIArgs(verb, resource)
	args = append(args, "--subresource", subresource)
	return managerCanIWithArgs(args, "")
}

// managerCanIArgs builds authorization-check arguments for the manager's complete ServiceAccount identity.
func managerCanIArgs(verb, resource string) []string {
	return []string{
		"auth", "can-i", verb, resource,
		"--as", managerSubject,
		asGroupFlag, serviceAccountsGroup,
		asGroupFlag, namespacedServiceAccountsGroup,
		asGroupFlag, authenticatedGroup,
	}
}

// managerCanIWithArgs executes an authorization check and handles kubectl's denial exit status.
func managerCanIWithArgs(args []string, resourceNamespace string) bool {
	if resourceNamespace != "" {
		args = append(args, "-n", resourceNamespace)
	}
	// kubectl writes scope warnings to stderr while its authorization answer is
	// on stdout. CombinedOutput would turn a valid "yes" into unrecognized text.
	outputBytes, err := exec.Command("kubectl", args...).Output()
	output := string(outputBytes)
	allowed, recognized := parseCanIOutput(output)
	if recognized {
		if !allowed {
			// kubectl auth can-i exits with status 1 when authorization is denied.
			return false
		}
		Expect(err).NotTo(HaveOccurred(), "kubectl auth can-i failed: %v", args)
		return true
	}

	Expect(err).NotTo(HaveOccurred(), "kubectl auth can-i failed: %v", args)
	Fail(fmt.Sprintf("kubectl auth can-i returned unexpected output for %v: %q", args, output))
	return false
}

// parseCanIOutput recognizes the allowed and denied output forms emitted by kubectl auth can-i.
func parseCanIOutput(output string) (allowed, recognized bool) {
	switch result := strings.TrimSpace(output); {
	case result == "yes":
		return true, true
	case result == "no" || strings.HasPrefix(result, "no - "):
		return false, true
	default:
		return false, false
	}
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

// waitForMigratedAPIPod waits for a replacement API Pod, verifies that the
// migration init container exited successfully, and returns its name. A
// previous name forces the caller to observe a real replacement after restart.
func waitForMigratedAPIPod(previousName string) string {
	var podName string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods",
			"-l", "app.kubernetes.io/name="+apiOperandName,
			"-o", "jsonpath={.items[0].metadata.name}", "-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		podName = strings.TrimSpace(output)
		g.Expect(podName).NotTo(BeEmpty())
		if previousName != "" {
			g.Expect(podName).NotTo(Equal(previousName))
		}

		cmd = exec.Command("kubectl", "get", "pod", podName,
			"-o", `jsonpath={.status.initContainerStatuses[?(@.name=="db-migrate")].state.terminated.exitCode}`,
			"-n", namespace)
		output, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(output)).To(Equal("0"))

		cmd = exec.Command("kubectl", "get", "pod", podName,
			"-o", `jsonpath={.status.containerStatuses[?(@.name=="hyperfleet-api")].ready}`,
			"-n", namespace)
		output, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(output)).To(Equal("true"))
	}).Should(Succeed())

	return podName
}

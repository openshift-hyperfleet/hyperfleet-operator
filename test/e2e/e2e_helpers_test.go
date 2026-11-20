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
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"testing"
)

func TestInvalidBearerTokenLogPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		logs string
		want bool
	}{
		{
			name: "invalid token",
			logs: "ERROR\tcontroller-runtime.metrics\tAuthentication failed\t" +
				`{"error": "invalid bearer token"}`,
			want: true,
		},
		{
			name: "JSON logging",
			logs: `{"msg":"Authentication failed","error":"invalid bearer token"}`,
			want: true,
		},
		{
			name: "authentication backend failure",
			logs: `Authentication failed {"error": "TokenReview request timed out"}`,
		},
		{
			name: "unrelated server error",
			logs: `request failed {"error": "Internal Server Error"}`,
		},
		{
			name: "unrelated lines must not combine",
			logs: "Authentication failed\n" + `another message {"error": "invalid bearer token"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := regexp.MustCompile(invalidBearerTokenLogPattern).MatchString(tc.logs); got != tc.want {
				t.Errorf("invalid token log match = %t, want %t", got, tc.want)
			}
		})
	}
}

const (
	containerToolDocker = "docker"
	containerToolPodman = "podman"
)

func TestManagerImageBuildArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		arch          string
		containerTool string
		wantSuffix    string
	}{
		{arch: "arm64", containerTool: containerToolDocker, wantSuffix: containerToolDocker},
		{arch: "arm64", containerTool: "", wantSuffix: containerToolDocker},
		{arch: "amd64", containerTool: containerToolPodman, wantSuffix: containerToolPodman},
		{arch: "amd64", containerTool: containerToolDocker, wantSuffix: containerToolDocker},
	} {
		t.Run(tc.arch+tc.containerTool, func(t *testing.T) {
			want := []string{
				"image",
				fmt.Sprintf("OPERATOR_IMG=%s", kindOperatorImg),
				fmt.Sprintf("PLATFORM=linux/%s", tc.arch),
				fmt.Sprintf("CONTAINER_TOOL=%s", tc.wantSuffix),
			}
			if got := managerImageBuildArgs(kindOperatorImg, tc.arch, tc.containerTool); !reflect.DeepEqual(got, want) {
				t.Errorf("managerImageBuildArgs(%q) = %v, want %v", tc.wantSuffix, got, want)
			}
		})
	}
}

// Exercise the actual Pod shell command with controlled curl responses. No
// network requests or real ServiceAccount credentials are used by this test.
func TestSecureMetricsCurlCommand(t *testing.T) {
	t.Parallel()
	const fakeCurl = `
cat() {
  if [ "$1" = /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
    printf 'test-token\n'
  else
    command cat "$@"
  fi
}
curl() {
  auth= ca= output=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --header) auth="$2"; shift ;;
      --cacert) ca="$2"; shift ;;
      --output) output="$2"; shift ;;
    esac
    shift
  done
  if [ "$ca" = /var/run/metrics/untrusted-ca.crt ]; then
    return 60
  fi
  case "$auth" in
    "Authorization: Bearer test-tokeninvalid")
      printf '%s\n' "$INVALID_BODY" > "$output"
      printf '%s' "$INVALID_STATUS"
      return "$INVALID_EXIT"
      ;;
    "Authorization: Bearer test-token") printf 'hyperfleet_operator_up 1\n' ;;
    "") printf '401' ;;
    *) return 1 ;;
  esac
}
`
	for _, tc := range []struct {
		name   string
		status string
		body   string
		exit   string
		wantOK bool
	}{
		{name: "current auth failure", status: "500", body: "Authentication failed", exit: "0", wantOK: true},
		{name: "unauthorized", status: "401", body: "Unauthorized", exit: "0", wantOK: true},
		{name: "generic server error", status: "500", body: "Internal Server Error", exit: "0"},
		{name: "service unavailable", status: "503", body: "Service Unavailable", exit: "0"},
		{name: "token accepted", status: "200", body: "hyperfleet_operator_up 1", exit: "0"},
		{name: "redirect", status: "302", body: "Authentication failed", exit: "0"},
		{name: "transport failure", status: "000", exit: "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", fakeCurl+fmt.Sprintf(secureMetricsCurlCommand, "https://metrics.test/metrics"))
			cmd.Env = append(os.Environ(), "INVALID_STATUS="+tc.status, "INVALID_BODY="+tc.body, "INVALID_EXIT="+tc.exit)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("secure metrics probe error = %v, want success=%t\n%s", err, tc.wantOK, output)
			}
		})
	}
}

func TestParseCanIOutput(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		output     string
		allowed    bool
		recognized bool
	}{
		{name: "allowed", output: "yes\n", allowed: true, recognized: true},
		{name: "denied", output: "no\n", allowed: false, recognized: true},
		{name: "denied with reason", output: "no - RBAC: access denied\n", allowed: false, recognized: true},
		{name: "command failure", output: "Error from server (Forbidden)", allowed: false, recognized: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, recognized := parseCanIOutput(tc.output)
			if allowed != tc.allowed || recognized != tc.recognized {
				t.Errorf("parseCanIOutput(%q) = (%t, %t), want (%t, %t)", tc.output, allowed, recognized, tc.allowed, tc.recognized)
			}
		})
	}
}

func TestManagerCanIArgs(t *testing.T) {
	t.Parallel()

	// managerCanIArgs reads the package-level namespace var at call time;
	// pin it to a known value so the test is independent of --namespace.
	origNamespace := namespace
	namespace = "test-ns"
	t.Cleanup(func() { namespace = origNamespace })

	want := []string{
		"auth", "can-i", getVerb, "deployments.apps",
		"--as", "system:serviceaccount:test-ns:hyperfleet-operator-controller-manager",
		asGroupFlag, "system:serviceaccounts",
		asGroupFlag, "system:serviceaccounts:test-ns",
		asGroupFlag, "system:authenticated",
	}
	if got := managerCanIArgs(getVerb, "deployments.apps"); !reflect.DeepEqual(got, want) {
		t.Errorf("managerCanIArgs() = %v, want %v", got, want)
	}
}

func TestSentinelOperandNames(t *testing.T) {
	t.Parallel()

	names := sentinelOperandNames("nodepools")
	nodepoolName := "hyperfleet-sentinel-nodepools"
	want := sentinelOperands{
		Deployment:      nodepoolName,
		ServiceAccount:  nodepoolName,
		ConfigMap:       nodepoolName + "-config",
		BrokerConfigMap: nodepoolName + "-broker-config",
		NetworkPolicy:   nodepoolName,
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("sentinelOperandNames(nodepools) = %+v, want %+v", names, want)
	}
}

func TestSentinelPodLabel(t *testing.T) {
	t.Parallel()

	// The label value is the component name, not the resource name.
	if got := sentinelPodLabel("wifconfigs"); got != "app.kubernetes.io/name=sentinel-wifconfigs" {
		t.Errorf("sentinelPodLabel(wifconfigs) = %q", got)
	}
}

func TestParseSentinelConfig(t *testing.T) {
	t.Parallel()

	const config = `# Sentinel configuration — rendered by hyperfleet-operator (HYPERFLEET-1673).
# Managed resource: manual edits are overwritten on the next reconcile.
sentinel:
  name: hyperfleet-sentinel-clusters
clients:
  hyperfleet_api:
    base_url: http://hyperfleet-api.hyperfleet-system.svc:8000
    version: v1
    timeout: 10s
    auth:
      token_path: /var/run/secrets/hyperfleet/api-token/token
      scheme: Bearer
  broker:
    topic: hyperfleet-system-clusters
resource_type: clusters
`
	cfg, err := parseSentinelConfig(config)
	if err != nil {
		t.Fatalf("parseSentinelConfig() error = %v", err)
	}
	if cfg.ResourceType != "clusters" {
		t.Errorf("ResourceType = %q, want clusters", cfg.ResourceType)
	}
	if cfg.Clients.HyperFleetAPI.BaseURL != "http://hyperfleet-api.hyperfleet-system.svc:8000" {
		t.Errorf("BaseURL = %q", cfg.Clients.HyperFleetAPI.BaseURL)
	}
	if cfg.Clients.Broker.Topic != "hyperfleet-system-clusters" {
		t.Errorf("Topic = %q", cfg.Clients.Broker.Topic)
	}
	if cfg.Clients.HyperFleetAPI.Auth == nil {
		t.Fatal("Auth block not parsed")
	}
	if cfg.Clients.HyperFleetAPI.Auth.TokenPath != "/var/run/secrets/hyperfleet/api-token/token" {
		t.Errorf("TokenPath = %q", cfg.Clients.HyperFleetAPI.Auth.TokenPath)
	}
	if cfg.Clients.HyperFleetAPI.Auth.Scheme != "Bearer" {
		t.Errorf("Scheme = %q", cfg.Clients.HyperFleetAPI.Auth.Scheme)
	}

	// Without the auth block, Auth must parse as nil so the e2e absence
	// assertion is meaningful.
	const noAuth = `clients:
  hyperfleet_api:
    base_url: http://hyperfleet-api.hyperfleet-system.svc:8000
resource_type: versions
`
	cfg, err = parseSentinelConfig(noAuth)
	if err != nil {
		t.Fatalf("parseSentinelConfig(noAuth) error = %v", err)
	}
	if cfg.Clients.HyperFleetAPI.Auth != nil {
		t.Errorf("Auth = %+v, want nil", cfg.Clients.HyperFleetAPI.Auth)
	}
}

func TestParseSentinelBrokerConfig(t *testing.T) {
	t.Parallel()

	const config = `# Comments have been added to broker.yaml
# ensure that yaml can still be parsed
broker:
  type: rabbitmq
  rabbitmq:
    url: ""
    exchange_type: topic
`
	cfg, err := parseSentinelBrokerConfig(config)
	if err != nil {
		t.Fatalf("parseSentinelBrokerConfig() error = %v", err)
	}
	if cfg.Broker.Type != "rabbitmq" {
		t.Errorf("Type = %q, want rabbitmq", cfg.Broker.Type)
	}
	if cfg.Broker.RabbitMQ == nil {
		t.Fatal("RabbitMQ block not parsed")
	}
	if cfg.Broker.RabbitMQ.URL == nil {
		t.Fatal("URL key not rendered; want the empty key so BROKER_RABBITMQ_URL can supply it")
	}
	if *cfg.Broker.RabbitMQ.URL != "" {
		t.Errorf("URL = %q, want empty so broker credentials stay in the Secret", *cfg.Broker.RabbitMQ.URL)
	}
	if cfg.Broker.RabbitMQ.ExchangeType != "topic" {
		t.Errorf("ExchangeType = %q, want topic", cfg.Broker.RabbitMQ.ExchangeType)
	}
}

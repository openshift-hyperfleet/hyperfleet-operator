// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/blang/semver/v4"
	operatorversion "github.com/operator-framework/api/pkg/lib/version"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	testAPIConfigName    = "api-config"
	testConfigKey        = "config.yaml"
	testDatabaseRole     = "database"
	testConfigA          = "config-a"
	testAlpha            = "alpha"
	testBroker           = "broker"
	testBrokerSecretName = "broker-secret"
	testFinalHash        = "final-hash"
)

func testImage(name, digit string) string {
	return "quay.io/test/" + name + "@sha256:" + strings.Repeat(digit, 64)
}

func testRelease(t *testing.T, v, apiDigit string) release {
	t.Helper()
	r := release{Bundle: testImage("bundle", apiDigit), Images: map[string]string{}}
	r.CSV.Kind = csvKind
	r.CSV.Name = packageName + ".v" + v
	r.CSV.Spec.Version = operatorversion.OperatorVersion{Version: semver.MustParse(v)}
	r.CSV.Spec.RelatedImages = []operatorsv1alpha1.RelatedImage{
		{Name: testAPIImageName, Image: testImage(apiComponentName, apiDigit)},
		{Name: "hyperfleet-operator", Image: testImage("operator", "f")}}
	r.CSV.Spec.InstallStrategy.StrategySpec.DeploymentSpecs = []operatorsv1alpha1.StrategyDeploymentSpec{
		{Name: "operator", Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager",
				Image: testImage("operator", "f"), Env: []corev1.EnvVar{{Name: "RELATED_IMAGE_HYPERFLEET_API",
					Value: testImage(apiComponentName, apiDigit)}}}}}}}},
	}
	r.CSV.Spec.InstallModes = []operatorsv1alpha1.InstallMode{
		{Type: operatorsv1alpha1.InstallModeTypeAllNamespaces, Supported: true},
	}
	r.CSV.Spec.CustomResourceDefinitions.Owned = []operatorsv1alpha1.CRDDescription{{Name: hfcCRD}}
	if err := r.validate(); err != nil {
		t.Fatal(err)
	}
	return r
}

func testPair(t *testing.T) releasePair {
	t.Helper()
	return releasePair{From: testRelease(t, testPreviousVersion, "a"), To: testRelease(t, testTargetVersion, "b")}
}

func testExpectations() ReleaseExpectations {
	return ReleaseExpectations{Components: []Component{{Name: apiComponentName,
		Deployment: testAPIImageName, Container: apiComponentName,
		RelatedImage: testAPIImageName, MigrationContainer: testMigrationContainer, HashChange: ptr.To(false)}}}
}

func testScenario() Scenario {
	to := testExpectations()
	to.HashContract = &HashContract{Bundle: testImage("bundle", "b"), Scheme: hashSchemeV1}
	to.Components[0].HashInputs = &HashInputs{ConfigMaps: []HashConfigMap{{Name: testAPIConfigName, Key: testConfigKey}},
		Secrets: []HashSecret{{Role: testDatabaseRole, Name: testDatabaseSecret}}}
	return Scenario{From: testExpectations(), To: to}
}

func bundleJSON(t *testing.T, r release) []byte {
	t.Helper()
	csv, err := json.Marshal(r.CSV)
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(map[string][]byte{"data": csv})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(catalogObject{Schema: bundleSchema, Package: packageName,
		Name: r.CSV.Name, Image: r.Bundle,
		Properties: []property{{Type: bundleObjectProperty, Value: value}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func catalogJSON(t *testing.T, pair releasePair, entries []channelEntry) []byte {
	t.Helper()
	var result []byte
	objects := []catalogObject{
		{Schema: bundleSchema, Package: packageName, Name: pair.From.CSV.Name, Image: pair.From.Bundle},
		{Schema: bundleSchema, Package: packageName, Name: pair.To.CSV.Name, Image: pair.To.Bundle},
		{Schema: "olm.channel", Package: packageName, Name: testChannel, Entries: entries},
	}
	for _, object := range objects {
		data, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, data...)
		result = append(result, '\n')
	}
	return result
}

func testEntries(pair releasePair) []channelEntry {
	return []channelEntry{{Name: pair.From.CSV.Name}, {Name: pair.To.CSV.Name,
		Replaces: pair.From.CSV.Name}}
}

func TestBundleMetadataValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*release)
		invalid bool
	}{
		{"valid", func(*release) {}, false},
		{"mutable operand", func(r *release) { r.CSV.Spec.RelatedImages[0].Image = "quay.io/test/api:latest" }, true},
		{"env disagreement", func(r *release) {
			container := &r.CSV.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0].Spec.Template.Spec.Containers[0]
			container.Env[0].Value = testImage(apiComponentName, "c")
		}, true},
		{"unlisted operator", func(r *release) { r.CSV.Spec.RelatedImages = r.CSV.Spec.RelatedImages[:1] }, true},
		{"no all namespaces", func(r *release) { r.CSV.Spec.InstallModes[0].Supported = false }, true},
		{"foreign crd", func(r *release) { r.CSV.Spec.CustomResourceDefinitions.Owned[0].Name = "other.example.org" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRelease(t, testPreviousVersion, "a")
			tc.change(&r)
			_, err := parseBundle(bundleJSON(t, r), r.Bundle)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v; invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestCatalogRequiresExactSequentialPair(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func([]channelEntry) []channelEntry
		invalid bool
	}{
		{"direct", func(e []channelEntry) []channelEntry { return e }, false},
		{"skipped version", func(e []channelEntry) []channelEntry { e[1].Replaces = "other.v1.0.0"; return e }, true},
		{"missing baseline", func(e []channelEntry) []channelEntry { return e[1:] }, true},
		{"not head", func(e []channelEntry) []channelEntry {
			return append(e, channelEntry{Name: "later", Replaces: e[1].Name})
		}, true},
		{"skip range", func(e []channelEntry) []channelEntry { e[1].SkipRange = ">=1.0.0"; return e }, true},
		{"skips baseline", func(e []channelEntry) []channelEntry { e[1].Skips = []string{e[0].Name}; return e }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := testPair(t)
			err := validateCatalog(catalogJSON(t, pair, tc.change(testEntries(pair))), testChannel, pair)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v; invalid=%v", err, tc.invalid)
			}
		})
	}
	pair := testPair(t)
	data := catalogJSON(t, pair, testEntries(pair))
	pair.To.Bundle = testImage("wrong", "d")
	if validateCatalog(data, testChannel, pair) == nil {
		t.Fatal("accepted a different bundle digest")
	}
}

func TestPairRejectsUnsupportedUpgradeCoverage(t *testing.T) {
	scenario := testScenario()
	for _, tc := range []struct {
		name, from, to string
		sameAPI        bool
		invalid        bool
	}{
		{"valid", testPreviousVersion, testTargetVersion, false, false},
		{"previous RC", "1.0.0-rc1", testTargetVersion, false, true},
		{"downgrade", "1.0.2", testTargetVersion, false, true},
		{"cross major", testPreviousVersion, "2.0.0-rc1", false, true},
		{"operator only", testPreviousVersion, testTargetVersion, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := releasePair{From: testRelease(t, tc.from, "a"), To: testRelease(t, tc.to, "b")}
			if tc.sameAPI {
				pair.To.Images[testAPIImageName] = pair.From.Images[testAPIImageName]
			}
			err := validatePair(pair, scenario)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v; invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestPairRejectsHashContractForAnotherBundle(t *testing.T) {
	pair := testPair(t)
	scenario := testScenario()
	scenario.To.HashContract.Bundle = testImage("bundle", "c")
	if err := validatePair(pair, scenario); err == nil || !strings.Contains(err.Error(), "exact target bundle") {
		t.Fatal("target hash contract was not bound to the selected bundle", err)
	}
}

func TestPairRejectsUnsupportedOrIncompleteHashContract(t *testing.T) {
	pair := testPair(t)
	for _, tc := range []struct {
		name   string
		mutate func(*Scenario)
	}{
		{"unknown scheme", func(s *Scenario) { s.To.HashContract.Scheme = "unknown" }},
		{"missing inputs", func(s *Scenario) { s.To.Components[0].HashInputs = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := testScenario()
			tc.mutate(&scenario)
			if err := validatePair(pair, scenario); err == nil {
				t.Fatal("preflight accepted an unsupported target hash contract")
			}
		})
	}
}

func TestPreflightRequiresOPMValidation(t *testing.T) {
	pair := testPair(t)
	c := Config{OPM: "opm", Catalog: testImage("catalog", "c"), FromBundle: pair.From.Bundle,
		ToBundle: pair.To.Bundle,
		Channel:  testChannel, Scenario: testScenario()}
	validated := false
	_, err := preflight(t.Context(), c, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "validate" {
			validated = true
			return nil, errors.New("catalog graph invalid")
		}
		switch args[1] {
		case pair.From.Bundle:
			return bundleJSON(t, pair.From), nil
		case pair.To.Bundle:
			return bundleJSON(t, pair.To), nil
		default:
			return catalogJSON(t, pair, testEntries(pair)), nil
		}
	})
	if !validated || err == nil {
		t.Fatal("failed catalog validation must fail preflight")
	}
}

func TestRegistryMoveDoesNotCountAsAPIUpgrade(t *testing.T) {
	pair := testPair(t)
	pair.To.Images[testAPIImageName] = testImage("api-mirror", "a")
	if err := validatePair(pair, testScenario()); err == nil {
		t.Fatal("same API digest under another repository must not count as an image-changing upgrade")
	}
}

func TestOperatorEnvironmentMustMatchPublishedBundle(t *testing.T) {
	r := testRelease(t, testTargetVersion, "b")
	want := r.CSV.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0].Spec.Template.Spec
	actual := want.DeepCopy()
	if err := matchRelatedEnvironment(*actual, want); err != nil {
		t.Fatal(err)
	}
	actual.Containers[0].Env = nil
	if matchRelatedEnvironment(*actual, want) == nil {
		t.Fatal("missing operator RELATED_IMAGE wiring accepted")
	}
}

func readyDeployment() appsv1.Deployment {
	return appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: apiComponentName, Generation: 2},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}, Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
}

const (
	testAPIImageName       = "hyperfleet-api"
	testPreviousVersion    = "1.0.0"
	testOtherIdentity      = "other"
	testTargetCSV          = "target.v1.0.1-rc1"
	testMigrationContainer = "db-migrate"
)

const (
	testChannel       = "stable-v1"
	testTargetVersion = "1.0.1-rc1"
)

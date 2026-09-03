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

package api

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

// DefaultImage is the compiled-in fallback image used when the operator is not
// given RELATED_IMAGE_HYPERFLEET_API. It is digest-pinned so plain-manifest and
// local deployments use the same immutable image contract as OLM installs.
//
// Must be v0.3.0 or later: config.yaml renders entities (pkg/registry) and the
// multi-issuer server.jwt.configs list, neither of which exist in v0.2.x's
// config schema — the API's loader uses viper's UnmarshalExact, so a v0.2.x
// binary rejects this config and crash-loops at startup (verified field-for-field
// against hyperfleet-api's pkg/config/server.go and pkg/registry at each tag;
// see PR #6 review discussion).
//
// Pinned to 0.4.0 specifically because it also carries
// HYPERFLEET-1603 (hyperfleet-api#364, merged to main 2026-09-01): database
// credentials via HYPERFLEET_DATABASE_*_FILE (ResolveFileOverrides in
// pkg/config/db.go) — verified field-for-field present at the v0.4.0 git tag.
//
// Registry is redhat-services-prod, not openshift-hyperfleet: hyperfleet-api's
// Konflux pipelines (.tekton/hyperfleet-api-{push,tag}.yaml) publish only to
// the redhat-user-workloads staging tenant; a separate Konflux Release step
// promotes to redhat-services-prod, which is what actually carries a signed,
// pullable 0.4.0 tag (verified against quay.io's tag API — the tag has no "v"
// prefix there, unlike the hyperfleet-api git tag). openshift-hyperfleet/hyperfleet-api
// is a legacy pre-Konflux registry that stopped receiving pushes after v0.2.1
// and never got a v0.4.0 image at all (PR #6 review comment r3905519856).
const DefaultImage = "quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-api@sha256:8533d0d875480f31f5112e454659a095a5d2e993c139a9045a06be6b67b829ca"

// Component renders the HyperFleet API operand. It satisfies the bundle.Component
// contract structurally (no import of internal/bundle, avoiding an import cycle:
// bundle imports this package). It carries only rendering inputs and never
// touches the cluster; the controller applies what Render produces.
type Component struct {
	// Image is the API container image. Empty falls back to DefaultImage.
	Image string
	// Namespace is the operator's own namespace, where all operands live.
	Namespace string
	// Entities is the bundle-specific entity registration list rendered into
	// config.yaml. It is supplied by internal/bundle, which owns the
	// per-bundle entity sets; an empty list renders no `entities:` key.
	Entities []EntityDescriptor
	// ResolvedJWKSURL is the JWKS URL derived by the controller via OIDC
	// discovery, used only when auth is enabled and the CR pins neither a JWKS
	// URL nor a JWKS Secret. Discovery is a network read, so it cannot happen in
	// the pure renderer; the controller resolves it and injects the result here.
	ResolvedJWKSURL string
}

// Options carries the non-image, non-namespace inputs to New. It is a struct so
// callers name each field (both are easy to swap as bare positional arguments)
// and so future inputs can be added without changing the signature.
type Options struct {
	// Entities is the bundle-specific entity registration list.
	Entities []EntityDescriptor
	// ResolvedJWKSURL is the OIDC-discovered JWKS URL; see Component.
	ResolvedJWKSURL string
}

// New constructs the API component for the given image and operator namespace.
func New(image, namespace string, opts Options) *Component {
	return &Component{
		Image:           image,
		Namespace:       namespace,
		Entities:        opts.Entities,
		ResolvedJWKSURL: opts.ResolvedJWKSURL,
	}
}

// Name identifies the component in logs and the app.kubernetes.io/component label.
func (c *Component) Name() string {
	return ComponentName
}

// Render returns the full desired-state operand set for the API, in a
// dependency-friendly order (identity and RBAC before the workload that uses
// them). It is a pure function of its inputs: no cluster reads or writes.
//
// The config.yaml content, database env, and conditional TLS/JWKS mounts are
// derived from the CR spec here (HYPERFLEET-1408). The content-hash rollout
// annotation is NOT set here — it depends on referenced Secret *data*, which
// only the controller can read; the controller stamps it after Render. The ctx
// is part of the contract (later components may need it) but is unused here.
func (c *Component) Render(_ context.Context, cr *hyperfleetv1alpha1.HyperFleetConfig) ([]client.Object, error) {
	image := c.Image
	if image == "" {
		image = DefaultImage
	}

	jwkURL, jwkFile := c.resolveJWKSource(cr)
	configYAML, err := renderConfig(configInput{
		AuthEnabled: AuthEnabled(cr),
		Issuer:      cr.Spec.API.Auth.Issuer,
		Audience:    cr.Spec.API.Auth.Audience,
		JWKCertURL:  jwkURL,
		JWKCertFile: jwkFile,
		TLSEnabled:  cr.Spec.API.TLS != nil,
		Entities:    c.Entities,
	})
	if err != nil {
		return nil, fmt.Errorf("render api config: %w", err)
	}

	return []client.Object{
		serviceAccount(cr, c.Namespace),
		role(cr, c.Namespace),
		roleBinding(cr, c.Namespace),
		configMap(cr, c.Namespace, configYAML),
		service(cr, c.Namespace),
		deployment(cr, image, c.Namespace),
	}, nil
}

// AuthEnabled reports whether JWT auth is on. The CRD defaults enabled to true,
// so a nil pointer (field omitted) means enabled — matching the schema default
// rather than Go's zero value. Exported so the controller applies the identical
// default when deciding whether to perform OIDC discovery.
func AuthEnabled(cr *hyperfleetv1alpha1.HyperFleetConfig) bool {
	e := cr.Spec.API.Auth.Enabled
	return e == nil || *e
}

// resolveJWKSource picks the single JWKS source for the config file: a mounted
// CR Secret (a file path) when jwkCertSecretRef is set, else the
// controller-supplied OIDC-discovered URL. It returns (url, file) with at most
// one non-empty, and ("", "") when auth is disabled. When auth is on and
// neither resolves (discovery not yet run), both are empty and renderConfig
// reports the wiring error.
func (c *Component) resolveJWKSource(cr *hyperfleetv1alpha1.HyperFleetConfig) (url, file string) {
	if !AuthEnabled(cr) {
		return "", ""
	}
	if cr.Spec.API.Auth.JWKCertSecretRef != nil {
		return "", jwksFilePath
	}
	return c.ResolvedJWKSURL, ""
}

// Conditions reports the component's health as metav1.Conditions. The contract is
// defined now (HYPERFLEET-1407) so it need not be reopened next story, but its
// output is not yet rolled up into status.conditions — real health derivation and
// status wiring land in HYPERFLEET-1409. Returning nil until then keeps the
// reconciler from writing status prematurely.
func (c *Component) Conditions(_ context.Context, _ *hyperfleetv1alpha1.HyperFleetConfig) ([]metav1.Condition, error) {
	return nil, nil
}

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

// Package bundle defines the component contract and resolves a bundle to its
// ordered component set. It is the in-operator "bundle definition": adding a
// component later is one new entry here plus its own package — never a new
// controller.
package bundle

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
)

// Component is the contract every component satisfies. It is deliberately tiny —
// render the desired objects, report health — with no extension points until a
// second component exists to justify them.
//
//   - Render is a pure function (CR → desired objects); it must not read or write
//     the cluster. The controller applies what it returns.
//   - Conditions is also cluster-access-free: it derives health from applied, the
//     objects Render produced, already updated in place by apply.Objects with the
//     server's current status (see internal/apply). This avoids giving every
//     component its own client.Client and an extra round of API reads for state
//     the controller already has. Consumed starting in HYPERFLEET-1409.
type Component interface {
	Name() string
	Render(ctx context.Context, cr *hyperfleetv1alpha1.HyperFleetConfig) ([]client.Object, error)
	Conditions(ctx context.Context, cr *hyperfleetv1alpha1.HyperFleetConfig, applied []client.Object) ([]metav1.Condition, error)
}

// Config carries the inputs the resolver needs to construct components.
type Config struct {
	// APIImage is the image for the API component (empty → its compiled-in default).
	APIImage string
	// Namespace is the operator's own namespace, where operands are created.
	Namespace string
	// ResolvedJWKSURL is the JWKS URL the controller derived via OIDC discovery.
	// It is threaded through to the API component, which needs it only when auth
	// is enabled and the CR pins neither a JWKS URL nor a JWKS Secret. Empty
	// otherwise (the component reads the CR field/Secret path directly).
	ResolvedJWKSURL string
}

// cloudCAPIEntities is the entity registration set for the cloud-capi bundle.
// It is lifted verbatim (verified field-for-field) from hyperfleet-api's
// configs/config.yaml.example at the tag api.DefaultImage pins, so the
// operator-rendered config.yaml registers the same resource types that image's
// API binary expects. "Keep it in sync with the API's example" is doing real
// work here: this set, the pinned image version, and that version's config
// schema (pkg/registry, server.jwt shape) all move together — bumping one
// without the others is exactly how PR #6's review caught a v0.2.1 binary
// rejecting this config at startup (UnmarshalExact). There is no automated
// check across repos for this; it is a manual invariant to watch when either
// side changes.
var cloudCAPIEntities = []api.EntityDescriptor{
	{
		Kind:              "Cluster",
		Plural:            "clusters",
		SpecSchemaName:    "ClusterSpec",
		RequiredAdapters:  []string{"validation", "dns", "pullsecret", "hypershift"},
		NameMinLen:        3,
		NameMaxLen:        53,
		RequireSpecSchema: true,
	},
	{
		Kind:              "NodePool",
		Plural:            "nodepools",
		ParentKind:        "Cluster",
		OnParentDelete:    "cascade",
		SpecSchemaName:    "NodePoolSpec",
		RequiredAdapters:  []string{"validation", "hypershift"},
		NameMinLen:        3,
		NameMaxLen:        15,
		RequireSpecSchema: true,
	},
	{Kind: "Channel", Plural: "channels", SpecSchemaName: "ChannelSpec"},
	{Kind: "Version", Plural: "versions", ParentKind: "Channel", OnParentDelete: "restrict", SpecSchemaName: "VersionSpec"},
	{Kind: "WifConfig", Plural: "wifconfigs", SpecSchemaName: "WifConfigSpec"},
}

// entitiesForBundle returns the entity registration set for a bundle, or an
// error if the bundle has none defined yet. An empty entity set is not a safe
// default to fall back to silently: the API then registers NO entity types at
// all (LoadDescriptors ranges over the slice; there is no built-in default
// set), so it would serve zero resource routes while still reporting as
// healthy — a partner who picks that bundle gets a running-looking API with no
// visible signal that it does nothing. Failing resolution instead surfaces the
// gap immediately.
func entitiesForBundle(b hyperfleetv1alpha1.BundleType) ([]api.EntityDescriptor, error) {
	switch b {
	case hyperfleetv1alpha1.BundleCloudCAPI:
		return cloudCAPIEntities, nil
	case hyperfleetv1alpha1.BundleOnPremAgent:
		// The on-prem/agent bundle's entity set is not yet defined. Until it is,
		// resolving this bundle must fail rather than silently produce an API that
		// serves no routes.
		return nil, fmt.Errorf("bundle %q has no entity set defined yet; it cannot be resolved", b)
	default:
		return nil, fmt.Errorf("unknown bundle %q", b)
	}
}

// sharedTier lists the components present in every bundle regardless of flavor.
// In phase 1 this is exactly [API], so every bundle resolves to [API]. It takes
// the bundle so the API component can be given the bundle-specific entity set.
func sharedTier(b hyperfleetv1alpha1.BundleType, cfg Config) ([]Component, error) {
	entities, err := entitiesForBundle(b)
	if err != nil {
		return nil, err
	}
	return []Component{
		api.New(cfg.APIImage, cfg.Namespace, api.Options{
			Entities:        entities,
			ResolvedJWKSURL: cfg.ResolvedJWKSURL,
		}),
	}, nil
}

// bundleSpecific returns the components unique to a bundle beyond the shared
// tier. Phase 1 has none for either bundle; this is the single extension point
// where a future bundle-specific component is registered.
func bundleSpecific(_ hyperfleetv1alpha1.BundleType) []Component {
	return nil
}

// Resolve maps a bundle to its ordered component set: the shared tier followed
// by any bundle-specific components. The shared tier is first so its
// components (currently the API) reconcile before anything that might depend
// on them. It errors for a bundle with no usable component set (see
// entitiesForBundle) rather than resolving to a silently-broken deployment.
func Resolve(b hyperfleetv1alpha1.BundleType, cfg Config) ([]Component, error) {
	shared, err := sharedTier(b, cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve bundle %q: %w", b, err)
	}
	return append(shared, bundleSpecific(b)...), nil
}

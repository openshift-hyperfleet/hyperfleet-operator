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

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

// DefaultImage is the compiled-in fallback image used when the operator is not
// given RELATED_IMAGE_HYPERFLEET_API. Production deployments override it with a
// digest-pinned image via that env var (OLM relatedImages convention).
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
const DefaultImage = "quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-api:0.4.0"

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

// Conditions reports the component's health as metav1.Conditions, derived from
// the live state of the Deployment Render produced (already updated in place by
// apply.Objects with the server's current status — see bundle.Component). It
// never reads the cluster itself.
//
// Available reflects whether the Deployment is up: True only once all desired
// replicas are ready, distinguishing "not present at all" (DeploymentUnavailable)
// from "up but partially ready" (DeploymentNotReady). Progressing is derived from
// replica-count/generation lag rather than trusting the Deployment's own
// "Progressing" condition Reason verbatim, since that built-in condition stays
// True/NewReplicaSetAvailable even at steady state, which is not what
// HyperFleetConfig's Progressing means.
func (c *Component) Conditions(_ context.Context, _ *hyperfleetv1alpha1.HyperFleetConfig, applied []client.Object) ([]metav1.Condition, error) {
	dep := findDeployment(applied)
	return []metav1.Condition{
		availableCondition(dep),
		progressingCondition(dep),
	}, nil
}

// findDeployment returns the API Deployment from applied, or nil if absent.
func findDeployment(applied []client.Object) *appsv1.Deployment {
	for _, o := range applied {
		if dep, ok := o.(*appsv1.Deployment); ok && dep.Name == ResourceName {
			return dep
		}
	}
	return nil
}

// desiredReplicas returns dep.Spec.Replicas, defaulting to 1 to match
// render.go's current fixed baseline (Replicas is not yet configurable).
func desiredReplicas(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas != nil {
		return *dep.Spec.Replicas
	}
	return 1
}

// availableCondition derives Available from the Deployment's replica counts.
func availableCondition(dep *appsv1.Deployment) metav1.Condition {
	if dep == nil {
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentUnavailable,
			Message: "the API Deployment does not exist",
		}
	}

	desired := desiredReplicas(dep)

	switch {
	case desired == 0:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentAvailable,
			Message: "the API Deployment is scaled to zero",
		}
	case dep.Status.AvailableReplicas == 0:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentUnavailable,
			Message: "the API Deployment has no available replicas",
		}
	case dep.Status.AvailableReplicas < desired:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentNotReady,
			Message: fmt.Sprintf("the API Deployment has %d/%d replicas available", dep.Status.AvailableReplicas, desired),
		}
	default:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentAvailable,
			Message: "the API Deployment is available",
		}
	}
}

// progressingCondition derives Progressing from replica-count and generation
// lag: the Deployment has not caught up with its most recently observed
// generation, or not all replicas have been updated to the current template.
func progressingCondition(dep *appsv1.Deployment) metav1.Condition {
	if dep == nil {
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonRolloutInProgress,
			Message: "the API Deployment does not exist yet",
		}
	}

	desired := desiredReplicas(dep)

	if dep.Status.ObservedGeneration < dep.Generation || dep.Status.UpdatedReplicas < desired {
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonRolloutInProgress,
			Message: "the API Deployment rollout has not completed",
		}
	}
	return metav1.Condition{
		Type:    hyperfleetv1alpha1.ConditionProgressing,
		Status:  metav1.ConditionFalse,
		Reason:  hyperfleetv1alpha1.ReasonRolloutComplete,
		Message: "the API Deployment rollout is complete",
	}
}

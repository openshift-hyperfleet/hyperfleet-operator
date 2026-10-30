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

package sentinel

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
)

// Options carries all construction inputs for a Sentinel component instance.
// It is embedded in Component so there is a single source of truth for field
// names and documentation.
type Options struct {
	// Image is the Sentinel container image; Render errors when empty (no
	// compiled-in fallback).
	Image string
	// Namespace is the operator's own namespace, where all operands live.
	Namespace string
	// ResourceType is the plural resource type this instance watches.
	ResourceType string
	// APIBaseURL is the base URL of the API component this instance talks to.
	APIBaseURL string
	// BrokerInfo carries the selected message broker type and its
	// settings (the Google Pub/Sub project ID when the type is
	// googlepubsub). Read once from environment variables in main.go and
	// threaded here via bundle.Config.
	BrokerInfo *BrokerInfo
	// ParentKind is set if ResourceType has a parent
	ParentKind string
}

// Component renders one Sentinel instance, watching a single resource type. A
// bundle resolves to one Component per registered entity (one process watches
// one resource type). It carries only rendering inputs and never touches the
// cluster; the controller applies what Render produces.
type Component struct {
	Options
}

// New constructs one Sentinel component instance.
func New(opts Options) *Component {
	return &Component{Options: opts}
}

// Name identifies this instance in logs, metrics and labels; suffixed with
// ResourceType because a bundle resolves to several instances.
func (c *Component) Name() string {
	return fmt.Sprintf("%s-%s", ComponentName, c.ResourceType)
}

// Rollout returns the ConfigMap and Deployment names the controller uses to
// stamp a config-hash annotation for rolling pods on config/secret changes.
func (c *Component) Rollout() component.RolloutTarget {
	return component.RolloutTarget{
		ConfigEntries: []component.ConfigEntry{
			{ConfigMapName: ConfigMapName(c.ResourceType), ConfigFileKey: ConfigFileKey},
			{ConfigMapName: BrokerConfigMapName(c.ResourceType), ConfigFileKey: BrokerConfigFileKey},
		},
		DeploymentName: ResourceName(c.ResourceType),
	}
}

// ReferencedSecrets returns the Secrets this Sentinel instance consumes. Only
// RabbitMQ requires a credential Secret; it must be pre-created by the partner
// (the operator references but never creates it). A rotation of the Secret
// triggers a reconcile and rolls the Sentinel pods via the config hash.
func (c *Component) ReferencedSecrets(cr *hyperfleetv1alpha1.HyperFleetConfig) []component.SecretReference {
	refs := []component.SecretReference{}
	if c.BrokerInfo.Type == BrokerTypeRabbitMQ {
		refs = append(refs, component.SecretReference{
			Role:       "broker",
			SecretName: BrokerSecretName,
		})
	}
	return refs
}

// Render returns this instance's operand set in dependency order. Pure: no
// cluster reads or writes.
func (c *Component) Render(_ context.Context, cr *hyperfleetv1alpha1.HyperFleetConfig) ([]client.Object, error) {
	// No compiled-in default: fail loudly rather than render a dev tag.
	if c.Image == "" {
		return nil, fmt.Errorf("sentinel image is required")
	}

	configYAML, err := renderConfig(configInput{
		ResourceType: c.ResourceType,
		HasParent:    c.ParentKind != "",
		APIBaseURL:   c.APIBaseURL,
		BrokerTopic:  brokerTopic(c.Namespace, c.ResourceType),
		AuthEnabled:  api.AuthEnabled(cr),
	})
	if err != nil {
		return nil, fmt.Errorf("render sentinel config for %q: %w", c.ResourceType, err)
	}
	brokerYAML, err := renderBrokerConfig(c.BrokerInfo)
	if err != nil {
		return nil, fmt.Errorf("render sentinel broker config for %q: %w", c.ResourceType, err)
	}

	return []client.Object{
		serviceAccount(cr.Name, c.Namespace, c.ResourceType),
		configMap(cr.Name, c.Namespace, c.ResourceType, configYAML),
		brokerConfigMap(cr.Name, c.Namespace, c.ResourceType, brokerYAML),
		networkPolicy(cr.Name, c.Namespace, c.ResourceType),
		deployment(cr, c.Image, c.Namespace, c.ResourceType, c.BrokerInfo.Type),
	}, nil
}

// Conditions derives health from the applied Deployment (refreshed in place by
// apply.Objects). Identical to api.Conditions — both are Deployment-backed
// workloads with no further readiness signal of their own.
func (c *Component) Conditions(_ context.Context, _ *hyperfleetv1alpha1.HyperFleetConfig, applied []client.Object) ([]metav1.Condition, error) {
	dep := findDeployment(applied, ResourceName(c.ResourceType))
	return []metav1.Condition{
		availableCondition(dep),
		progressingCondition(dep),
	}, nil
}

// findDeployment returns the named Deployment from applied, or nil if absent.
func findDeployment(applied []client.Object, name string) *appsv1.Deployment {
	for _, o := range applied {
		if dep, ok := o.(*appsv1.Deployment); ok && dep.Name == name {
			return dep
		}
	}
	return nil
}

// desiredReplicas returns dep.Spec.Replicas, defaulting to 1.
func desiredReplicas(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas != nil {
		return *dep.Spec.Replicas
	}
	return 1
}

// availableCondition derives Available from the Deployment's replica counts;
// see api.availableCondition for the rationale.
func availableCondition(dep *appsv1.Deployment) metav1.Condition {
	if dep == nil {
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentUnavailable,
			Message: "the Sentinel Deployment does not exist",
		}
	}

	desired := desiredReplicas(dep)

	switch {
	case desired == 0:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentAvailable,
			Message: "the Sentinel Deployment is scaled to zero",
		}
	case dep.Status.AvailableReplicas == 0:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentUnavailable,
			Message: "the Sentinel Deployment has no available replicas",
		}
	case dep.Status.AvailableReplicas < desired:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentNotReady,
			Message: fmt.Sprintf("the Sentinel Deployment has %d/%d replicas available", dep.Status.AvailableReplicas, desired),
		}
	default:
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonDeploymentAvailable,
			Message: "the Sentinel Deployment is available",
		}
	}
}

// progressingCondition derives Progressing from the same signals Kubernetes
// uses for rollout completion; see api.progressingCondition.
func progressingCondition(dep *appsv1.Deployment) metav1.Condition {
	if dep == nil {
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonRolloutInProgress,
			Message: "the Sentinel Deployment does not exist yet",
		}
	}

	desired := desiredReplicas(dep)

	if dep.Status.ObservedGeneration < dep.Generation ||
		dep.Status.UpdatedReplicas < desired ||
		dep.Status.Replicas != dep.Status.UpdatedReplicas ||
		dep.Status.AvailableReplicas < dep.Status.UpdatedReplicas {
		return metav1.Condition{
			Type:    hyperfleetv1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  hyperfleetv1alpha1.ReasonRolloutInProgress,
			Message: "the Sentinel Deployment rollout has not completed",
		}
	}
	return metav1.Condition{
		Type:    hyperfleetv1alpha1.ConditionProgressing,
		Status:  metav1.ConditionFalse,
		Reason:  hyperfleetv1alpha1.ReasonRolloutComplete,
		Message: "the Sentinel Deployment rollout is complete",
	}
}

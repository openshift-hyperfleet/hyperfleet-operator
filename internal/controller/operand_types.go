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

package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NamespacedOperandTypes is the single source of truth for the namespaced
// objects rendered by the bundle components. Callers receive fresh objects so
// the returned values can safely be used as cache map keys and watch inputs.
// The list must stay aligned with the namespaced operand RBAC markers below.
func NamespacedOperandTypes() []client.Object {
	return []client.Object{
		&appsv1.Deployment{},
		&corev1.Service{},
		&corev1.ServiceAccount{},
		&corev1.ConfigMap{},
		&rbacv1.Role{},
		&rbacv1.RoleBinding{},
	}
}

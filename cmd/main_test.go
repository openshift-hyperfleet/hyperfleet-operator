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

package main

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestNamespacedCacheByObject(t *testing.T) {
	t.Parallel()

	const operatorNamespace = "test-operator"
	got := namespacedCacheByObject(operatorNamespace)
	wantTypes := []reflect.Type{
		reflect.TypeFor[*corev1.Secret](),
		reflect.TypeFor[*appsv1.Deployment](),
		reflect.TypeFor[*corev1.Service](),
		reflect.TypeFor[*corev1.ServiceAccount](),
		reflect.TypeFor[*corev1.ConfigMap](),
		reflect.TypeFor[*rbacv1.Role](),
		reflect.TypeFor[*rbacv1.RoleBinding](),
	}

	if len(got) != len(wantTypes) {
		t.Fatalf("namespacedCacheByObject() returned %d types, want %d", len(got), len(wantTypes))
	}
	for _, wantType := range wantTypes {
		var found bool
		for object, config := range got {
			if reflect.TypeOf(object) != wantType {
				continue
			}
			found = true
			if len(config.Namespaces) != 1 {
				t.Fatalf("cache config for %v has namespaces %v, want only %q", wantType, config.Namespaces, operatorNamespace)
			}
			if _, ok := config.Namespaces[operatorNamespace]; !ok {
				t.Fatalf("cache config for %v does not include %q: %v", wantType, operatorNamespace, config.Namespaces)
			}
			break
		}
		if !found {
			t.Errorf("namespacedCacheByObject() is missing %v", wantType)
		}
	}

}

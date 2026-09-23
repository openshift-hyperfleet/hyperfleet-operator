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
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestNamespacedOperandTypes(t *testing.T) {
	t.Parallel()

	want := []reflect.Type{
		reflect.TypeFor[*appsv1.Deployment](),
		reflect.TypeFor[*corev1.Service](),
		reflect.TypeFor[*corev1.ServiceAccount](),
		reflect.TypeFor[*corev1.ConfigMap](),
		reflect.TypeFor[*rbacv1.Role](),
		reflect.TypeFor[*rbacv1.RoleBinding](),
	}

	gotObjects := NamespacedOperandTypes()
	got := make([]reflect.Type, 0, len(gotObjects))
	for _, object := range gotObjects {
		got = append(got, reflect.TypeOf(object))
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NamespacedOperandTypes() types = %v, want %v", got, want)
	}
}

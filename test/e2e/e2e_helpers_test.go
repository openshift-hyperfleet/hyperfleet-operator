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

import "testing"

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

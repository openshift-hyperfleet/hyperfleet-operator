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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const operatorNamespace = "hyperfleet-system"

var _ = Describe("HyperFleetConfig controller setup", func() {
	It("wraps a real Complete failure while preserving the underlying error", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())

		reconciler := &HyperFleetConfigReconciler{
			Client:            mgr.GetClient(),
			APIReader:         mgr.GetAPIReader(),
			Scheme:            mgr.GetScheme(),
			OperatorNamespace: operatorNamespace,
		}
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

		// controller-runtime rejects a second controller with the same explicit
		// name. Calling the actual SetupWithManager path proves that the Complete
		// error (not merely a helper) gets controller-specific context and retains
		// its unwrap chain.
		err = reconciler.SetupWithManager(mgr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("complete hyperfleetconfig controller"))
		Expect(errors.Unwrap(err)).NotTo(Succeed())
	})
})

var _ = Describe("HyperFleetConfig controller setup error", func() {
	It("leaves nil unchanged", func() {
		Expect(wrapHyperFleetConfigControllerSetupError(nil)).To(Succeed())
	})

	It("retains the exact cause for errors.Is callers", func() {
		root := errors.New("duplicate controller name")
		err := wrapHyperFleetConfigControllerSetupError(root)
		Expect(err.Error()).To(ContainSubstring("complete hyperfleetconfig controller"))
		Expect(errors.Is(err, root)).To(BeTrue())
	})
})

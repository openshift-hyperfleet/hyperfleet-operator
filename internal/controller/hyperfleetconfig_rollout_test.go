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
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	apicomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
)

// These are plain unit tests (no envtest): OIDC discovery, the content-hash, and
// the Secret→request mapping are all pure or use an in-process HTTP server, so
// they need no API server.

// Hash-entry ids reused across TestComputeConfigHashProperties and
// TestStampConfigHashSetsAnnotation. One entry per referenced Secret (its
// resourceVersion), not per key — see referencedSecretData.
const (
	dbSecretHashID  = "database"
	tlsSecretHashID = "tls"
)

// blockedLoopbackIssuer is a loopback address the default (hardened) discovery
// client always refuses to dial (see blockDiscoveryDial), so a discovery call
// against it fails immediately without touching the network. Used where a test
// needs any reliably-failing discovery call and doesn't care about the error
// itself — e.g. proving the default client is built once and reused.
const blockedLoopbackIssuer = "http://127.0.0.1:1"

// discoveryCR returns an auth-enabled CR with no pinned JWKS source, so
// resolveJWKSURL falls through to OIDC discovery against the given issuer.
func discoveryCR(issuer string) *hyperfleetv1alpha1.HyperFleetConfig {
	return &hyperfleetv1alpha1.HyperFleetConfig{
		ObjectMeta: metav1.ObjectMeta{Name: hyperfleetv1alpha1.SingletonName},
		Spec: hyperfleetv1alpha1.HyperFleetConfigSpec{
			Bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			API: hyperfleetv1alpha1.APISpec{
				Auth: hyperfleetv1alpha1.AuthSpec{
					Enabled:  ptr.To(true),
					Issuer:   issuer,
					Audience: "hyperfleet-api",
				},
			},
		},
	}
}

func TestResolveJWKSURLDiscoversFromIssuer(t *testing.T) {
	g := NewWithT(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		// The issuer in the document must match the one we asked for (the server's
		// own URL); "http://"+r.Host reconstructs it for the httptest server.
		// t.Errorf, not t.Fatalf: this handler runs on the server's own goroutine,
		// and FailNow (which Fatal calls) is only safe from the test's goroutine.
		if _, err := w.Write([]byte(`{"issuer":"http://` + r.Host + `","jwks_uri":"https://issuer.example.com/keys"}`)); err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	defer srv.Close()

	r := &HyperFleetConfigReconciler{HTTPClient: srv.Client()}
	url, err := r.resolveJWKSURL(context.Background(), discoveryCR(srv.URL))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(url).To(Equal("https://issuer.example.com/keys"))
	g.Expect(gotPath).To(Equal("/.well-known/openid-configuration"))
}

func TestDiscoverJWKSURLRejectsIssuerMismatch(t *testing.T) {
	g := NewWithT(t)

	// 200 with a well-formed https jwks_uri but an issuer that does not match the
	// one we asked for: the document is untrusted and must be rejected before its
	// jwks_uri is used (OIDC Discovery 1.0 §4.3).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"issuer":"https://evil.example.com","jwks_uri":"https://evil.example.com/keys"}`)); err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	defer srv.Close()

	r := &HyperFleetConfigReconciler{HTTPClient: srv.Client()}
	_, err := r.discoverJWKSURL(context.Background(), srv.URL)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("issuer"))
}

func TestDiscoverJWKSURLRejectsNonHTTPS(t *testing.T) {
	g := NewWithT(t)

	// Matching issuer but a plaintext jwks_uri: retrieving signing keys over http
	// is MITM-able, so the discovered URL must be rejected even though the pinned
	// jwkCertURL is the only one the CRD guards.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"issuer":"http://` + r.Host + `","jwks_uri":"http://insecure.example.com/keys"}`)); err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	defer srv.Close()

	r := &HyperFleetConfigReconciler{HTTPClient: srv.Client()}
	_, err := r.discoverJWKSURL(context.Background(), srv.URL)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("non-https"))
}

func TestResolveJWKSURLSkipsDiscoveryWhenPinned(t *testing.T) {
	g := NewWithT(t)

	// A client pointed at a closed server would error if discovery were attempted;
	// with a pinned Secret it must never be called.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.Close()

	r := &HyperFleetConfigReconciler{HTTPClient: srv.Client()}

	cr := discoveryCR(srv.URL)
	cr.Spec.API.Auth.JWKCertSecretRef = &hyperfleetv1alpha1.SecretReference{Name: "hyperfleet-jwks"}
	url, err := r.resolveJWKSURL(context.Background(), cr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(url).To(BeEmpty(), "pinned Secret: renderer reads it from the CR, no discovery")

	// Auth disabled: also no discovery.
	off := discoveryCR(srv.URL)
	off.Spec.API.Auth.Enabled = ptr.To(false)
	url, err = r.resolveJWKSURL(context.Background(), off)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(url).To(BeEmpty())
}

func TestDiscoverJWKSURLErrors(t *testing.T) {
	g := NewWithT(t)

	// Non-200.
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	r := &HyperFleetConfigReconciler{HTTPClient: notFound.Client()}
	_, err := r.discoverJWKSURL(context.Background(), notFound.URL)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("status 404"))

	// 200 with a matching issuer but no jwks_uri (issuer is validated first, so it
	// must match to reach the missing-jwks_uri error).
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"issuer":"http://` + r.Host + `"}`)); err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	defer empty.Close()
	r = &HyperFleetConfigReconciler{HTTPClient: empty.Client()}
	_, err = r.discoverJWKSURL(context.Background(), empty.URL)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no jwks_uri"))
}

func TestDiscoverJWKSURLReusesDefaultClient(t *testing.T) {
	g := NewWithT(t)

	// HTTPClient is deliberately left nil to exercise the lazily-built default
	// client. The dial is expected to fail — the default client blocks loopback
	// destinations (TestDiscoveryHTTPClientBlocksLoopbackDial) — the point here
	// is only that the client is constructed once and reused across calls, not
	// the discovery outcome.
	r := &HyperFleetConfigReconciler{}
	_, err := r.discoverJWKSURL(context.Background(), blockedLoopbackIssuer)
	g.Expect(err).To(HaveOccurred())
	first := r.discoveryClient
	g.Expect(first).NotTo(BeNil())

	_, err = r.discoverJWKSURL(context.Background(), blockedLoopbackIssuer)
	g.Expect(err).To(HaveOccurred())
	g.Expect(r.discoveryClient).To(BeIdenticalTo(first),
		"the default client must be built once (discoveryClientOnce) and reused, not rebuilt per call")
}

func TestResolveJWKSURLCachesSuccessfulDiscovery(t *testing.T) {
	g := NewWithT(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"issuer":"http://` + r.Host + `","jwks_uri":"https://issuer.example.com/keys"}`)); err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	defer srv.Close()

	r := &HyperFleetConfigReconciler{HTTPClient: srv.Client()}
	url, err := r.resolveJWKSURL(context.Background(), discoveryCR(srv.URL))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(url).To(Equal("https://issuer.example.com/keys"))

	cached, ok := r.cachedDiscovery(srv.URL)
	g.Expect(ok).To(BeTrue())
	g.Expect(cached).To(Equal(url))
}

func TestResolveJWKSURLFallsBackToCachedDiscoveryOnFailure(t *testing.T) {
	g := NewWithT(t)

	// A discovery endpoint that always fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := &HyperFleetConfigReconciler{HTTPClient: srv.Client()}
	r.cacheDiscovery(srv.URL, "https://issuer.example.com/cached-keys")

	// The failing discovery call must not fail the reconcile: a prior successful
	// result is cached for this issuer, so a rotation-triggered reconcile (or any
	// other) still succeeds using the last-known jwks_uri.
	url, err := r.resolveJWKSURL(context.Background(), discoveryCR(srv.URL))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(url).To(Equal("https://issuer.example.com/cached-keys"))
}

func TestResolveJWKSURLFailsWithoutCache(t *testing.T) {
	g := NewWithT(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// No cached result for this issuer: the very first discovery still must
	// fail the reconcile rather than silently proceeding with no JWKS source.
	r := &HyperFleetConfigReconciler{HTTPClient: srv.Client()}
	_, err := r.resolveJWKSURL(context.Background(), discoveryCR(srv.URL))
	g.Expect(err).To(HaveOccurred())
}

func TestIsDisallowedDiscoveryTarget(t *testing.T) {
	g := NewWithT(t)

	disallowed := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.5", "172.16.0.5", "192.168.1.5", // RFC1918 private
		"169.254.169.254", "169.254.1.1", // link-local, incl. cloud metadata
		"0.0.0.0",       // unspecified
		"224.0.0.1",     // multicast
		"fc00::1",       // IPv6 unique local
		"100.64.0.1",    // CGNAT / shared address space (RFC 6598)
		"100.127.255.1", // CGNAT upper bound
		"0.1.2.3",       // "this host on this network" (RFC 1122)
		"192.0.0.1",     // IETF protocol assignments
		"198.18.0.1",    // benchmarking
		"240.0.0.1",     // reserved / former class E
		"2001:db8::1",   // IPv6 documentation
	}
	for _, s := range disallowed {
		g.Expect(isDisallowedDiscoveryTarget(net.ParseIP(s))).To(BeTrue(), s)
	}

	allowed := []string{
		"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888",
		"100.63.255.255", "100.128.0.0", // just outside the CGNAT block, still public
	}
	for _, s := range allowed {
		g.Expect(isDisallowedDiscoveryTarget(net.ParseIP(s))).To(BeFalse(), s)
	}
}

func TestDiscoveryHTTPClientRefusesRedirects(t *testing.T) {
	g := NewWithT(t)

	c := newDiscoveryHTTPClient()
	g.Expect(c.CheckRedirect(&http.Request{}, nil)).To(MatchError(errNoDiscoveryRedirects))
}

func TestDiscoveryHTTPClientBlocksLoopbackDial(t *testing.T) {
	g := NewWithT(t)

	// httptest servers listen on loopback, so this proves the default (hardened)
	// client refuses the connection even to an otherwise well-formed, reachable
	// discovery endpoint — the dial itself is blocked before any HTTP occurs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"issuer":"http://` + r.Host + `","jwks_uri":"https://issuer.example.com/keys"}`)); err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	defer srv.Close()

	r := &HyperFleetConfigReconciler{} // HTTPClient nil → newDiscoveryHTTPClient()
	_, err := r.discoverJWKSURL(context.Background(), srv.URL)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("disallowed address"))
}

func TestComputeConfigHashProperties(t *testing.T) {
	g := NewWithT(t)

	base := []hashEntry{
		{id: dbSecretHashID, present: true, value: []byte("1001")},
		{id: tlsSecretHashID, present: true, value: []byte("1002")},
	}

	h := computeConfigHash("config-a", base)
	g.Expect(h).To(HaveLen(64)) // hex-encoded SHA-256

	// Stable and order-independent (entries are sorted by id internally).
	reordered := []hashEntry{base[1], base[0]}
	g.Expect(computeConfigHash("config-a", reordered)).To(Equal(h))

	// Config change → different hash.
	g.Expect(computeConfigHash("config-b", base)).NotTo(Equal(h))

	// resourceVersion change (any write, including rotation) → different hash.
	rotated := []hashEntry{base[0], {id: tlsSecretHashID, present: true, value: []byte("1003")}}
	g.Expect(computeConfigHash("config-a", rotated)).NotTo(Equal(h))

	// Absent vs present-empty must differ (the present/absent discriminator).
	absent := []hashEntry{base[0], {id: tlsSecretHashID, present: false}}
	presentEmpty := []hashEntry{base[0], {id: tlsSecretHashID, present: true, value: []byte("")}}
	g.Expect(computeConfigHash("config-a", absent)).NotTo(Equal(computeConfigHash("config-a", presentEmpty)))

	// A present value equal to the bytes of any absent marker must still differ
	// from a genuinely absent datum: the discriminator byte keeps them distinct so
	// no value can masquerade as "absent".
	presentAbsentLiteral := []hashEntry{base[0], {id: tlsSecretHashID, present: true, value: []byte("<absent>")}}
	g.Expect(computeConfigHash("config-a", absent)).NotTo(Equal(computeConfigHash("config-a", presentAbsentLiteral)))
}

func TestStampConfigHashSetsAnnotation(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{
		ObjectMeta: metav1.ObjectMeta{Name: hyperfleetv1alpha1.SingletonName},
		Spec: hyperfleetv1alpha1.HyperFleetConfigSpec{
			Bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			API: hyperfleetv1alpha1.APISpec{
				Database: hyperfleetv1alpha1.DatabaseSpec{
					SecretRef: hyperfleetv1alpha1.SecretReference{Name: testDBSecretName},
				},
				Auth: hyperfleetv1alpha1.AuthSpec{
					Enabled:          ptr.To(true),
					Issuer:           "https://issuer.example.com",
					Audience:         "hyperfleet-api",
					JWKCertSecretRef: &hyperfleetv1alpha1.SecretReference{Name: "hyperfleet-jwks"},
				},
			},
		},
	}

	render := func() []client.Object {
		objs, err := apicomponent.New("img", "hyperfleet-system", apicomponent.Options{}).Render(context.Background(), cr)
		g.Expect(err).NotTo(HaveOccurred())
		return objs
	}

	entries := []hashEntry{{id: dbSecretHashID, present: true, value: []byte("h1")}}
	objs := render()
	stampConfigHash(objs, entries)

	depOf := func(objs []client.Object) *appsv1.Deployment {
		for _, o := range objs {
			if d, ok := o.(*appsv1.Deployment); ok {
				return d
			}
		}
		return nil
	}

	dep := depOf(objs)
	g.Expect(dep).NotTo(BeNil())
	got := dep.Spec.Template.Annotations[configHashAnnotation]
	g.Expect(got).NotTo(BeEmpty())

	// Re-stamping the same render with the same entries yields the same hash.
	objs2 := render()
	stampConfigHash(objs2, entries)
	g.Expect(depOf(objs2).Spec.Template.Annotations[configHashAnnotation]).To(Equal(got))

	// A rotated secret value changes the stamped hash.
	objs3 := render()
	stampConfigHash(objs3, []hashEntry{{id: dbSecretHashID, present: true, value: []byte("h2")}})
	g.Expect(depOf(objs3).Spec.Template.Annotations[configHashAnnotation]).NotTo(Equal(got))
}

func TestMapSecretToConfig(t *testing.T) {
	g := NewWithT(t)

	r := &HyperFleetConfigReconciler{OperatorNamespace: "hyperfleet-system"}

	inNS := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "hyperfleet-db", Namespace: "hyperfleet-system"}}
	reqs := r.mapSecretToConfig(context.Background(), inNS)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].Name).To(Equal(hyperfleetv1alpha1.SingletonName))

	other := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "hyperfleet-db", Namespace: "elsewhere"}}
	g.Expect(r.mapSecretToConfig(context.Background(), other)).To(BeEmpty())
}

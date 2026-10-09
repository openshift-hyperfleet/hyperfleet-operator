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
	"strings"
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
	tests := []struct {
		name       string
		address    string
		disallowed bool
	}{
		// net.IP categories that are disallowed independently of the IANA table.
		{"IPv4 loopback", "127.0.0.1", true},
		{"IPv6 loopback", "::1", true},
		{"RFC1918 private", "10.0.0.5", true},
		{"IPv6 unique local", "fc00::1", true},
		{"link-local cloud metadata", "169.254.169.254", true},
		{"unspecified", "0.0.0.0", true},
		{"multicast", "224.0.0.1", true},

		// IPv4 IANA categories and their boundary addresses.
		{"IPv4 this network", "0.1.2.3", true},
		{"IPv4 shared address lower boundary", "100.64.0.0", true},
		{"IPv4 shared address upper boundary", "100.127.255.255", true},
		{"IPv4 IETF protocol assignment", "192.0.0.1", true},
		{"IPv4 dummy address", "192.0.0.8", true},
		{"IPv4 NAT64 discovery", "192.0.0.170", true},
		{"IPv4 documentation", "192.0.2.1", true},
		{"IPv4 retired relay anycast lower boundary", "192.88.99.0", true},
		{"IPv4 retired relay anycast upper boundary", "192.88.99.255", true},
		{"IPv4 private use", "192.168.1.5", true},
		{"IPv4 benchmarking", "198.18.0.1", true},
		{"IPv4 documentation TEST-NET-2", "198.51.100.1", true},
		{"IPv4 documentation TEST-NET-3", "203.0.113.1", true},
		{"IPv4 reserved", "240.0.0.1", true},

		// IPv6 IANA categories and their boundary addresses.
		{"IPv4-mapped IPv6", "::ffff:192.0.2.1", true},
		{"IPv6 translation prefix with no global reachability", "64:ff9b:1::1", true},
		{"IPv6 discard-only", "100::1", true},
		{"IPv6 dummy prefix", "100:0:0:1::1", true},
		{"IPv6 IETF protocol assignment", "2001:0:ffff::1", true},
		{"IPv6 Teredo N/A reachability", "2001::1", true},
		{"IPv6 benchmarking lower boundary", "2001:2::", true},
		{"IPv6 benchmarking upper boundary", "2001:2:0:ffff:ffff:ffff:ffff:ffff", true},
		{"IPv6 retired ORCHID", "2001:10::1", true},
		{"IPv6 documentation", "2001:db8::1", true},
		{"IPv6 6to4 N/A reachability", "2002::1", true},
		{"IPv6 documentation 3fff", "3fff::1", true},
		{"IPv6 SRv6 SID", "5f00::1", true},
		{"IPv6 link-local", "fe80::1", true},

		// Explicitly globally reachable special-purpose exceptions must remain allowed.
		{"IPv4 PCP anycast exception", "192.0.0.9", false},
		{"IPv4 TURN anycast exception", "192.0.0.10", false},
		{"IPv4 AS112 exception", "192.31.196.1", false},
		{"IPv4 AMT exception", "192.52.193.1", false},
		{"IPv4 direct-delegation AS112 exception", "192.175.48.1", false},
		{"IPv6 translation exception", "64:ff9b::1", false},
		{"IPv6 PCP anycast exception", "2001:1::1", false},
		{"IPv6 TURN anycast exception", "2001:1::2", false},
		{"IPv6 DNS-SD anycast exception", "2001:1::3", false},
		{"IPv6 AMT exception", "2001:3::1", false},
		{"IPv6 AS112 exception", "2001:4:112::1", false},
		{"IPv6 ORCHIDv2 exception", "2001:20::1", false},
		{"IPv6 DETs exception", "2001:30::1", false},
		{"IPv6 direct-delegation AS112 exception", "2620:4f:8000::1", false},

		// Adjacent public space proves the boundaries do not silently widen the deny list.
		{"IPv4 before shared address space", "100.63.255.255", false},
		{"IPv4 after shared address space", "100.128.0.0", false},
		{"IPv4 before retired relay anycast", "192.88.98.255", false},
		{"IPv4 after retired relay anycast", "192.88.100.0", false},
		// These addresses are immediately adjacent to the /48 benchmarking
		// allocation, but remain in the broader 2001::/23 IETF allocation and
		// are therefore correctly denied. The next line proves the outer /23
		// boundary itself does not overreach.
		{"IPv6 before benchmarking remains in IETF allocation", "2001:1:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"IPv6 after benchmarking remains in IETF allocation", "2001:2:1::", true},
		{"IPv6 after IETF allocation", "2001:200::", false},
		{"ordinary public IPv4", "8.8.8.8", false},
		{"ordinary public IPv6", "2001:4860:4860::8888", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.address)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) = nil", tc.address)
			}
			if got := isDisallowedDiscoveryTarget(ip); got != tc.disallowed {
				t.Errorf("isDisallowedDiscoveryTarget(%s) = %t, want %t", tc.address, got, tc.disallowed)
			}
		})
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

func TestBlockDiscoveryDialBlocksIPv4MappedIPv6(t *testing.T) {
	err := blockDiscoveryDial("", "[::ffff:8.8.8.8]:443", nil)
	if err == nil || !strings.Contains(err.Error(), "IPv4-mapped") {
		t.Fatalf("blockDiscoveryDial() = %v, want IPv4-mapped address rejection", err)
	}
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

	r := &HyperFleetConfigReconciler{OperatorNamespace: operatorNamespace}

	inNS := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "hyperfleet-db", Namespace: operatorNamespace}}
	reqs := r.mapSecretToConfig(context.Background(), inNS)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].Name).To(Equal(hyperfleetv1alpha1.SingletonName))

	other := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "hyperfleet-db", Namespace: "elsewhere"}}
	g.Expect(r.mapSecretToConfig(context.Background(), other)).To(BeEmpty())
}

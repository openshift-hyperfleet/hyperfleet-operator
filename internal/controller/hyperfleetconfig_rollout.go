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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	apicomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
)

// This file holds the two cluster/network-dependent pieces of reconciliation that
// cannot live in the pure component renderer (HYPERFLEET-1408):
//
//   - OIDC discovery of the JWKS URL, and
//   - the content-hash rollout: reading each referenced Secret's resourceVersion
//     and stamping a hash on the Deployment pod template so a config change or a
//     Secret rotation rolls the pods (the Helm `checksum/config` pattern,
//     extended to referenced Secrets). The hash covers resourceVersion rather
//     than Secret data on purpose — see referencedSecretData.

// configHashAnnotation is stamped on the API Deployment's pod template. When it
// changes, the Deployment controller performs a rolling update, so a config or
// referenced-secret change takes effect even though the container image is
// unchanged.
const configHashAnnotation = "hyperfleet.redhat.com/config-hash"

// oidcDiscoveryPath is the well-known suffix appended to an issuer to fetch its
// OpenID provider metadata, per OpenID Connect Discovery 1.0.
const oidcDiscoveryPath = "/.well-known/openid-configuration"

// discoveryTimeout bounds a single OIDC discovery HTTP request.
const discoveryTimeout = 10 * time.Second

// errNoDiscoveryRedirects is returned by newDiscoveryHTTPClient's CheckRedirect
// to refuse following any redirect.
var errNoDiscoveryRedirects = errors.New("OIDC discovery does not follow redirects")

// newDiscoveryHTTPClient constructs the default client used when no HTTPClient
// is injected (production; tests always inject one pointed at an httptest
// server, and never reach this constructor). Callers must build it once and
// reuse it — see discoveryClientOnce — rather than call this per request.
// spec.api.auth.issuer is partner-supplied — anyone able to update the
// HyperFleetConfig singleton controls discoveryURL — so this client is hardened
// against using that as a pivot into the controller's network:
//   - CheckRedirect refuses every redirect, so a discovery endpoint cannot send
//     the request on to an internal target the partner could not reach directly.
//   - The dialer's Control hook inspects the resolved address of every
//     connection (including ones a redirect would otherwise have started) and
//     refuses loopback, private, link-local (including the 169.254.169.254
//     cloud metadata address), and multicast destinations.
func newDiscoveryHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: discoveryTimeout, Control: blockDiscoveryDial}
	return &http.Client{
		Timeout:   discoveryTimeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errNoDiscoveryRedirects
		},
	}
}

// blockDiscoveryDial is a net.Dialer.Control hook. It runs after DNS
// resolution on the actual address about to be dialed, so it also closes off
// DNS-rebinding: a hostname that resolves differently between an earlier check
// and the real connection is still caught here, because this is the real
// connection.
func blockDiscoveryDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("refusing to dial non-IP discovery address %q", host)
	}
	if isDisallowedDiscoveryTarget(ip) {
		return fmt.Errorf("refusing OIDC discovery dial to disallowed address %s", ip)
	}
	return nil
}

// reservedDiscoveryCIDRs are non-public destination ranges the net.IP.IsXxx
// helpers do not classify. net.IP.IsPrivate covers RFC1918 and IPv6 ULA
// (fc00::/7) but not, notably, the shared CGNAT space (100.64.0.0/10, RFC 6598)
// a partner-controlled issuer could use to pivot into a carrier- or
// cloud-internal host. The remainder are IANA special-purpose ranges that are
// never a legitimate public IdP, so blocking them costs nothing and closes the
// gap left by relying on IsPrivate alone.
var reservedDiscoveryCIDRs = []*net.IPNet{
	mustCIDR("0.0.0.0/8"),       // "this host on this network" (RFC 1122)
	mustCIDR("100.64.0.0/10"),   // shared address space / CGNAT (RFC 6598)
	mustCIDR("192.0.0.0/24"),    // IETF protocol assignments (RFC 6890)
	mustCIDR("192.0.2.0/24"),    // documentation TEST-NET-1 (RFC 5737)
	mustCIDR("198.18.0.0/15"),   // benchmarking (RFC 2544)
	mustCIDR("198.51.100.0/24"), // documentation TEST-NET-2 (RFC 5737)
	mustCIDR("203.0.113.0/24"),  // documentation TEST-NET-3 (RFC 5737)
	mustCIDR("240.0.0.0/4"),     // reserved / former class E (RFC 1112)
	mustCIDR("100::/64"),        // discard-only (RFC 6666)
	mustCIDR("2001:db8::/32"),   // documentation (RFC 3849)
}

// mustCIDR parses a CIDR literal that is a compile-time constant; a parse error
// can only be a programming error, so it panics rather than returning one.
func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("reserved discovery CIDR %q: %v", s, err))
	}
	return n
}

// isDisallowedDiscoveryTarget reports whether ip is a loopback, private,
// link-local, unspecified, or multicast address, or falls in one of the
// reserved ranges above (CGNAT and other non-public IANA special-purpose
// blocks) — the set of destinations an outbound OIDC discovery request must
// never reach, since spec.api.auth.issuer is partner-controlled. IsPrivate
// alone is insufficient: it does not cover CGNAT (100.64.0.0/10).
func isDisallowedDiscoveryTarget(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, n := range reservedDiscoveryCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveJWKSURL returns the JWKS URL the renderer should write into config.yaml,
// or "" when none is needed. It performs OIDC discovery only when the CR pins no
// Secret: auth on and jwkCertSecretRef unset. When the CR pins a Secret, the
// renderer uses the mounted file; when auth is off, no JWKS is needed. There is
// no CR field to pin a URL directly — see AuthSpec's doc comment.
//
// A discovery failure with a previously-cached result for the same issuer
// degrades to that cached jwks_uri instead of failing the reconcile: discovery
// runs on every reconcile of the singleton CR, including ones triggered solely
// by an unrelated database or TLS Secret rotation (mapSecretToConfig enqueues
// on any Secret change in the namespace), so a transient or persistent IdP
// outage would otherwise block those rotations from rolling the pods. Only the
// very first discovery for an issuer (nothing cached yet) can still fail the
// reconcile.
func (r *HyperFleetConfigReconciler) resolveJWKSURL(ctx context.Context, cr *hyperfleetv1alpha1.HyperFleetConfig) (string, error) {
	if !apicomponent.AuthEnabled(cr) {
		return "", nil
	}
	a := cr.Spec.API.Auth
	if a.JWKCertSecretRef != nil {
		return "", nil
	}

	jwksURI, err := r.discoverJWKSURL(ctx, a.Issuer)
	if err != nil {
		if cached, ok := r.cachedDiscovery(a.Issuer); ok {
			logf.FromContext(ctx).Error(err, "OIDC discovery failed; continuing with the last-known jwks_uri",
				"issuer", a.Issuer, "jwks_uri", cached)
			return cached, nil
		}
		return "", err
	}
	r.cacheDiscovery(a.Issuer, jwksURI)
	return jwksURI, nil
}

// cachedDiscovery returns the last successfully discovered jwks_uri for issuer.
func (r *HyperFleetConfigReconciler) cachedDiscovery(issuer string) (string, bool) {
	r.discoveryCacheMu.Lock()
	defer r.discoveryCacheMu.Unlock()
	uri, ok := r.discoveryCache[issuer]
	return uri, ok
}

// cacheDiscovery records a successful discovery result for issuer.
func (r *HyperFleetConfigReconciler) cacheDiscovery(issuer, jwksURI string) {
	r.discoveryCacheMu.Lock()
	defer r.discoveryCacheMu.Unlock()
	if r.discoveryCache == nil {
		r.discoveryCache = map[string]string{}
	}
	r.discoveryCache[issuer] = jwksURI
}

// discoverJWKSURL fetches {issuer}/.well-known/openid-configuration and returns
// its jwks_uri. Any transport error, non-200 status, malformed body, missing
// jwks_uri, issuer mismatch, or non-https jwks_uri is returned as an error so
// Reconcile requeues (a Degraded condition for persistent failures is
// HYPERFLEET-1512).
//
// Two security checks mirror the guarantees the CRD enforces on a pinned
// jwkCertURL, which OIDC discovery would otherwise bypass:
//   - issuer match: the discovery document's `issuer` MUST equal the configured
//     issuer (OIDC Discovery 1.0 §4.3). Without it, a redirected or spoofed
//     endpoint could bind the configured issuer to another issuer's signing keys.
//   - https jwks_uri: the returned URL MUST be https with a host, so a hostile
//     document cannot downgrade key retrieval to plaintext (MITM) or point the
//     API at an internal/attacker-controlled endpoint (SSRF).
func (r *HyperFleetConfigReconciler) discoverJWKSURL(ctx context.Context, issuer string) (string, error) {
	// Per the OIDC Discovery spec the well-known path is appended to the issuer
	// with any single trailing slash collapsed.
	normalizedIssuer := strings.TrimSuffix(issuer, "/")
	discoveryURL := normalizedIssuer + oidcDiscoveryPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("build discovery request for %q: %w", discoveryURL, err)
	}
	req.Header.Set("Accept", "application/json")

	httpClient := r.HTTPClient
	if httpClient == nil {
		// Built once per reconciler instance and reused across every discovery
		// call — see discoveryClientOnce's doc comment for why a fresh client per
		// call is a resource leak, not just wasted allocation.
		r.discoveryClientOnce.Do(func() { r.discoveryClient = newDiscoveryHTTPClient() })
		httpClient = r.discoveryClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %q: %w", discoveryURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery %q returned status %d", discoveryURL, resp.StatusCode)
	}

	// Bound the read so a hostile or misconfigured endpoint cannot stream an
	// unbounded body into memory; provider metadata is small.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read discovery body from %q: %w", discoveryURL, err)
	}

	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse discovery document from %q: %w", discoveryURL, err)
	}

	// The document is untrusted until its issuer matches the one we asked for; a
	// single trailing slash on either side is not significant.
	if strings.TrimSuffix(doc.Issuer, "/") != normalizedIssuer {
		return "", fmt.Errorf("discovery document from %q has issuer %q, want %q", discoveryURL, doc.Issuer, normalizedIssuer)
	}

	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery document from %q has no jwks_uri", discoveryURL)
	}

	// Enforce the same https guarantee the CRD applies to a pinned jwkCertURL.
	u, err := url.Parse(doc.JWKSURI)
	if err != nil {
		return "", fmt.Errorf("discovery document from %q has malformed jwks_uri %q: %w", discoveryURL, doc.JWKSURI, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("discovery document from %q has non-https jwks_uri %q", discoveryURL, doc.JWKSURI)
	}

	return doc.JWKSURI, nil
}

// hashEntry is one referenced Secret's contribution to the rollout hash: its
// resourceVersion, not its data (see referencedSecretData for why). present
// distinguishes a missing Secret (hashed as an absent discriminator, see
// computeConfigHash) from an existing one, so a Secret appearing later still
// changes the hash and rolls the pods.
type hashEntry struct {
	id      string
	present bool
	value   []byte
}

// referencedSecretData reads the resourceVersion of each Secret the API
// actually consumes and returns them as hash entries. It reads only what the
// current spec references: the database Secret always, the TLS Secret when
// spec.api.tls is set, and the JWKS Secret when spec.api.auth.jwkCertSecretRef
// is set. A missing Secret is not an error here (see Reconcile); it is
// recorded as absent.
//
// The hash covers resourceVersion, not Secret data. Hashing the actual
// credential bytes was the original design, but it turns the pod-template
// annotation — readable by anyone who can read the Deployment or its Pods, a
// much wider audience than Secret readers — into an offline oracle for
// low-entropy credentials (e.g. a weak database password): with the rendered
// config.yaml and the other connection fields typically knowable, the
// password becomes the only unknown in an otherwise-known SHA-256 preimage.
// resourceVersion changes on every write, including a rotation, which is all
// the rollout needs, and it means the operator never needs to read Secret
// payloads at all — only their metadata.
func (r *HyperFleetConfigReconciler) referencedSecretData(ctx context.Context, cr *hyperfleetv1alpha1.HyperFleetConfig) ([]hashEntry, error) {
	// getEntry returns the named Secret's resourceVersion as a hash entry.
	// NotFound is reported as present=false with no error; any other error is
	// returned.
	getEntry := func(role, secretName string) (hashEntry, error) {
		s := &corev1.Secret{}
		key := types.NamespacedName{Name: secretName, Namespace: r.OperatorNamespace}
		if err := r.Get(ctx, key, s); err != nil {
			if apierrors.IsNotFound(err) {
				return hashEntry{id: role}, nil
			}
			return hashEntry{}, fmt.Errorf("get secret %q: %w", secretName, err)
		}
		return hashEntry{id: role, present: true, value: []byte(s.ResourceVersion)}, nil
	}

	var entries []hashEntry

	dbEntry, err := getEntry("database", cr.Spec.API.Database.SecretRef.Name)
	if err != nil {
		return nil, err
	}
	entries = append(entries, dbEntry)

	if cr.Spec.API.TLS != nil {
		tlsEntry, err := getEntry("tls", cr.Spec.API.TLS.SecretRef.Name)
		if err != nil {
			return nil, err
		}
		entries = append(entries, tlsEntry)
	}

	if apicomponent.AuthEnabled(cr) && cr.Spec.API.Auth.JWKCertSecretRef != nil {
		jwksEntry, err := getEntry("jwks", cr.Spec.API.Auth.JWKCertSecretRef.Name)
		if err != nil {
			return nil, err
		}
		entries = append(entries, jwksEntry)
	}

	return entries, nil
}

// computeConfigHash returns a stable SHA-256 over the rendered config.yaml and
// the referenced-secret entries. Entries are sorted by id and every field is
// length-delimited with a NUL separator so distinct inputs cannot collide by
// concatenation (e.g. "ab"+"c" vs "a"+"bc").
func computeConfigHash(configYAML string, entries []hashEntry) string {
	sorted := make([]hashEntry, len(entries))
	copy(sorted, entries)
	slices.SortFunc(sorted, func(a, b hashEntry) int { return strings.Compare(a.id, b.id) })

	h := sha256.New()
	writeField := func(b []byte) {
		// Length-prefix each field so boundaries are unambiguous.
		_, _ = io.WriteString(h, fmt.Sprintf("%d:", len(b)))
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}

	writeField([]byte(configYAML))
	for _, e := range sorted {
		writeField([]byte(e.id))
		// A one-byte present/absent discriminator precedes the value so a missing
		// datum can never collide with a present value that happens to equal the
		// absent marker's bytes. Absent writes only the 0x00 tag; present writes
		// 0x01 followed by the length-delimited value.
		if e.present {
			_, _ = h.Write([]byte{1})
			writeField(e.value)
		} else {
			_, _ = h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stampConfigHash computes the rollout hash from the component's rendered
// ConfigMap plus the referenced-secret entries and writes it onto the component's
// Deployment pod-template annotations. It matches operands by the API component's
// well-known names, so for a component without that ConfigMap+Deployment pair it
// is a no-op beyond returning the (ConfigMap-less) hash. Mutating the Deployment
// in place is safe: the object was just rendered and has not yet been applied.
// The returned hash is also folded into the applied-config metric (see
// hashConfig) so a Secret rotation or resolved-value drift shows up there too,
// not just as a pod-template rollout.
func stampConfigHash(objs []client.Object, entries []hashEntry) string {
	var configYAML string
	for _, o := range objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == apicomponent.ConfigMapName {
			configYAML = cm.Data[apicomponent.ConfigFileKey]
			break
		}
	}

	hash := computeConfigHash(configYAML, entries)

	for _, o := range objs {
		dep, ok := o.(*appsv1.Deployment)
		if !ok || dep.Name != apicomponent.ResourceName {
			continue
		}
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations[configHashAnnotation] = hash
	}
	return hash
}

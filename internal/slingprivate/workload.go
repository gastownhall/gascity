package slingprivate

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
)

// SPIFFEWorkload extracts the workload identity an assertion must name from a
// request's verified mTLS peer chain: the single SPIFFE URI SAN on the leaf
// certificate.
//
// It reads VerifiedChains, never PeerCertificates. PeerCertificates is whatever
// the client sent, verified or not; on a listener configured with
// tls.RequireAndVerifyClientCert the two agree, but reading the verified chain
// means a misconfigured listener produces no identity — and therefore a
// rejection — instead of trusting an unvalidated certificate.
//
// A leaf carrying zero or more than one URI SAN yields no identity. One SPIFFE
// ID per workload is the invariant the assertion binding depends on: a
// multi-SAN certificate would let the holder choose which identity to present.
func SPIFFEWorkload(r *http.Request) (string, bool) {
	if r.TLS == nil || !r.TLS.HandshakeComplete {
		return "", false
	}
	leaf := verifiedLeaf(r.TLS)
	if leaf == nil {
		return "", false
	}
	if len(leaf.URIs) != 1 {
		return "", false
	}
	id := leaf.URIs[0]
	if id == nil || id.Scheme != "spiffe" || id.Host == "" || id.Path == "" {
		return "", false
	}
	return canonicalSPIFFE(id), true
}

func verifiedLeaf(state *tls.ConnectionState) *x509.Certificate {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return nil
	}
	return state.VerifiedChains[0][0]
}

// canonicalSPIFFE renders the SPIFFE ID in the exact form a minter stamps:
// scheme and host lowercased (both are case-insensitive in a URI, so comparing
// them raw would make two spellings of one identity unequal), path byte-for-byte
// (it is case-sensitive and identity-bearing).
func canonicalSPIFFE(id *url.URL) string {
	return "spiffe://" + lower(id.Host) + id.Path
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

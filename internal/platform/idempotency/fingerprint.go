package idempotency

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
)

// fingerprintVersion is mixed into every fingerprint so that a future change to the
// canonicalization or the field set does not silently collide with the previous scheme's
// hashes. On a change, every stored record fingerprints differently, which means live
// idempotency records stop matching — so a bump is a migration, and the version string is here
// to make that decision explicit rather than accidental.
const fingerprintVersion = "pp.idem.fingerprint.v1"

// Fingerprint is SHA-256 over the JCS-canonicalized request body plus the scope tuple
// (baseline §14.2).
//
// # Why the scope is in the hash as well as in the key
//
// The stored key already scopes the record to (tenant, merchant, method, path, key). Hashing
// the scope in as well costs nothing and means the fingerprint alone is a complete statement of
// "this exact operation": a fingerprint copied between rows by a bad migration, or compared
// across scopes by a future caller, cannot accidentally match. Fields are length-prefixed
// before hashing so that the boundary between them is unambiguous — without it, tenant "ab"
// with merchant "c" and tenant "a" with merchant "bc" would hash identically, which is a small
// but real cross-tenant collision.
//
// # Non-JSON and unparseable bodies
//
// A body that is not a single well-formed JSON document is hashed raw, under a different domain
// tag so it can never collide with a canonicalized one. This is deliberate rather than an
// error return: by the time a request reaches idempotency it has passed the media-type check,
// so an unparseable body is either an empty body on an endpoint that takes none, or a malformed
// one that the request parser is about to reject anyway. Hashing it raw keeps this function
// total, and a total function is one that cannot be the reason a request fails.
func Fingerprint(body []byte, key ports.IdempotencyKey) string {
	h := sha256.New()
	writeField(h, []byte(fingerprintVersion))
	writeField(h, []byte(key.TenantID))
	writeField(h, []byte(key.MerchantID))
	writeField(h, []byte(key.Method))
	writeField(h, []byte(key.PathTemplate))
	writeField(h, []byte(key.Key))

	if canonical, err := Canonicalize(body); err == nil {
		writeField(h, []byte("jcs"))
		writeField(h, canonical)
	} else {
		writeField(h, []byte("raw"))
		writeField(h, body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeField(h hash.Hash, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}

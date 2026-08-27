package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Small conversions between the domain's "zero value means absent" convention and the schema's
// NOT NULL columns with format CHECKs.
//
// They exist as named functions rather than inline expressions for one reason: each of them
// encodes a decision about what an empty domain value means in the database, and an inline
// `if s == "" { s = "XX" }` at nine call sites is nine independent decisions that will
// eventually disagree.

// nullIfEmpty maps the empty string to SQL NULL.
//
// It is used for columns whose uniqueness is partial — merchants.external_reference is
// `UNIQUE (tenant_id, external_reference) WHERE external_reference IS NOT NULL`. Storing empty
// strings there instead of NULL would make every merchant without an external reference collide
// with every other one.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// orTwoLetter returns a country code the schema's `^[A-Z]{2}$` CHECK will accept.
//
// "XX" is the ISO 3166-1 user-assigned code for "unknown", so an absent country is stored as an
// explicit unknown rather than as an empty string that the CHECK would reject and that a reader
// would have to guess the meaning of.
func orTwoLetter(s string) string {
	if len(s) != 2 {
		return "XX"
	}
	return s
}

// orFourDigit returns a merchant category code the schema's `^[0-9]{4}$` CHECK will accept.
// "0000" is not a real MCC, which is the point: it reads as "not yet supplied" rather than as a
// category the merchant might actually be in.
func orFourDigit(s string) string {
	if len(s) != 4 {
		return "0000"
	}
	return s
}

// orCurrency substitutes a placeholder for the zero Currency.
//
// The zero money.Money is deliberately in the empty currency so that an accidentally
// zero-valued amount fails validation rather than silently behaving as USD 0. The schema's
// CHAR(3) column cannot hold the empty string, so an absent amount is written as XXX — ISO
// 4217's own code for "no currency", which is exactly what it means here.
func orCurrency(c money.Currency) string {
	if len(c) != 3 {
		return "XXX"
	}
	return string(c)
}

// safeMoney rebuilds a Money from a persisted amount and currency, degrading to a zero value in
// the placeholder currency rather than failing.
//
// This is deliberately lenient, and only for the *descriptive* amounts — a merchant's declared
// monthly volume and average ticket. Those are risk-assessment inputs, not money that moves, and
// refusing to load a merchant because their declared volume was recorded in a currency that has
// since left the supported set would take a live merchant offline for a bookkeeping reason.
// Amounts on the money path use money.MustNew against a validated currency and fail loudly.
func safeMoney(amount int64, currency string) money.Money {
	c := money.Currency(currency)
	if !c.IsSupported() {
		return money.Money{}
	}
	return money.MustNew(amount, c)
}

// unmarshalJSON decodes an aggregated child array, reporting a readable error.
//
// A JSON decode failure here means a column contains something the query did not build, which is
// a schema-versus-code mismatch rather than bad input — hence INTERNAL_ERROR, with the subject
// named so the log line says which aggregate could not be assembled.
func unmarshalJSON(raw []byte, dst any, subject string) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return apierror.Wrapf(err, apierror.CodeInternalError, "postgres: unreadable %s", subject)
	}
	return nil
}

// hexDigest returns the hex-encoded SHA-256 of s.
//
// It is used only to derive stable surrogate identifiers from natural keys, so that two
// concurrent first-writes for the same key target the same row and contend on an ON CONFLICT
// rather than inserting two rows a unique index then rejects. It authenticates nothing.
func hexDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

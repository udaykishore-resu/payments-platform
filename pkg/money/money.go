// Package money implements the platform's monetary value object.
//
// The single most important rule in this package: money is an integer number of minor units
// plus a currency, and there is no floating-point anywhere. float64 cannot represent 0.10; a
// payment platform that stores amounts as floats will, given enough volume, be off by cents,
// and being off by cents in a ledger is a reconciliation incident rather than a rounding
// curiosity. See docs/spec/00-design-baseline.md §7 and ADR-018.
//
// Depends only on the standard library, so the domain layer may import it.
package money

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Errors returned by this package. They are sentinel values so callers can branch with
// errors.Is rather than string matching.
var (
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrUnknownCurrency  = errors.New("money: unknown currency code")
	ErrOverflow         = errors.New("money: arithmetic overflow")
	ErrNegativeAllocate = errors.New("money: cannot allocate with non-positive ratios")
	ErrDivideByZero     = errors.New("money: division by zero")
	ErrMalformed        = errors.New("money: malformed representation")
)

// Currency is an ISO 4217 alphabetic code, uppercase.
type Currency string

// currencyInfo carries the exponent (number of minor units per major unit, as a power of ten)
// and the numeric code. The exponent is the only field the arithmetic needs; the numeric code
// is carried because some gateway APIs and settlement files use it.
type currencyInfo struct {
	Exponent int
	Numeric  string
	Name     string
}

// currencies is the supported ISO 4217 set. This is deliberately a curated list rather than
// the full ISO table: an unlisted currency is a configuration error we want to surface at
// validation time, not a value we want to silently accept with a guessed exponent.
//
// Note the exponents that catch people out: JPY and KRW have none, JOD/KWD/BHD/OMR/TND have
// three, and CLF has four. Code that assumes "cents" is wrong for roughly a quarter of the
// world's payment volume.
var currencies = map[Currency]currencyInfo{
	"AED": {2, "784", "UAE Dirham"},
	"AUD": {2, "036", "Australian Dollar"},
	"BHD": {3, "048", "Bahraini Dinar"},
	"BRL": {2, "986", "Brazilian Real"},
	"CAD": {2, "124", "Canadian Dollar"},
	"CHF": {2, "756", "Swiss Franc"},
	"CLF": {4, "990", "Unidad de Fomento"},
	"CLP": {0, "152", "Chilean Peso"},
	"CNY": {2, "156", "Yuan Renminbi"},
	"COP": {2, "170", "Colombian Peso"},
	"CZK": {2, "203", "Czech Koruna"},
	"DKK": {2, "208", "Danish Krone"},
	"EUR": {2, "978", "Euro"},
	"GBP": {2, "826", "Pound Sterling"},
	"HKD": {2, "344", "Hong Kong Dollar"},
	"HUF": {2, "348", "Forint"},
	"IDR": {2, "360", "Rupiah"},
	"ILS": {2, "376", "New Israeli Sheqel"},
	"INR": {2, "356", "Indian Rupee"},
	"ISK": {0, "352", "Iceland Krona"},
	"JOD": {3, "400", "Jordanian Dinar"},
	"JPY": {0, "392", "Yen"},
	"KRW": {0, "410", "Won"},
	"KWD": {3, "414", "Kuwaiti Dinar"},
	"MXN": {2, "484", "Mexican Peso"},
	"MYR": {2, "458", "Malaysian Ringgit"},
	"NOK": {2, "578", "Norwegian Krone"},
	"NZD": {2, "554", "New Zealand Dollar"},
	"OMR": {3, "512", "Rial Omani"},
	"PHP": {2, "608", "Philippine Peso"},
	"PLN": {2, "985", "Zloty"},
	"RON": {2, "946", "Romanian Leu"},
	"SAR": {2, "682", "Saudi Riyal"},
	"SEK": {2, "752", "Swedish Krona"},
	"SGD": {2, "702", "Singapore Dollar"},
	"THB": {2, "764", "Baht"},
	"TND": {3, "788", "Tunisian Dinar"},
	"TRY": {2, "949", "Turkish Lira"},
	"TWD": {2, "901", "New Taiwan Dollar"},
	"USD": {2, "840", "US Dollar"},
	"VND": {0, "704", "Dong"},
	"ZAR": {2, "710", "Rand"},
}

// ParseCurrency validates and normalises a currency code.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(s)))
	if _, ok := currencies[c]; !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownCurrency, s)
	}
	return c, nil
}

// IsSupported reports whether the currency is in the supported set.
func (c Currency) IsSupported() bool { _, ok := currencies[c]; return ok }

// Exponent returns the number of decimal places the currency uses. It returns 2 for unknown
// currencies, but callers should validate with IsSupported first; the fallback exists so that
// a formatting call in a log line cannot panic.
func (c Currency) Exponent() int {
	if info, ok := currencies[c]; ok {
		return info.Exponent
	}
	return 2
}

// Numeric returns the ISO 4217 numeric code, or "" if unknown.
func (c Currency) Numeric() string { return currencies[c].Numeric }

// String satisfies fmt.Stringer.
func (c Currency) String() string { return string(c) }

// SupportedCurrencies returns the sorted list of supported codes. Used by the control plane to
// validate merchant configuration and by the OpenAPI enum generator.
func SupportedCurrencies() []Currency {
	out := make([]Currency, 0, len(currencies))
	for c := range currencies {
		out = append(out, c)
	}
	// insertion sort: the list is small and this avoids importing sort into a hot package
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Money is an immutable amount in minor units of a currency.
//
// The zero value is zero in the empty currency, which is deliberately not a valid currency:
// an accidentally zero-valued Money will fail validation rather than silently behave as USD 0.
type Money struct {
	amount   int64
	currency Currency
}

// New constructs a Money from minor units. It returns an error for an unsupported currency so
// that an unknown code cannot enter the system at any boundary.
func New(minorUnits int64, c Currency) (Money, error) {
	if !c.IsSupported() {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, c)
	}
	return Money{amount: minorUnits, currency: c}, nil
}

// MustNew is New for constants and tests; it panics on an unsupported currency.
func MustNew(minorUnits int64, c Currency) Money {
	m, err := New(minorUnits, c)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero returns a zero amount in the given currency.
func Zero(c Currency) Money { return Money{amount: 0, currency: c} }

// Amount returns the amount in minor units.
func (m Money) Amount() int64 { return m.amount }

// Currency returns the currency.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.amount == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (m Money) IsPositive() bool { return m.amount > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (m Money) IsNegative() bool { return m.amount < 0 }

// IsValid reports whether the Money carries a supported currency.
func (m Money) IsValid() bool { return m.currency.IsSupported() }

// Add returns m+other, or ErrCurrencyMismatch if the currencies differ.
//
// Mixing currencies is a bug, never an implicit conversion. There is no exchange rate in this
// package on purpose: FX is a business decision with a rate source, a spread and an audit
// trail, and it does not belong in an arithmetic primitive.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	sum := m.amount + other.amount
	// Overflow detection for signed addition: the sign flipped in a way addition cannot.
	if (m.amount > 0 && other.amount > 0 && sum < 0) || (m.amount < 0 && other.amount < 0 && sum >= 0) {
		return Money{}, ErrOverflow
	}
	return Money{amount: sum, currency: m.currency}, nil
}

// Sub returns m-other, or ErrCurrencyMismatch if the currencies differ.
func (m Money) Sub(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	diff := m.amount - other.amount
	if (m.amount >= 0 && other.amount < 0 && diff < 0) || (m.amount < 0 && other.amount > 0 && diff >= 0) {
		return Money{}, ErrOverflow
	}
	return Money{amount: diff, currency: m.currency}, nil
}

// Mul multiplies by an integer factor. Used for quantity, never for rates.
func (m Money) Mul(factor int64) (Money, error) {
	if factor != 0 && (m.amount > math.MaxInt64/absInt64(factor) || m.amount < math.MinInt64/absInt64(factor)) {
		return Money{}, ErrOverflow
	}
	return Money{amount: m.amount * factor, currency: m.currency}, nil
}

// MulBasisPoints multiplies by bp/10000, rounding half away from zero.
//
// Basis points, not floats: fees, interchange and FX spreads are all quoted in basis points,
// and doing the arithmetic in integers keeps the result exactly reproducible on both sides of
// a reconciliation.
func (m Money) MulBasisPoints(bp int64) (Money, error) {
	prod := m.amount * bp
	if m.amount != 0 && prod/m.amount != bp {
		return Money{}, ErrOverflow
	}
	q, r := prod/10000, prod%10000
	if r >= 5000 {
		q++
	} else if r <= -5000 {
		q--
	}
	return Money{amount: q, currency: m.currency}, nil
}

// Neg returns -m.
func (m Money) Neg() Money { return Money{amount: -m.amount, currency: m.currency} }

// Abs returns |m|.
func (m Money) Abs() Money {
	if m.amount < 0 {
		return m.Neg()
	}
	return m
}

// Cmp compares m and other, returning -1, 0 or +1. It returns an error on currency mismatch
// rather than an arbitrary ordering, because "is this refund larger than the capture" must
// never quietly answer yes across currencies.
func (m Money) Cmp(other Money) (int, error) {
	if m.currency != other.currency {
		return 0, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	switch {
	case m.amount < other.amount:
		return -1, nil
	case m.amount > other.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

// GreaterThan reports whether m > other. Currency mismatch reports false and an error.
func (m Money) GreaterThan(other Money) (bool, error) {
	c, err := m.Cmp(other)
	return c > 0, err
}

// LessThan reports whether m < other.
func (m Money) LessThan(other Money) (bool, error) {
	c, err := m.Cmp(other)
	return c < 0, err
}

// Equal reports whether m and other are the same amount in the same currency. Unlike Cmp it
// does not error: two Moneys in different currencies are simply not equal.
func (m Money) Equal(other Money) bool {
	return m.amount == other.amount && m.currency == other.currency
}

// Allocate splits m into len(ratios) parts in proportion to ratios, distributing the remainder
// by the largest-remainder method so that the parts always sum exactly to m.
//
// This is the function that stops a three-way split of USD 10.00 producing 3.33 + 3.33 + 3.33
// and losing a cent. Any place the platform divides money — multi-capture, fee allocation,
// split settlement — goes through here.
func (m Money) Allocate(ratios []int64) ([]Money, error) {
	if len(ratios) == 0 {
		return nil, ErrNegativeAllocate
	}
	var total int64
	for _, r := range ratios {
		if r < 0 {
			return nil, ErrNegativeAllocate
		}
		total += r
	}
	if total == 0 {
		return nil, ErrDivideByZero
	}

	parts := make([]Money, len(ratios))
	remainders := make([]int64, len(ratios))
	var allocated int64
	for i, r := range ratios {
		prod := m.amount * r
		share := prod / total
		parts[i] = Money{amount: share, currency: m.currency}
		remainders[i] = prod - share*total // scaled remainder; comparing these is exact
		allocated += share
	}

	// Distribute the leftover minor units one at a time to the largest remainders. Ties go to
	// the lowest index, which makes the result deterministic and therefore testable and
	// reproducible across a reconciliation.
	leftover := m.amount - allocated
	step := int64(1)
	if leftover < 0 {
		step = -1
		leftover = -leftover
	}
	for ; leftover > 0; leftover-- {
		best := -1
		var bestRem int64
		for i := range parts {
			if remainders[i] == 0 && step > 0 {
				continue
			}
			if best == -1 || (step > 0 && remainders[i] > bestRem) || (step < 0 && remainders[i] < bestRem) {
				best, bestRem = i, remainders[i]
			}
		}
		if best == -1 {
			best = 0
		}
		parts[best].amount += step
		remainders[best] = 0
	}
	return parts, nil
}

// Split divides m into n equal parts, distributing the remainder to the earliest parts.
func (m Money) Split(n int) ([]Money, error) {
	if n <= 0 {
		return nil, ErrDivideByZero
	}
	ratios := make([]int64, n)
	for i := range ratios {
		ratios[i] = 1
	}
	return m.Allocate(ratios)
}

// String renders the amount for humans and logs, e.g. "USD 10.50", "JPY 1000", "KWD 1.234".
// It is not the wire format; see MarshalJSON.
func (m Money) String() string {
	exp := m.currency.Exponent()
	if exp == 0 {
		return string(m.currency) + " " + strconv.FormatInt(m.amount, 10)
	}
	neg := m.amount < 0
	a := m.amount
	if neg {
		a = -a
	}
	div := int64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	major, minor := a/div, a%div
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s %s%d.%0*d", m.currency, sign, major, exp, minor)
}

// wire is the JSON representation: integer minor units plus the code. Never a decimal string
// and never a float — a float here would reintroduce the exact bug the whole package exists to
// prevent, one serialization hop away from the ledger.
type wire struct {
	Amount   *int64 `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON implements json.Marshaler.
func (m Money) MarshalJSON() ([]byte, error) {
	a := m.amount
	return json.Marshal(wire{Amount: &a, Currency: string(m.currency)})
}

// UnmarshalJSON implements json.Unmarshaler, rejecting a missing amount, a missing currency,
// and an unsupported currency. Rejecting at the unmarshal boundary means no downstream code
// has to re-check.
func (m *Money) UnmarshalJSON(b []byte) error {
	var w wire
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if w.Amount == nil {
		return fmt.Errorf("%w: missing amount", ErrMalformed)
	}
	c, err := ParseCurrency(w.Currency)
	if err != nil {
		return err
	}
	m.amount, m.currency = *w.Amount, c
	return nil
}

// Value implements driver.Valuer so a Money can be written to Postgres. The amount and the
// currency are stored in separate columns in practice; this exists for the composite-type and
// test paths.
func (m Money) Value() (driver.Value, error) { return m.String(), nil }

// Scan implements sql.Scanner for the same reason.
func (m *Money) Scan(src any) error {
	s, ok := src.(string)
	if !ok {
		b, isBytes := src.([]byte)
		if !isBytes {
			return fmt.Errorf("%w: cannot scan %T", ErrMalformed, src)
		}
		s = string(b)
	}
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return fmt.Errorf("%w: %q", ErrMalformed, s)
	}
	c, err := ParseCurrency(parts[0])
	if err != nil {
		return err
	}
	minor, err := ParseMinorUnits(parts[1], c)
	if err != nil {
		return err
	}
	m.amount, m.currency = minor, c
	return nil
}

// ParseMinorUnits converts a decimal string in major units ("10.50") to minor units for the
// given currency, rejecting more decimal places than the currency allows.
//
// This exists for ingesting gateway settlement reports and merchant CSV uploads, which are the
// only places a decimal string legitimately appears. It is not used on the API boundary.
func ParseMinorUnits(s string, c Currency) (int64, error) {
	exp := c.Exponent()
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	if hasFrac && len(fracPart) > exp {
		return 0, fmt.Errorf("%w: %q has more than %d decimal places for %s", ErrMalformed, s, exp, c)
	}
	for len(fracPart) < exp {
		fracPart += "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	var frac int64
	if exp > 0 {
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrMalformed, err)
		}
	}
	mult := int64(1)
	for i := 0; i < exp; i++ {
		mult *= 10
	}
	if whole > (math.MaxInt64-frac)/mult {
		return 0, ErrOverflow
	}
	total := whole*mult + frac
	if neg {
		total = -total
	}
	return total, nil
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Sum adds a series of Money values, requiring all to share a currency. An empty slice
// returns zero in the given currency, which is why the currency is an explicit argument
// rather than inferred: summing an empty list must still produce a well-formed value.
func Sum(c Currency, values ...Money) (Money, error) {
	acc := Zero(c)
	for _, v := range values {
		var err error
		acc, err = acc.Add(v)
		if err != nil {
			return Money{}, err
		}
	}
	return acc, nil
}

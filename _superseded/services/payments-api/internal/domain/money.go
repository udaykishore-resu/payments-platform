package domain

import "fmt"

// Money represents a monetary amount as an integer count of the currency's minor unit (e.g.
// cents for USD), never a floating point number. Floating point arithmetic on money is a classic,
// entirely avoidable class of financial bug (rounding errors compound silently); representing
// amounts as integers makes the whole class of bug unrepresentable.
type Money struct {
	AmountMinor int64  // e.g. 10000 == $100.00 for a 2-decimal currency
	Currency    string // ISO-4217, e.g. "USD"
}

func (m Money) Validate() error {
	if m.AmountMinor <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	if len(m.Currency) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter ISO-4217 code", ErrInvalidRequest)
	}
	return nil
}

func (m Money) Negate() Money {
	return Money{AmountMinor: -m.AmountMinor, Currency: m.Currency}
}

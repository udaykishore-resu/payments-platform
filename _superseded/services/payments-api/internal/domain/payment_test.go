package domain

import "testing"

func TestCreatePaymentInput_Validate(t *testing.T) {
	valid := CreatePaymentInput{
		IdempotencyKey:  "key-1",
		SourceAccountID: "acct-1",
		DestAccountID:   "acct-2",
		Amount:          Money{AmountMinor: 1000, Currency: "USD"},
	}

	t.Run("valid input passes", func(t *testing.T) {
		if err := valid.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing idempotency key", func(t *testing.T) {
		in := valid
		in.IdempotencyKey = ""
		if err := in.Validate(); err == nil {
			t.Fatal("expected error for missing idempotency key")
		}
	})

	t.Run("missing source account", func(t *testing.T) {
		in := valid
		in.SourceAccountID = ""
		if err := in.Validate(); err == nil {
			t.Fatal("expected error for missing source account")
		}
	})

	t.Run("same source and dest account", func(t *testing.T) {
		in := valid
		in.DestAccountID = in.SourceAccountID
		if err := in.Validate(); err == nil {
			t.Fatal("expected error when source == dest account (a payment must move funds between two distinct accounts)")
		}
	})

	t.Run("invalid amount", func(t *testing.T) {
		in := valid
		in.Amount = Money{AmountMinor: 0, Currency: "USD"}
		if err := in.Validate(); err == nil {
			t.Fatal("expected error for invalid amount")
		}
	})
}

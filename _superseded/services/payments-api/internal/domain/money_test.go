package domain

import "testing"

func TestMoney_Validate(t *testing.T) {
	tests := []struct {
		name    string
		money   Money
		wantErr bool
	}{
		{"valid", Money{AmountMinor: 100, Currency: "USD"}, false},
		{"zero amount", Money{AmountMinor: 0, Currency: "USD"}, true},
		{"negative amount", Money{AmountMinor: -100, Currency: "USD"}, true},
		{"short currency", Money{AmountMinor: 100, Currency: "US"}, true},
		{"empty currency", Money{AmountMinor: 100, Currency: ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.money.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMoney_Negate(t *testing.T) {
	m := Money{AmountMinor: 500, Currency: "USD"}
	n := m.Negate()
	if n.AmountMinor != -500 {
		t.Fatalf("expected -500, got %d", n.AmountMinor)
	}
	if n.Currency != "USD" {
		t.Fatalf("expected currency preserved, got %s", n.Currency)
	}
	// Sum of a leg and its negation is always zero — this is the property the DB balance
	// trigger enforces at the ledger level; asserting it here at the Money level too.
	if m.AmountMinor+n.AmountMinor != 0 {
		t.Fatalf("money and its negation must sum to zero")
	}
}

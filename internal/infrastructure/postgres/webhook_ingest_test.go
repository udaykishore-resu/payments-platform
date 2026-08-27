package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestIngestStoreRefusesARecordThatAlreadyNamesATenant is the test the type exists to pass.
//
// The store's whole reason to exist is that it writes without a tenant in context, which every
// other write in this package refuses to do. That exception is only safe while it is confined to
// rows whose tenant is genuinely unknown: the moment a caller can hand it a tenant, it becomes a
// second path to a tenanted row that row-level security never checked.
//
// The refusal happens before the pool is touched, which is why a nil pool is enough to exercise
// it — and that ordering is itself the property being asserted.
func TestIngestStoreRefusesARecordThatAlreadyNamesATenant(t *testing.T) {
	t.Parallel()

	store := NewWebhookIngestStore(nil, shared.SystemClock{})
	_, err := store.Record(context.Background(), ports.InboundWebhook{
		TenantID:       shared.TenantID("ten_2P392VS591CTDZN8Z88EYCD8MR"),
		GatewayID:      shared.GatewayID("simulator"),
		GatewayEventID: "evt_1",
	})
	if err == nil {
		t.Fatal("a record naming a tenant was accepted; the untenanted path must not write tenanted rows")
	}
	if code := apierror.CodeOf(err); code != apierror.CodeTenantMismatch {
		t.Fatalf("code = %s, want %s", code, apierror.CodeTenantMismatch)
	}
	if !strings.Contains(err.Error(), "tenanted repository") {
		t.Fatalf("the error does not tell the caller where the write belongs: %v", err)
	}
}

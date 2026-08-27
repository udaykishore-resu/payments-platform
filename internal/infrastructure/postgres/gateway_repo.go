package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// GatewayRepository is the platform-global gateway registry.
//
// It is the only repository in this package with no tenant field, because pp.gateways is the
// only table with no tenant_id: every tenant sees the same gateway definitions, and a per-tenant
// copy would drift the moment one tenant's descriptor was updated and another's was not. The
// application role holds SELECT and nothing else; Save exists for platformctl, which connects
// as a role that does have the grant.
type GatewayRepository struct {
	q querier
}

var _ ports.GatewayRepository = (*GatewayRepository)(nil)

const selectGateway = `
SELECT gateway_id, display_name, vendor, api_version, base_urls, capabilities, cost_model,
       signature_scheme, status, version, created_at, updated_at
FROM pp.gateways`

// Get loads one gateway descriptor.
func (r *GatewayRepository) Get(ctx context.Context, id shared.GatewayID) (*gateway.Gateway, error) {
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}
	g, err := scanGateway(r.q.QueryRow(ctx, selectGateway+" WHERE gateway_id = $1", id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeGatewayNotConfigured, "gateway", id.String())
		}
		return nil, mapError(err, "get gateway")
	}
	return g, nil
}

// List returns every registered gateway, including deprecated and disabled ones.
//
// Disabled gateways are returned deliberately. Routing filters them out, but reconciliation must
// still be able to resolve an old payment's gateway, and a lookup that returned nothing for a
// disabled gateway would leave those payments permanently unresolvable — which is the opposite
// of what disabling a gateway is supposed to mean.
func (r *GatewayRepository) List(ctx context.Context) ([]*gateway.Gateway, error) {
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx, selectGateway+" ORDER BY gateway_id")
	if err != nil {
		return nil, mapError(err, "list gateways")
	}
	defer rows.Close()
	var out []*gateway.Gateway
	for rows.Next() {
		g, err := scanGateway(rows)
		if err != nil {
			return nil, mapError(err, "list gateways")
		}
		out = append(out, g)
	}
	return out, mapError(rows.Err(), "list gateways")
}

// Save upserts a gateway descriptor under optimistic concurrency.
func (r *GatewayRepository) Save(ctx context.Context, g *gateway.Gateway) error {
	if _, err := tenantOf(ctx); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.gateways (
    gateway_id, display_name, vendor, api_version, base_urls, capabilities, cost_model,
    signature_scheme, status, version, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (gateway_id) DO UPDATE SET
    display_name = EXCLUDED.display_name, vendor = EXCLUDED.vendor,
    api_version = EXCLUDED.api_version, base_urls = EXCLUDED.base_urls,
    capabilities = EXCLUDED.capabilities, cost_model = EXCLUDED.cost_model,
    signature_scheme = EXCLUDED.signature_scheme, status = EXCLUDED.status,
    version = EXCLUDED.version, updated_at = EXCLUDED.updated_at
WHERE pp.gateways.version = EXCLUDED.version - 1`

	urls, err := json.Marshal(stringMapOf(g.BaseURLs()))
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode gateway base URLs")
	}
	caps, err := json.Marshal(capabilitiesToDTO(g.Capabilities()))
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode gateway capabilities")
	}
	cost, err := json.Marshal(costModelToDTO(g.CostModel()))
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode gateway cost model")
	}

	tag, err := r.q.Exec(ctx, q,
		g.ID().String(), g.DisplayName(), g.Vendor(), g.APIVersion(), urls, caps, cost,
		string(g.SignatureScheme()), string(g.Status()), int64(g.Version()),
		g.CreatedAt(), g.UpdatedAt())
	if err != nil {
		return mapError(err, "save gateway")
	}
	if tag.RowsAffected() == 0 {
		return apierror.Newf(apierror.CodeConfigurationVersionConflict,
			"gateway %s was modified concurrently; reload and reapply", g.ID())
	}
	return nil
}

func scanGateway(row scanRow) (*gateway.Gateway, error) {
	var (
		p                    gateway.RehydrateGatewayParams
		id, scheme, status   string
		urlsRaw, capsRaw     []byte
		costRaw              []byte
		version              int64
		createdAt, updatedAt time.Time
	)
	if err := row.Scan(&id, &p.DisplayName, &p.Vendor, &p.APIVersion,
		&urlsRaw, &capsRaw, &costRaw, &scheme, &status, &version,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}

	p.ID = shared.GatewayID(id)
	p.SignatureScheme = gateway.SignatureScheme(scheme)
	p.Status = gateway.Status(status)
	p.Version = shared.Version(version)
	p.CreatedAt, p.UpdatedAt = createdAt, updatedAt

	var urls map[string]string
	if err := unmarshalJSON(urlsRaw, &urls, "gateway base URLs"); err != nil {
		return nil, err
	}
	p.BaseURLs = make(map[shared.Environment]string, len(urls))
	for k, v := range urls {
		p.BaseURLs[shared.Environment(k)] = v
	}

	var dto capabilitiesDTO
	if err := unmarshalJSON(capsRaw, &dto, "gateway capabilities"); err != nil {
		return nil, err
	}
	p.Capabilities = dto.toDomain()

	var cost costModelDTO
	if err := unmarshalJSON(costRaw, &cost, "gateway cost model"); err != nil {
		return nil, err
	}
	model, err := cost.toDomain()
	if err != nil {
		// A malformed price list is a configuration defect, and loading the gateway with an
		// empty one would silently make it look like the cheapest option to the router. Refuse.
		return nil, apierror.Wrapf(err, apierror.CodeInternalError,
			"gateway %s has an invalid cost model", id)
	}
	p.CostModel = model

	return gateway.RehydrateGateway(p)
}

// capabilitiesDTO is the JSON shape of pp.gateways.capabilities.
//
// It is a separate type from gateway.Capabilities rather than a set of struct tags on the domain
// type, and that is the point of an adapter: the domain expresses the refund window as a
// time.Duration because that is what the business rule reasons in, while the stored document
// expresses it in days because that is what a gateway's contract states and what an operator
// editing the registry would write. Tagging the domain type would force one of the two to adopt
// the other's units, and it would put a persistence concern in internal/domain.
type capabilitiesDTO struct {
	Countries  []string `json:"countries"`
	Currencies []string `json:"currencies"`
	Methods    []string `json:"methods"`
	Operations []string `json:"operations"`

	SupportsPartialCapture   bool `json:"supportsPartialCapture"`
	SupportsMultipleCaptures bool `json:"supportsMultipleCaptures"`
	SupportsPartialRefund    bool `json:"supportsPartialRefund"`
	SupportsVoid             bool `json:"supportsVoid"`
	Supports3DS2             bool `json:"supports3DS2"`
	SupportsNetworkTokens    bool `json:"supportsNetworkTokens"`
	SupportsIdempotencyKeys  bool `json:"supportsIdempotencyKeys"`

	MaxRefundWindowDays        int `json:"maxRefundWindowDays"`
	AuthorizationValidityHours int `json:"authorizationValidityHours"`

	MinAmount map[string]int64 `json:"minAmount,omitempty"`
	MaxAmount map[string]int64 `json:"maxAmount,omitempty"`
}

func (d capabilitiesDTO) toDomain() gateway.Capabilities {
	c := gateway.Capabilities{
		SupportsPartialCapture:   d.SupportsPartialCapture,
		SupportsMultipleCaptures: d.SupportsMultipleCaptures,
		SupportsPartialRefund:    d.SupportsPartialRefund,
		SupportsVoid:             d.SupportsVoid,
		Supports3DS2:             d.Supports3DS2,
		SupportsNetworkTokens:    d.SupportsNetworkTokens,
		SupportsIdempotencyKeys:  d.SupportsIdempotencyKeys,
		MaxRefundWindow:          time.Duration(d.MaxRefundWindowDays) * 24 * time.Hour,
		AuthorizationValidity:    time.Duration(d.AuthorizationValidityHours) * time.Hour,
	}
	for _, v := range d.Countries {
		c.Countries = append(c.Countries, shared.Country(v))
	}
	for _, v := range d.Currencies {
		c.Currencies = append(c.Currencies, money.Currency(v))
	}
	for _, v := range d.Methods {
		c.Methods = append(c.Methods, shared.PaymentMethod(v))
	}
	for _, v := range d.Operations {
		c.Operations = append(c.Operations, shared.Operation(v))
	}
	// An amount bound in an unsupported currency is dropped rather than defaulted. Defaulting it
	// to another currency's bound would silently reject legitimate payments, and a missing bound
	// means "unbounded in that direction", which is the safe reading of a descriptor that has
	// nothing to say.
	c.MinAmount = amountMap(d.MinAmount)
	c.MaxAmount = amountMap(d.MaxAmount)
	return c
}

func amountMap(in map[string]int64) map[money.Currency]money.Money {
	if len(in) == 0 {
		return nil
	}
	out := make(map[money.Currency]money.Money, len(in))
	for k, v := range in {
		c := money.Currency(k)
		if !c.IsSupported() {
			continue
		}
		out[c] = money.MustNew(v, c)
	}
	return out
}

func capabilitiesToDTO(c gateway.Capabilities) capabilitiesDTO {
	d := capabilitiesDTO{
		Countries:                stringsOf(c.Countries),
		Currencies:               stringsOf(c.Currencies),
		Methods:                  stringsOf(c.Methods),
		Operations:               stringsOf(c.Operations),
		SupportsPartialCapture:   c.SupportsPartialCapture,
		SupportsMultipleCaptures: c.SupportsMultipleCaptures,
		SupportsPartialRefund:    c.SupportsPartialRefund,
		SupportsVoid:             c.SupportsVoid,
		Supports3DS2:             c.Supports3DS2,
		SupportsNetworkTokens:    c.SupportsNetworkTokens,
		SupportsIdempotencyKeys:  c.SupportsIdempotencyKeys,
		MaxRefundWindowDays:      int(c.MaxRefundWindow / (24 * time.Hour)),
		// Integer division truncates, which for a window expressed in whole days or hours is
		// exact. A gateway whose contract genuinely said "36 hours" would need an hours field of
		// its own rather than a rounded day — and rounding *down* is the conservative direction,
		// so the truncation cannot make the platform believe a window is longer than it is.
		AuthorizationValidityHours: int(c.AuthorizationValidity / time.Hour),
	}
	d.MinAmount = minorUnitsMap(c.MinAmount)
	d.MaxAmount = minorUnitsMap(c.MaxAmount)
	return d
}

func minorUnitsMap(in map[money.Currency]money.Money) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[string(k)] = v.Amount()
	}
	return out
}

// costModelDTO is the JSON shape of pp.gateways.cost_model. Fees are minor units, never decimal
// strings: the estimate the router scores on and the invoice finance reconciles against have to
// agree to the minor unit, and a decimal string round-trips through a parser that can round.
type costModelDTO struct {
	Rates []costRateDTO `json:"rates"`
}

type costRateDTO struct {
	Currency    string `json:"currency"`
	Method      string `json:"method"`
	BasisPoints int64  `json:"basisPoints"`
	FixedFee    int64  `json:"fixedFee"`
}

func (d costModelDTO) toDomain() (gateway.CostModel, error) {
	if len(d.Rates) == 0 {
		return gateway.CostModel{}, nil
	}
	rates := make([]gateway.CostRate, 0, len(d.Rates))
	for _, r := range d.Rates {
		c := money.Currency(r.Currency)
		if !c.IsSupported() {
			continue
		}
		method := shared.PaymentMethod(r.Method)
		if r.Method == "*" {
			// The registry document spells the wildcard "*" because an empty JSON string in a
			// hand-edited document reads as an oversight. The domain spells it as the empty
			// string, so that ParsePaymentMethod — which rejects the empty string — can never
			// produce a wildcard from caller input.
			method = gateway.AnyMethod
		}
		rates = append(rates, gateway.CostRate{
			Currency:    c,
			Method:      method,
			BasisPoints: r.BasisPoints,
			FixedFee:    money.MustNew(r.FixedFee, c),
		})
	}
	return gateway.NewCostModel(rates...)
}

func costModelToDTO(m gateway.CostModel) costModelDTO {
	rates := m.Rates()
	out := costModelDTO{Rates: make([]costRateDTO, 0, len(rates))}
	for _, r := range rates {
		method := string(r.Method)
		if r.Method == gateway.AnyMethod {
			method = "*"
		}
		out.Rates = append(out.Rates, costRateDTO{
			Currency:    string(r.Currency),
			Method:      method,
			BasisPoints: r.BasisPoints,
			FixedFee:    r.FixedFee.Amount(),
		})
	}
	return out
}

func stringMapOf[K ~string](in map[K]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

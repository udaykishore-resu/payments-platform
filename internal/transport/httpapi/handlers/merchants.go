package handlers

import (
	"context"
	"net/http"
	"strings"

	appmerchant "github.com/udaykishore-resu/payments-platform/internal/application/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l2merchant"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// MerchantService is the merchant registry this file exposes.
type MerchantService interface {
	Create(ctx context.Context, cmd appmerchant.CreateCommand) (*merchant.Merchant, error)
	Get(ctx context.Context, tenantID shared.TenantID, id shared.MerchantID) (*merchant.Merchant, error)
	List(ctx context.Context, tenantID shared.TenantID, f ports.MerchantFilter, page ports.Page) ([]*merchant.Merchant, string, error)
	Update(ctx context.Context, cmd appmerchant.UpdateCommand) (*merchant.Merchant, error)
}

func registerMerchants(rt *httpapi.Router, d Deps) {
	h := &merchantHandlers{svc: d.Merchants, baseURL: d.BaseURL}
	rt.Handle(http.MethodPost, httpapi.RouteCreateMerchant, "createMerchant", h.create)
	rt.Handle(http.MethodGet, httpapi.RouteListMerchants, "listMerchants", h.list)
	rt.Handle(http.MethodGet, httpapi.RouteGetMerchant, "getMerchant", h.get)
	rt.Handle(http.MethodPatch, httpapi.RouteUpdateMerchant, "updateMerchant", h.update)
}

type merchantHandlers struct {
	svc     MerchantService
	baseURL string
}

// create implements `createMerchant`.
//
// The tenant is taken from the access token and the request body has no field for one, so the
// contract's "a tenantId supplied in the body is rejected with 403 TENANT_MISMATCH" is enforced
// by the strict decoder's unknown-field rejection rather than by a check somebody could remove.
// That is the stronger form of the same rule.
func (h *merchantHandlers) create(w http.ResponseWriter, r *http.Request) error {
	var req httpapi.CreateMerchantRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	tc, err := tenantctx.FromContext(r.Context())
	if err != nil {
		return err
	}
	profile, err := businessProfileToDomain(req.BusinessProfile)
	if err != nil {
		return err
	}
	m, err := h.svc.Create(r.Context(), appmerchant.CreateCommand{
		TenantID:     tc.TenantID,
		LegalName:    req.BusinessProfile.LegalName,
		DisplayName:  req.DisplayName,
		ExternalRef:  req.ExternalReference,
		Environment:  environmentOf(tc.Environment),
		Profile:      profile,
		BusinessType: businessTypeOf(req.BusinessProfile.EntityType),
		Actor:        actorFrom(r),
	})
	if err != nil {
		return err
	}
	httpapi.SetETagRaw(w, appmerchant.ETag(m))
	httpapi.SetLocation(w, h.baseURL, "/v1/merchants/"+m.ID().String())
	httpapi.WriteJSON(w, r, http.StatusCreated, httpapi.MerchantOf(m))
	return nil
}

// get implements `getMerchant`, with conditional-read support so a control-plane UI polling a
// merchant through onboarding pays for a header exchange rather than a document.
func (h *merchantHandlers) get(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	m, err := h.svc.Get(r.Context(), tc.TenantID, id)
	if err != nil {
		return err
	}
	etag := appmerchant.ETag(m)
	if notModified(w, r, etag) {
		return nil
	}
	httpapi.SetETagRaw(w, etag)
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.MerchantOf(m))
	return nil
}

// list implements `listMerchants`.
func (h *merchantHandlers) list(w http.ResponseWriter, r *http.Request) error {
	page, err := httpapi.DecodePage(r)
	if err != nil {
		return err
	}
	tc, err := tenantctx.FromContext(r.Context())
	if err != nil {
		return err
	}
	f := ports.MerchantFilter{Search: r.URL.Query().Get("externalReference")}
	for _, raw := range r.URL.Query()["status"] {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			st := merchant.Status(part)
			if !st.IsKnown() {
				return apierror.Newf(apierror.CodeValidationFailed,
					"unknown merchant status %q", part).
					WithDetail(apierror.Detail{
						Field: "status", Code: "UNKNOWN_ENUM_VALUE",
						Message: "See the MerchantStatus enum in the API contract.",
						RuleID:  "L1.ENUM_MEMBER",
					})
			}
			f.Statuses = append(f.Statuses, st)
		}
	}
	items, next, err := h.svc.List(r.Context(), tc.TenantID, f,
		ports.Page{Limit: page.Limit, Cursor: page.Cursor})
	if err != nil {
		return err
	}
	out := make([]httpapi.Merchant, 0, len(items))
	for _, m := range items {
		out = append(out, httpapi.MerchantOf(m))
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.PageOf(out, next))
	return nil
}

// update implements `updateMerchant`.
//
// If-Match is required and its absence is 428, not 412. The distinction is operational: 412 says
// "you read a stale version", which sends an engineer looking for a race that did not happen,
// while 428 says "you did not read at all", which is a one-line fix in their client.
//
// Lifecycle state is deliberately not mutable here. It is driven by the onboarding workflow and
// by the dedicated suspend and terminate operations, and a PATCH that could set `status: ACTIVE`
// would be a way to skip certification.
func (h *merchantHandlers) update(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	ifMatch, err := httpapi.RequireIfMatch(r)
	if err != nil {
		return err
	}
	var req httpapi.UpdateMerchantRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	if err := rejectUnappliedUpdates(req); err != nil {
		return err
	}
	m, err := h.svc.Update(r.Context(), appmerchant.UpdateCommand{
		TenantID:   tc.TenantID,
		MerchantID: id,
		IfMatch:    ifMatch,
		Actor:      actorFrom(r),
	})
	if err != nil {
		return err
	}
	httpapi.SetETagRaw(w, appmerchant.ETag(m))
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.MerchantOf(m))
	return nil
}

// rejectUnappliedUpdates refuses a PATCH naming an attribute the application service cannot yet
// apply.
//
// [appmerchant.UpdateCommand] currently exposes only the active configuration version; display
// name, external reference, business profile, bank accounts and principals are declared by the
// contract but have no mutator on the service. The three possible behaviours are: accept and
// silently drop, accept and pretend, or refuse.
//
// Refusing is the only one that is not a lie. A client that PATCHes a display name, receives 200
// with the *old* name in the body, and does not diff the response has shipped a bug that surfaces
// weeks later as "your API ignores updates". A 422 naming the field is a five-minute
// conversation. When the service grows the mutators, this function shrinks; until then it is the
// honest edge of a partially-implemented operation.
func rejectUnappliedUpdates(req httpapi.UpdateMerchantRequest) error {
	var details []apierror.Detail
	add := func(field string) {
		details = append(details, apierror.Detail{
			Field: field, Code: "NOT_YET_MUTABLE",
			Message: "This attribute is declared by the contract but is not yet applied by this operation.",
			RuleID:  "L1.FIELD_SUPPORTED",
		})
	}
	if req.DisplayName != nil {
		add("displayName")
	}
	if req.ExternalReference != nil {
		add("externalReference")
	}
	if req.BusinessProfile != nil {
		add("businessProfile")
	}
	if req.BankAccounts != nil {
		add("bankAccounts")
	}
	if req.Principals != nil {
		add("principals")
	}
	if len(details) == 0 {
		return nil
	}
	return apierror.Newf(apierror.CodeValidationFailed,
		"%d attribute(s) in this request cannot be applied by updateMerchant", len(details)).
		WithDetails(details...)
}

// merchantTarget resolves {merchantId} and the tenant together.
//
// The merchant is *not* re-checked against the principal's scope here: the tenant middleware
// already did that before the handler ran, which is what stops a handler from being the place a
// scope check can be forgotten.
func merchantTarget(r *http.Request) (shared.MerchantID, tenantctx.TenantContext, error) {
	raw, err := pathValue(r, "merchantId")
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	id, err := shared.ParseMerchantID(raw)
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	tc, err := tenantctx.FromContext(r.Context())
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	return id, tc, nil
}

// actorFrom builds the audit actor from the authenticated principal.
//
// The actor's identity is the *token's* subject, never a body field. An audit trail whose actor a
// caller can set is an audit trail that cannot be used as evidence, which is the only reason it
// exists.
func actorFrom(r *http.Request) appmerchant.Actor {
	p := httpapi.Principal(r.Context())
	if p == nil {
		return appmerchant.Actor{}
	}
	return appmerchant.Actor{ID: p.ID, Name: p.Name}
}

// businessProfileToDomain converts the wire profile, validating the fields whose invalidity is
// cheaper to catch here than three layers down.
func businessProfileToDomain(p httpapi.BusinessProfile) (merchant.BusinessProfile, error) {
	country, err := shared.ParseCountry(p.IncorporationCountry)
	if err != nil {
		return merchant.BusinessProfile{}, err
	}
	mcc, err := shared.ParseMCC(p.MCC)
	if err != nil {
		return merchant.BusinessProfile{}, err
	}
	volume, err := p.DeclaredMonthlyVolume.ToDomain()
	if err != nil {
		return merchant.BusinessProfile{}, err
	}
	return merchant.BusinessProfile{
		LegalEntityType:    p.EntityType,
		RegistrationNumber: p.RegistrationNumber,
		// Only the last four digits of a tax identifier are retained. The full value is personal
		// data with no operational use after KYC submission, and a field that is not stored is a
		// field that cannot be breached.
		TaxIDLast4:            lastFourOf(p.TaxID),
		Country:               country,
		WebsiteURL:            p.WebsiteURL,
		SupportEmail:          p.SupportEmail,
		SupportPhone:          p.SupportPhone,
		MCC:                   mcc,
		ExpectedMonthlyVolume: volume,
		ExpectedAverageTicket: money.Zero(volume.Currency()),
	}, nil
}

func lastFourOf(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// businessTypeOf maps the contract's legal-entity enum onto the L2 validator's business type.
//
// The two vocabularies differ because the contract's is a legal-form taxonomy a merchant
// recognises and the validator's is a KYB taxonomy that drives which documents are demanded.
// Mapping rather than merging keeps a change to the KYB model from being a breaking API change.
// TRUST maps to LLC because the KYB requirements — a register extract plus beneficial owners —
// are the same set; GOVERNMENT maps to PUBLIC_BODY, which exists for exactly that case.
func businessTypeOf(entity string) l2merchant.BusinessType {
	switch entity {
	case "SOLE_TRADER":
		return l2merchant.SoleTrader
	case "PARTNERSHIP":
		return l2merchant.Partnership
	case "PUBLIC_LIMITED":
		return l2merchant.Corporation
	case "NON_PROFIT":
		return l2merchant.NonProfit
	case "GOVERNMENT":
		return l2merchant.PublicBody
	default:
		return l2merchant.LLC
	}
}

// environmentOf defaults an unset tenant environment to sandbox.
//
// Defaulting to sandbox rather than production is the safe direction: a merchant created in the
// wrong environment is an inconvenience in one direction and a live merchant nobody meant to
// create in the other.
func environmentOf(e shared.Environment) shared.Environment {
	if e.IsValid() {
		return e
	}
	return shared.EnvironmentSandbox
}

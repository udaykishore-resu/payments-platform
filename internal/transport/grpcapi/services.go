//go:build grpc

// This file is built only after `buf generate` has produced the bindings for
// api/proto/payments/v1. See doc.go for the command, the CI invocation and why the generated code
// is not committed.
//
// It is written against the generated types as buf will name them: package `paymentsv1`, service
// server interfaces `PaymentServiceServer`, `MerchantServiceServer`, `OnboardingServiceServer`,
// `ConfigurationServiceServer` and `GatewayServiceServer`, each with an embedded
// `Unimplemented…Server` for forward compatibility.

package grpcapi

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	paymentsv1 "github.com/udaykishore-resu/payments-platform/api/proto/payments/v1"
	appconfig "github.com/udaykishore-resu/payments-platform/internal/application/config"
	appmerchant "github.com/udaykishore-resu/payments-platform/internal/application/merchant"
	apponboarding "github.com/udaykishore-resu/payments-platform/internal/application/onboarding"
	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	domainconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Services is the set of application services this surface exposes. A nil member means the
// corresponding gRPC service is not registered, which is what lets one harness serve nine
// binaries that each expose a different subset.
type Services struct {
	Payments      PaymentService
	Merchants     MerchantService
	Onboarding    OnboardingService
	Configuration ConfigurationService
	Gateways      GatewayService
}

// PaymentService is the money path, narrowed to what this surface calls. It is the same shape the
// REST handlers declare, and deliberately so: two surfaces calling two different narrowings of one
// service is two sets of assumptions about it.
type PaymentService interface {
	Create(ctx context.Context, cmd apppayment.CreateCommand) (*apppayment.Result, error)
	Get(ctx context.Context, id shared.PaymentID) (*payment.Payment, error)
	List(ctx context.Context, f ports.PaymentFilter, page ports.Page) ([]*payment.Payment, string, error)
	Capture(ctx context.Context, cmd apppayment.CaptureCommand) (*apppayment.Result, error)
	Refund(ctx context.Context, cmd apppayment.RefundCommand) (*apppayment.Result, error)
	Void(ctx context.Context, cmd apppayment.VoidCommand) (*apppayment.Result, error)
}

// MerchantService is the merchant registry.
type MerchantService interface {
	Create(ctx context.Context, cmd appmerchant.CreateCommand) (*merchant.Merchant, error)
	Get(ctx context.Context, tenantID shared.TenantID, id shared.MerchantID) (*merchant.Merchant, error)
	List(ctx context.Context, tenantID shared.TenantID, f ports.MerchantFilter, page ports.Page) ([]*merchant.Merchant, string, error)
}

// OnboardingService is the durable onboarding workflow.
type OnboardingService interface {
	Start(ctx context.Context, cmd apponboarding.StartCommand) (*apponboarding.Case, error)
	Get(ctx context.Context, tenantID shared.TenantID, id shared.WorkflowID) (*apponboarding.Case, error)
	Signal(ctx context.Context, cmd apponboarding.SignalCommand) (*apponboarding.Case, error)
}

// ConfigurationService is the desired-state configuration store.
type ConfigurationService interface {
	GetActive(ctx context.Context, tenantID shared.TenantID, m shared.MerchantID) (*domainconfig.MerchantConfig, error)
	Publish(ctx context.Context, cmd appconfig.PublishCommand) (*domainconfig.MerchantConfig, error)
}

// GatewayService is the read-only gateway catalogue.
type GatewayService interface {
	Get(ctx context.Context, id shared.GatewayID) (*domaingateway.Gateway, error)
	List(ctx context.Context) ([]*domaingateway.Gateway, error)
}

// RegisterServices registers every wired service onto srv.
//
// It lives here rather than in the composition root because it is the one place that must know the
// generated bindings, and confining that knowledge to a build-tagged file is what keeps the
// default build free of them. The composition root calls this and never sees a paymentsv1 type.
func RegisterServices(srv *grpc.Server, s Services) {
	if s.Payments != nil {
		paymentsv1.RegisterPaymentServiceServer(srv, &paymentServer{svc: s.Payments})
	}
	if s.Merchants != nil {
		paymentsv1.RegisterMerchantServiceServer(srv, &merchantServer{svc: s.Merchants})
	}
	if s.Onboarding != nil {
		paymentsv1.RegisterOnboardingServiceServer(srv, &onboardingServer{svc: s.Onboarding})
	}
	if s.Configuration != nil {
		paymentsv1.RegisterConfigurationServiceServer(srv, &configServer{svc: s.Configuration})
	}
	if s.Gateways != nil {
		paymentsv1.RegisterGatewayServiceServer(srv, &gatewayServer{svc: s.Gateways})
	}
}

// paymentServer implements paymentsv1.PaymentServiceServer.
//
// The embedded Unimplemented server is not boilerplate: it is what makes adding an RPC to the
// .proto a non-breaking change for this binary. Without it, a regenerated interface with a new
// method stops compiling here, and the resulting pressure is to implement the method badly rather
// than to deploy the two changes in order.
type paymentServer struct {
	paymentsv1.UnimplementedPaymentServiceServer
	svc PaymentService
}

// CreatePayment implements the money-in RPC.
//
// The status semantics of the REST surface have no analogue here — gRPC has one success code — so
// the ambiguous outcome is carried in the response's state field instead. A caller must branch on
// `state == PROCESSING`, not on the absence of an error: baseline §12.3's rule that no timer may
// fail a payment applies identically on this transport.
func (s *paymentServer) CreatePayment(ctx context.Context,
	req *paymentsv1.CreatePaymentRequest) (*paymentsv1.Payment, error) {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return nil, Status(err)
	}
	amount, err := moneyOf(req.GetAmount())
	if err != nil {
		return nil, Status(err)
	}
	merchantID, err := shared.ParseMerchantID(req.GetMerchantId())
	if err != nil {
		return nil, Status(err)
	}
	method, err := shared.ParsePaymentMethod(req.GetPaymentMethod().String())
	if err != nil {
		return nil, Status(err)
	}
	res, err := s.svc.Create(ctx, apppayment.CreateCommand{
		TenantID:       tc.TenantID,
		MerchantID:     merchantID,
		Amount:         amount,
		Method:         method,
		MethodRef:      payment.PaymentMethodReference{Token: req.GetPaymentMethodReference().GetToken()},
		CaptureMethod:  captureMethodOf(req.GetCaptureMode()),
		StatementRef:   req.GetStatementDescriptor(),
		Reference:      req.GetReference(),
		Metadata:       req.GetMetadata(),
		IdempotencyKey: req.GetIdempotencyKey(),
		CorrelationID:  correlationIDFrom(ctx),
		RequestID:      requestIDFrom(ctx),
	})
	if err != nil {
		return nil, Status(err)
	}
	return paymentProto(res.Payment), nil
}

// GetPayment implements the read.
func (s *paymentServer) GetPayment(ctx context.Context,
	req *paymentsv1.GetPaymentRequest) (*paymentsv1.Payment, error) {
	id, err := shared.ParsePaymentID(req.GetPaymentId())
	if err != nil {
		return nil, Status(err)
	}
	p, err := s.svc.Get(ctx, id)
	if err != nil {
		return nil, Status(err)
	}
	return paymentProto(p), nil
}

// ListPayments implements the cursor-paginated listing.
func (s *paymentServer) ListPayments(ctx context.Context,
	req *paymentsv1.ListPaymentsRequest) (*paymentsv1.ListPaymentsResponse, error) {
	var filter ports.PaymentFilter
	if v := req.GetMerchantId(); v != "" {
		id, err := shared.ParseMerchantID(v)
		if err != nil {
			return nil, Status(err)
		}
		filter.MerchantID = id
	}
	items, next, err := s.svc.List(ctx, filter, ports.Page{
		Limit:  int(req.GetLimit()),
		Cursor: req.GetCursor(),
	})
	if err != nil {
		return nil, Status(err)
	}
	out := &paymentsv1.ListPaymentsResponse{NextCursor: next}
	for _, p := range items {
		out.Payments = append(out.Payments, paymentProto(p))
	}
	return out, nil
}

// CapturePayment converts an authorization hold into a debit.
func (s *paymentServer) CapturePayment(ctx context.Context,
	req *paymentsv1.CapturePaymentRequest) (*paymentsv1.Payment, error) {
	id, tc, err := paymentTarget(ctx, req.GetPaymentId())
	if err != nil {
		return nil, Status(err)
	}
	var amount *money.Money
	if req.GetAmount() != nil {
		v, err := moneyOf(req.GetAmount())
		if err != nil {
			return nil, Status(err)
		}
		amount = &v
	}
	res, err := s.svc.Capture(ctx, apppayment.CaptureCommand{
		TenantID:       tc.TenantID,
		PaymentID:      id,
		Amount:         amount,
		Final:          req.GetIsFinalCapture(),
		IdempotencyKey: req.GetIdempotencyKey(),
		CorrelationID:  correlationIDFrom(ctx),
	})
	if err != nil {
		return nil, Status(err)
	}
	return paymentProto(res.Payment), nil
}

// RefundPayment returns money to the payer.
func (s *paymentServer) RefundPayment(ctx context.Context,
	req *paymentsv1.RefundPaymentRequest) (*paymentsv1.Payment, error) {
	id, tc, err := paymentTarget(ctx, req.GetPaymentId())
	if err != nil {
		return nil, Status(err)
	}
	var amount *money.Money
	if req.GetAmount() != nil {
		v, err := moneyOf(req.GetAmount())
		if err != nil {
			return nil, Status(err)
		}
		amount = &v
	}
	res, err := s.svc.Refund(ctx, apppayment.RefundCommand{
		TenantID:       tc.TenantID,
		PaymentID:      id,
		Amount:         amount,
		Reason:         refundReasonOf(req.GetReason()),
		IdempotencyKey: req.GetIdempotencyKey(),
		CorrelationID:  correlationIDFrom(ctx),
	})
	if err != nil {
		return nil, Status(err)
	}
	return paymentProto(res.Payment), nil
}

// VoidPayment reverses an authorization hold before capture.
func (s *paymentServer) VoidPayment(ctx context.Context,
	req *paymentsv1.VoidPaymentRequest) (*paymentsv1.Payment, error) {
	id, tc, err := paymentTarget(ctx, req.GetPaymentId())
	if err != nil {
		return nil, Status(err)
	}
	res, err := s.svc.Void(ctx, apppayment.VoidCommand{
		TenantID:       tc.TenantID,
		PaymentID:      id,
		IdempotencyKey: req.GetIdempotencyKey(),
		CorrelationID:  correlationIDFrom(ctx),
	})
	if err != nil {
		return nil, Status(err)
	}
	return paymentProto(res.Payment), nil
}

type merchantServer struct {
	paymentsv1.UnimplementedMerchantServiceServer
	svc MerchantService
}

// GetMerchant reads one merchant within the caller's tenant.
func (s *merchantServer) GetMerchant(ctx context.Context,
	req *paymentsv1.GetMerchantRequest) (*paymentsv1.Merchant, error) {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return nil, Status(err)
	}
	id, err := shared.ParseMerchantID(req.GetMerchantId())
	if err != nil {
		return nil, Status(err)
	}
	m, err := s.svc.Get(ctx, tc.TenantID, id)
	if err != nil {
		return nil, Status(err)
	}
	return merchantProto(m), nil
}

// ListMerchants reads a cursor-paginated page of the tenant's merchants.
func (s *merchantServer) ListMerchants(ctx context.Context,
	req *paymentsv1.ListMerchantsRequest) (*paymentsv1.ListMerchantsResponse, error) {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return nil, Status(err)
	}
	items, next, err := s.svc.List(ctx, tc.TenantID, ports.MerchantFilter{},
		ports.Page{Limit: int(req.GetLimit()), Cursor: req.GetCursor()})
	if err != nil {
		return nil, Status(err)
	}
	out := &paymentsv1.ListMerchantsResponse{NextCursor: next}
	for _, m := range items {
		out.Merchants = append(out.Merchants, merchantProto(m))
	}
	return out, nil
}

type onboardingServer struct {
	paymentsv1.UnimplementedOnboardingServiceServer
	svc OnboardingService
}

// GetOnboardingCase reads a workflow instance's current state.
func (s *onboardingServer) GetOnboardingCase(ctx context.Context,
	req *paymentsv1.GetOnboardingCaseRequest) (*paymentsv1.OnboardingCase, error) {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return nil, Status(err)
	}
	id, err := shared.ParseWorkflowID(req.GetWorkflowInstanceId())
	if err != nil {
		return nil, Status(err)
	}
	c, err := s.svc.Get(ctx, tc.TenantID, id)
	if err != nil {
		return nil, Status(err)
	}
	return caseProto(c), nil
}

type configServer struct {
	paymentsv1.UnimplementedConfigurationServiceServer
	svc ConfigurationService
}

// GetActiveConfiguration reads a merchant's live desired-state document.
func (s *configServer) GetActiveConfiguration(ctx context.Context,
	req *paymentsv1.GetActiveConfigurationRequest) (*paymentsv1.ConfigurationVersion, error) {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return nil, Status(err)
	}
	id, err := shared.ParseMerchantID(req.GetMerchantId())
	if err != nil {
		return nil, Status(err)
	}
	c, err := s.svc.GetActive(ctx, tc.TenantID, id)
	if err != nil {
		return nil, Status(err)
	}
	return configProto(c), nil
}

type gatewayServer struct {
	paymentsv1.UnimplementedGatewayServiceServer
	svc GatewayService
}

// ListGateways reads the platform-global gateway catalogue.
func (s *gatewayServer) ListGateways(ctx context.Context,
	_ *paymentsv1.ListGatewaysRequest) (*paymentsv1.ListGatewaysResponse, error) {
	items, err := s.svc.List(ctx)
	if err != nil {
		return nil, Status(err)
	}
	out := &paymentsv1.ListGatewaysResponse{}
	for _, g := range items {
		out.Gateways = append(out.Gateways, gatewayProto(g))
	}
	return out, nil
}

// --- mapping ------------------------------------------------------------------------------------

func paymentTarget(ctx context.Context, raw string) (shared.PaymentID, tenantctx.TenantContext, error) {
	id, err := shared.ParsePaymentID(raw)
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	return id, tc, nil
}

// moneyOf converts the proto money message, rejecting an unsupported currency at the boundary for
// the same reason the REST DTO does: a bad currency is the caller's problem and must say so
// rather than surfacing as an internal error three layers down.
func moneyOf(m *paymentsv1.Money) (money.Money, error) {
	if m == nil {
		return money.Money{}, apierror.New(apierror.CodeAmountInvalid, "an amount is required")
	}
	cur, err := money.ParseCurrency(m.GetCurrency())
	if err != nil {
		return money.Money{}, apierror.Newf(apierror.CodeCurrencyNotSupported,
			"%q is not a supported ISO 4217 currency", m.GetCurrency())
	}
	return money.New(m.GetAmount(), cur)
}

func moneyProto(m money.Money) *paymentsv1.Money {
	return &paymentsv1.Money{Amount: m.Amount(), Currency: string(m.Currency())}
}

func captureMethodOf(mode paymentsv1.CaptureMode) payment.CaptureMethod {
	if mode == paymentsv1.CaptureMode_CAPTURE_MODE_MANUAL {
		return payment.CaptureManual
	}
	return payment.CaptureAutomatic
}

func refundReasonOf(r paymentsv1.RefundReason) payment.RefundReason {
	switch r {
	case paymentsv1.RefundReason_REFUND_REASON_DUPLICATE:
		return payment.RefundReasonDuplicate
	case paymentsv1.RefundReason_REFUND_REASON_FRAUDULENT:
		return payment.RefundReasonFraudulent
	case paymentsv1.RefundReason_REFUND_REASON_REQUESTED_BY_CUSTOMER:
		return payment.RefundReasonRequestedByCustomer
	default:
		return payment.RefundReasonOther
	}
}

// paymentProto renders the aggregate.
//
// It is a separate mapping from the REST DTO rather than a shared one, and that is deliberate:
// the two contracts version independently, and a shared mapper makes a proto field addition into
// a REST field addition by accident.
func paymentProto(p *payment.Payment) *paymentsv1.Payment {
	if p == nil {
		return nil
	}
	out := &paymentsv1.Payment{
		Id:                     p.ID().String(),
		MerchantId:             p.MerchantID().String(),
		State:                  paymentsv1.PaymentState(paymentsv1.PaymentState_value["PAYMENT_STATE_"+string(p.State())]),
		Amount:                 moneyProto(p.Amount()),
		CapturedAmount:         moneyProto(p.CapturedAmount()),
		RefundedAmount:         moneyProto(p.RefundedAmount()),
		PaymentMethod:          paymentsv1.PaymentMethod(paymentsv1.PaymentMethod_value["PAYMENT_METHOD_"+string(p.PaymentMethod())]),
		ReconciliationRequired: p.HasUnresolvedAttempt(),
		Version:                int64(p.Version()),
		CreatedAt:              timestamppb.New(p.CreatedAt()),
		UpdatedAt:              timestamppb.New(p.UpdatedAt()),
	}
	if a := p.LatestAttempt(); a != nil {
		out.CurrentAttemptId = a.ID().String()
	}
	return out
}

func merchantProto(m *merchant.Merchant) *paymentsv1.Merchant {
	if m == nil {
		return nil
	}
	return &paymentsv1.Merchant{
		Id:          m.ID().String(),
		TenantId:    m.TenantID().String(),
		DisplayName: m.DisplayName(),
		Status:      paymentsv1.MerchantStatus(paymentsv1.MerchantStatus_value["MERCHANT_STATUS_"+string(m.Status())]),
		Version:     int64(m.Version()),
		CreatedAt:   timestamppb.New(m.CreatedAt()),
		UpdatedAt:   timestamppb.New(m.UpdatedAt()),
	}
}

func caseProto(c *apponboarding.Case) *paymentsv1.OnboardingCase {
	if c == nil {
		return nil
	}
	return &paymentsv1.OnboardingCase{
		Id:                 c.WorkflowID.String(),
		MerchantId:         c.MerchantID.String(),
		WorkflowInstanceId: c.WorkflowID.String(),
		CurrentStepKey:     c.CurrentStep,
		OpenedAt:           timestamppb.New(c.CreatedAt),
	}
}

func configProto(c *domainconfig.MerchantConfig) *paymentsv1.ConfigurationVersion {
	if c == nil {
		return nil
	}
	return &paymentsv1.ConfigurationVersion{
		MerchantId:  c.MerchantID.String(),
		Version:     int32(c.Version),
		Digest:      c.ETag(),
		PublishedBy: c.CreatedBy,
		PublishedAt: timestamppb.New(c.CreatedAt),
	}
}

func gatewayProto(g *domaingateway.Gateway) *paymentsv1.Gateway {
	if g == nil {
		return nil
	}
	return &paymentsv1.Gateway{
		Id:             g.ID().String(),
		Code:           g.ID().String(),
		DisplayName:    g.DisplayName(),
		Status:         paymentsv1.GatewayStatus(paymentsv1.GatewayStatus_value["GATEWAY_STATUS_"+string(g.Status())]),
		AdapterVersion: g.APIVersion(),
	}
}

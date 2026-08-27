package handlers

import (
	"context"
	"net/http"
	"time"

	appwebhook "github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// WebhookService is the gateway ingress: verify, deduplicate, persist, publish.
type WebhookService interface {
	Ingest(ctx context.Context, req appwebhook.InboundRequest) (*appwebhook.Accepted, error)
}

// MaxWebhookBodyBytes is the ingest ceiling: 1 MiB.
//
// It is four times the general API limit because a gateway's event body is not ours to bound —
// a settlement notification with a hundred line items is a legitimate document — and because a
// gateway whose webhook we reject retries it, then disables the endpoint. Past this size the
// contract's 413 tells the gateway to use its own payload-reference mechanism, which every major
// gateway has.
const MaxWebhookBodyBytes = 1 << 20

func registerWebhooks(rt *httpapi.Router, d Deps) {
	h := &webhookHandlers{svc: d.Webhooks}
	rt.Handle(http.MethodPost, httpapi.RouteReceiveWebhook, "receiveGatewayWebhook", h.receive)
}

type webhookHandlers struct {
	svc WebhookService
}

// receive implements `receiveGatewayWebhook`.
//
// # The 50 ms budget, and why the handler does almost nothing
//
// Verify the signature and the timestamp window, deduplicate on the gateway's event id, persist
// the raw body, publish `webhook.received.v1`, return 202. All interpretation happens
// asynchronously.
//
// The budget is not a performance target, it is a stability control. Every major gateway retries
// a webhook that is slow or fails, with escalating concurrency, and several disable an endpoint
// that stays slow. So a handler that did the interpretation inline would, during exactly the
// incident that made it slow, be handed a multiplying retry storm by the gateway — and would then
// disable itself. Returning in 50 ms means our own degradation never recruits the gateway into
// amplifying it.
//
// # Why a duplicate is 200 and not an error
//
// A gateway that receives an error retries. A duplicate is not an error — it is the gateway doing
// exactly what at-least-once delivery requires — and answering it with 4xx makes the gateway
// retry a message we have already processed, forever. 200 with `duplicate: true` stops the
// retries and tells the truth.
//
// # Why the raw bytes and not a parsed body
//
// The signature is computed over the received octets. Any re-encoding — key reordering,
// whitespace normalisation, a number rendered differently — invalidates it. The body-limit
// middleware buffered the exact bytes; this handler passes those bytes to the verifier and stores
// those bytes, and never a re-serialisation.
func (h *webhookHandlers) receive(w http.ResponseWriter, r *http.Request) error {
	code, err := pathValue(r, "gateway")
	if err != nil {
		return err
	}
	gatewayID, err := shared.ParseGatewayID(code)
	if err != nil {
		return apierror.Wrapf(err, apierror.CodeWebhookUnknownGateway,
			"no gateway is registered under the code %q", code)
	}
	raw := httpapi.RawBody(r.Context())
	if len(raw) == 0 {
		return apierror.New(apierror.CodeValidationFailed, "the webhook body was empty").
			WithDetail(apierror.Detail{
				Code: "EMPTY_BODY", Message: "A gateway event body is required.",
				RuleID: "L1.BODY_REQUIRED",
			})
	}
	if len(raw) > MaxWebhookBodyBytes {
		return apierror.Newf(apierror.CodeRequestTooLarge,
			"webhook body exceeds the %d byte ingest cap", MaxWebhookBodyBytes)
	}

	acc, err := h.svc.Ingest(r.Context(), appwebhook.InboundRequest{
		GatewayID: gatewayID,
		Raw:       raw,
		Headers:   flattenHeaders(r.Header),
	})
	if err != nil {
		return err
	}

	body := httpapi.WebhookAck{
		WebhookID: acc.WebhookID.String(),
		Received:  true,
		Duplicate: acc.Duplicate,
	}
	now := time.Now().UTC()
	body.ReceivedAt = &now

	if acc.Duplicate {
		writeGatewayAck(w, r, http.StatusOK, gatewayID, body)
		return nil
	}
	writeGatewayAck(w, r, http.StatusAccepted, gatewayID, body)
	return nil
}

// adyenAcceptedBody is the literal string Adyen's protocol requires in a webhook acknowledgement.
//
// Adyen does not read the status code alone: its notification protocol requires the response body
// to contain `[accepted]`, and a 200 with any other body is treated as a failure and retried.
// This is the one place on this surface where a foreign protocol's shape wins over our own, and
// it wins because the alternative is Adyen retrying every notification forever and eventually
// disabling the endpoint.
const adyenAcceptedBody = "[accepted]"

// adyenGatewayCode is the catalogue code whose acknowledgement takes the protocol-specific form.
const adyenGatewayCode = "adyen"

// writeGatewayAck renders the acknowledgement in the form the gateway's own protocol requires.
//
// Every other gateway gets the contract's JSON WebhookAck. Adyen gets `[accepted]` as plain text.
// Branching on the gateway here rather than in an adapter is deliberate: this is a property of
// the *HTTP response*, not of the event translation, and pushing it into the anti-corruption
// layer would mean the adapter had to reach back into the response writer.
func writeGatewayAck(w http.ResponseWriter, r *http.Request, status int,
	gatewayID shared.GatewayID, body httpapi.WebhookAck) {
	if gatewayID.String() == adyenGatewayCode {
		h := w.Header()
		h.Set(httpapi.HeaderContentType, "text/plain; charset=utf-8")
		h.Set(httpapi.HeaderCacheControl, "no-store")
		if id := httpapi.RequestID(r.Context()); id != "" {
			h.Set(httpapi.HeaderRequestID, id)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(adyenAcceptedBody))
		return
	}
	httpapi.WriteJSON(w, r, status, body)
}

// flattenHeaders copies the request headers into the map the verifier expects.
//
// Only the first value of each header is taken. A signature header with two values is either a
// misconfigured proxy or an attempt to confuse the verifier into checking one value and accepting
// another; taking the first and ignoring the rest makes the verifier's input unambiguous, and a
// genuine mismatch then fails the signature rather than passing on a technicality.
func flattenHeaders(in http.Header) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// ingestContext is unused by the handler and exists to document a decision: the ingest runs on
// the request context, which carries the request's own deadline.
//
// It deliberately does *not* detach. A gateway that hung up has stopped waiting, and persisting a
// webhook whose sender will retry it in ten seconds anyway is work with no consumer. The
// at-least-once contract is what makes dropping it safe.
var _ = func(ctx context.Context) context.Context { return ctx }

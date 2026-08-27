package simulator

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// PathPrefix is the simulator protocol's URL prefix.
const PathPrefix = "/sim/v1"

// APIKeyHeader is the header the simulator authenticates callers with. It mirrors Adyen's
// `X-API-Key` rather than a bearer token so that an end-to-end run exercises a header-based
// credential path, which is the shape two of the three real adapters use.
const APIKeyHeader = "X-Sim-Api-Key"

// IdempotencyHeader is the simulator's idempotency header.
//
// It is a header rather than a body field because that is where every real gateway puts it, and
// because an adapter that only ever wrote the key into a body would pass a simulator test and then
// fail against all three vendors.
const IdempotencyHeader = "Idempotency-Key"

// MaxRequestBytes caps what the server will read. A simulator with an unbounded reader is a
// convenient way to make a test runner run out of memory when a client loops.
const MaxRequestBytes = 1 << 20

// ServerOptions configures the HTTP face.
type ServerOptions struct {
	// APIKey is the credential the server requires. An empty value disables the check, which is
	// appropriate for a purely local run but is not the default: exercising the credential path is
	// part of what the simulator is for.
	APIKey string
	// Clock is used for response timestamps.
	Clock shared.Clock
}

// Server exposes an Engine over the simulator's HTTP protocol.
//
// It exists so an end-to-end test can point a real adapter, through a real transport, at a real
// socket — which is the only way to exercise connection pooling, deadline propagation and the
// classification of a genuine client timeout. The in-process Engine cannot test any of those,
// because there is no socket to time out on.
type Server struct {
	engine *Engine
	opts   ServerOptions
	mux    *http.ServeMux
}

// NewServer builds the HTTP face. The returned Server is an http.Handler; cmd/gateway-simulator
// serves it, and a test serves it with httptest.NewServer.
func NewServer(engine *Engine, opts ServerOptions) *Server {
	if opts.Clock == nil {
		opts.Clock = shared.SystemClock{}
	}
	s := &Server{engine: engine, opts: opts, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Engine exposes the deterministic core, so a test can drive scenarios and read emitted webhooks
// while the server is running.
func (s *Server) Engine() *Engine { return s.engine }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc(PathPrefix+"/payments", s.handlePayments)
	s.mux.HandleFunc(PathPrefix+"/payments/", s.handlePaymentModification)
	s.mux.HandleFunc(PathPrefix+"/accounts", s.handleAccounts)
	s.mux.HandleFunc(PathPrefix+"/accounts/", s.handleAccountByID)
	s.mux.HandleFunc(PathPrefix+"/webhooks", s.handleWebhooks)
	s.mux.HandleFunc(PathPrefix+"/webhooks/", s.handleWebhookByID)
	s.mux.HandleFunc(PathPrefix+"/ping", s.handlePing)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.opts.APIKey == "" {
		return true
	}
	return r.Header.Get(APIKeyHeader) == s.opts.APIKey
}

func (s *Server) handlePayments(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed, "simulator: the api key was rejected")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.execute(w, r, opAuthorize, "")
	case http.MethodGet:
		s.execute(w, r, opLookup, "")
	default:
		s.writeError(w, http.StatusMethodNotAllowed, apierror.CodeMalformedRequest, "simulator: method not allowed")
	}
}

func (s *Server) handlePaymentModification(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed, "simulator: the api key was rejected")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, PathPrefix+"/payments/")
	ref, action, ok := strings.Cut(rest, "/")
	if !ok {
		if r.Method == http.MethodGet {
			s.execute(w, r, opLookup, rest)
			return
		}
		s.writeError(w, http.StatusNotFound, apierror.CodeMalformedRequest, "simulator: unknown path")
		return
	}
	switch action {
	case "capture":
		s.execute(w, r, opCapture, ref)
	case "refund":
		s.execute(w, r, opRefund, ref)
	case "void":
		s.execute(w, r, opVoid, ref)
	default:
		s.writeError(w, http.StatusNotFound, apierror.CodeMalformedRequest, "simulator: unknown action")
	}
}

// execute is the one place a request becomes a behaviour.
//
// The scenario branches that cannot be expressed in the Engine's return value — holding the
// connection open, answering with a body that is not JSON, answering 500 — are handled here,
// because they are properties of the *transport* rather than of the outcome. That split is
// deliberate: it keeps the Engine's in-process behaviour and the server's over-the-wire behaviour
// describing the same failures through the mechanisms each medium actually has.
func (s *Server) execute(w http.ResponseWriter, r *http.Request, operation, reference string) {
	req, err := s.readRequest(r, operation, reference)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, apierror.CodeMalformedRequest, err.Error())
		return
	}

	amt := money.Money{}
	if req.Amount != nil {
		if m, e := money.New(req.Amount.MinorUnits, money.Currency(strings.ToUpper(req.Amount.Currency))); e == nil {
			amt = m
		}
	}
	switch ResolveScenario(req.Metadata, amt) {
	case ScenarioTimeout:
		// Hold the connection until the client gives up. This is what a wedged gateway does, and it
		// is the only way to produce a genuine client-side timeout for the adapter to classify.
		<-r.Context().Done()
		return
	case ScenarioMalformed:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"AUTHORIZED",`))
		return
	case ScenarioGatewayError:
		s.writeError(w, http.StatusInternalServerError, apierror.CodeGatewayUnavailable,
			"simulator: the gateway failed")
		return
	case ScenarioAuthFailure:
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed,
			"simulator: the credentials were rejected")
		return
	default:
		// Every other scenario produces an ordinary response through the engine below; only the ones
		// above have to subvert the HTTP layer itself to be reproducible
	}

	resp, _, err := s.engine.Execute(r.Context(), *req)
	if err != nil {
		s.writeError(w, statusForError(err), apierror.CodeOf(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) readRequest(r *http.Request, operation, reference string) (*WireRequest, error) {
	req := &WireRequest{Operation: operation, Reference: reference}
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		if ref := q.Get("reference"); ref != "" {
			req.Reference = ref
		}
		req.IdempotencyKey = q.Get("idempotencyKey")
		return req, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes))
	if err != nil {
		return nil, apierror.New(apierror.CodeMalformedRequest, "simulator: the request body could not be read")
	}
	if len(body) > 0 {
		if err := decodeStrict(body, req); err != nil {
			return nil, err
		}
	}
	// The path and the method are authoritative for the operation and the reference. A body that
	// disagrees is a client bug, and letting the body win would let a capture be issued against the
	// authorize URL — the sort of thing that is very hard to see in a trace.
	req.Operation = operation
	if reference != "" {
		req.Reference = reference
	}
	if hdr := r.Header.Get(IdempotencyHeader); hdr != "" {
		req.IdempotencyKey = hdr
	}
	return req, nil
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed, "simulator: the api key was rejected")
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, apierror.CodeMalformedRequest, "simulator: method not allowed")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes))
	var req WireProvisionRequest
	if err := decodeStrict(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, apierror.CodeMalformedRequest, err.Error())
		return
	}
	if hdr := r.Header.Get(IdempotencyHeader); hdr != "" {
		req.IdempotencyKey = hdr
	}
	res, err := s.engine.Provision(r.Context(), spi.ProvisionRequest{
		IdempotencyKey: req.IdempotencyKey,
		MerchantID:     shared.MerchantID(req.MerchantID),
		LegalName:      req.LegalName,
	})
	if err != nil {
		s.writeError(w, statusForError(err), apierror.CodeOf(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, WireProvisionResponse{
		AccountID:           res.ExternalAccountID,
		Status:              res.Status,
		RequiresAction:      res.RequiresAction,
		ActionURL:           res.ActionURL,
		PendingRequirements: res.PendingRequirements,
	})
}

func (s *Server) handleAccountByID(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed, "simulator: the api key was rejected")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, PathPrefix+"/accounts/")
	if r.Method != http.MethodDelete {
		s.writeError(w, http.StatusMethodNotAllowed, apierror.CodeMalformedRequest, "simulator: method not allowed")
		return
	}
	s.engine.mu.Lock()
	_, exists := s.engine.accounts[id]
	s.engine.mu.Unlock()
	if !exists {
		// A 404 on a compensation is the normal case, and the adapter must tolerate it. Answering
		// 404 rather than 204 is what makes that tolerance an exercised path rather than an
		// assertion in a comment.
		s.writeError(w, http.StatusNotFound, apierror.CodePaymentNotFound, "simulator: no such account")
		return
	}
	if err := s.engine.Deprovision(r.Context(), id); err != nil {
		s.writeError(w, statusForError(err), apierror.CodeOf(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed, "simulator: the api key was rejected")
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, apierror.CodeMalformedRequest, "simulator: method not allowed")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes))
	var req WireWebhookRequest
	if err := decodeStrict(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, apierror.CodeMalformedRequest, err.Error())
		return
	}
	if hdr := r.Header.Get(IdempotencyHeader); hdr != "" {
		req.IdempotencyKey = hdr
	}
	reg, err := s.engine.RegisterWebhook(r.Context(), spi.WebhookRegistrationRequest{
		IdempotencyKey:    req.IdempotencyKey,
		ExternalAccountID: req.AccountID,
		URL:               req.URL,
		EventTypes:        req.EventTypes,
	})
	if err != nil {
		s.writeError(w, statusForError(err), apierror.CodeOf(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, WireWebhookResponse{
		RegistrationID: reg.RegistrationID,
		SigningSecret:  reg.SigningSecret,
		URL:            reg.URL,
	})
}

func (s *Server) handleWebhookByID(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed, "simulator: the api key was rejected")
		return
	}
	if r.Method != http.MethodDelete {
		s.writeError(w, http.StatusMethodNotAllowed, apierror.CodeMalformedRequest, "simulator: method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, PathPrefix+"/webhooks/")
	s.engine.mu.Lock()
	_, exists := s.engine.hooks[id]
	s.engine.mu.Unlock()
	if !exists {
		s.writeError(w, http.StatusNotFound, apierror.CodePaymentNotFound, "simulator: no such webhook")
		return
	}
	_ = s.engine.UnregisterWebhook(r.Context(), "", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, http.StatusUnauthorized, apierror.CodeGatewayAuthenticationFailed, "simulator: the api key was rejected")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   s.opts.Clock.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders a failure. The message is the platform's own; nothing from the request is
// echoed, because a simulator that reflected request content would make it possible for a test
// fixture's credential to appear in a response body.
func (s *Server) writeError(w http.ResponseWriter, status int, code apierror.Code, message string) {
	s.writeJSON(w, status, WireError{Code: string(code), Message: message})
}

func statusForError(err error) int {
	return apierror.HTTPStatusOf(err)
}

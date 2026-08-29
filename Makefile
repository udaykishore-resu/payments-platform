# Makefile — the developer entry points.
#
# `make` with no target prints the help. That is deliberate: a Makefile whose default
# target builds something is a Makefile that runs a two-minute build when someone typed
# `make` to find out what it does.
#
# Every target that a human is expected to run carries a `## description` comment on its
# rule line; the `help` target parses those, so the help text cannot drift from the targets
# — a target added without a description simply does not appear, which is a visible bug.
#
# Conventions used throughout:
#   * .PHONY on everything that is not a file, so a directory named `test` cannot make
#     `make test` a no-op;
#   * variables are `?=` so they can be overridden from the environment or the command line
#     (`make build SERVICE=payment-api VERSION=1.4.2`);
#   * no target invokes `go get`, `go mod tidy` or `go env -w`. go.mod is a shared,
#     reviewed artefact; a build that mutates it produces a diff nobody asked for and a
#     dependency change nobody reviewed.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --no-print-directory

# --- identity -------------------------------------------------------------------------------
MODULE      := github.com/udaykishore-resu/payments-platform
BIN_DIR     ?= bin
SERVICES    := payment-api payment-orchestrator control-plane-api webhook-ingress \
               workflow-worker outbox-relay event-consumer gateway-simulator platformctl

# VERSION prefers an annotated tag, falls back to the short SHA, and finally to `dev` in a
# tree with no git at all (a container build from a tarball). It is stamped into the binary
# so that `payment-api -version` in production answers the first question of every incident.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# SOURCE_DATE_EPOCH makes a local build byte-identical to the CI build of the same commit.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --pretty=%ct 2>/dev/null || echo 0)

LDFLAGS := -s -w \
  -X '$(MODULE)/internal/platform/config.Version=$(VERSION)' \
  -X '$(MODULE)/internal/platform/config.Commit=$(COMMIT)' \
  -X '$(MODULE)/internal/platform/config.BuildDate=$(BUILD_DATE)'

GO          ?= go
GOFLAGS     ?=
# -trimpath everywhere, not only in the container build: a stack trace containing
# /home/alice/src/... is a stack trace that says where it was compiled rather than what
# broke, and it makes two builds of one commit differ.
GO_BUILD    := $(GO) build -trimpath -ldflags "$(LDFLAGS)"

DOCKER      ?= docker
IMAGE_REPO  ?= ghcr.io/udaykishore-resu/payments-platform
PLATFORMS   ?= linux/arm64,linux/amd64

# ENV_FILE is the single statement of the local development variable set; every run-* target
# sources it. DSN is read from it too, so the migrate/seed targets and the services cannot
# disagree about which database they are talking to.
ENV_FILE    ?= .env.dev
DSN         ?= $(shell set -a; . ./$(ENV_FILE) >/dev/null 2>&1; set +a; printf '%s' "$${PP_DSN:-}")

# --- help ------------------------------------------------------------------------------------
.PHONY: help
help: ## Show this help
	@printf '\033[1m%s\033[0m\n' "payments-platform — make targets"
	@printf '\n'
	@awk 'BEGIN { FS = ":.*## " } \
	     /^## ---/ { sub(/^## --- ?/, ""); sub(/ ?-*$$/, ""); printf "\n\033[1m%s\033[0m\n", $$0; next } \
	     /^[a-zA-Z0-9_%-]+:.*## / { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' \
	     $(MAKEFILE_LIST)
	@printf '\n'
	@printf '  \033[2mVERSION=%s  COMMIT=%s\033[0m\n' "$(VERSION)" "$(COMMIT)"
	@printf '  \033[2mOverride anything: make build SERVICE=payment-api VERSION=1.4.2\033[0m\n\n'

## --- build ---

.PHONY: build
build: ## Build every service into ./bin
	@mkdir -p $(BIN_DIR)
	@for svc in $(SERVICES); do \
	  if [ -d "./cmd/$$svc" ] && compgen -G "./cmd/$$svc/*.go" >/dev/null; then \
	    printf '  building %s\n' "$$svc"; \
	    CGO_ENABLED=0 $(GO_BUILD) -o "$(BIN_DIR)/$$svc" "./cmd/$$svc"; \
	  else \
	    printf '  \033[33mskipping %s (no sources yet)\033[0m\n' "$$svc"; \
	  fi; \
	done
	@printf '\033[32m✓\033[0m binaries in ./%s\n' "$(BIN_DIR)"

.PHONY: build-one
build-one: ## Build one service: make build-one SERVICE=payment-api
	@test -n "$(SERVICE)" || { echo "SERVICE is required, e.g. make build-one SERVICE=payment-api"; exit 2; }
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO_BUILD) -o "$(BIN_DIR)/$(SERVICE)" "./cmd/$(SERVICE)"

.PHONY: generate
generate: ## Run go generate and fail if it produced a diff
	$(GO) generate ./...
	@# The diff check is the point. `go generate` that is never verified means the
	@# committed generated code and its source drift silently until someone regenerates
	@# and gets a 3000-line diff in an unrelated PR. CI stage 1 runs exactly this.
	@if ! git diff --quiet -- . 2>/dev/null; then \
	  printf '\033[31m✗ go generate produced a diff — commit the regenerated files\033[0m\n'; \
	  git diff --stat -- .; \
	  exit 1; \
	fi
	@printf '\033[32m✓\033[0m generated code is current\n'

.PHONY: clean
clean: ## Remove build output and local test artifacts
	rm -rf $(BIN_DIR) coverage.out coverage.html .loadtest sbom
	$(GO) clean -testcache

## --- test ---

.PHONY: test
test: ## Unit tests, no containers (~20s — the inner loop)
	$(GO) test ./... -short

.PHONY: test-race
test-race: ## Unit tests with the race detector (non-negotiable for the money path)
	@# -count=1 defeats the test cache. A cached PASS from before the change under test is
	@# the most misleading thing a test command can print.
	$(GO) test ./... -race -count=1

.PHONY: test-integration
test-integration: ## Integration tests: testcontainers (postgres, redis, redpanda)
	$(GO) test -tags=integration ./tests/integration/... ./internal/... -count=1 -timeout 20m

.PHONY: test-contract
test-contract: ## Gateway adapter suite + event-schema consumer contracts
	@# NO -tags=contract, deliberately. `tests/contract` documents itself as untagged because it
	@# needs no database, no broker and no running service — it reads the committed schemas and
	@# the adapters' own fixtures — so it belongs in the cheapest CI stage and in `go test ./...`.
	@# The flag was inert (a build tag excludes files, it does not require them), which made this
	@# target look like it selected a suite when it selected nothing, and made the same tests run
	@# twice: once here and once under `make test`. Running them under both is correct and cheap;
	@# pretending they are gated is not.
	$(GO) test ./tests/contract/... ./internal/adapters/... -count=1

.PHONY: test-e2e
test-e2e: ## End-to-end against the local stack (run `make dev-up` first)
	$(GO) test -tags=e2e ./tests/e2e/... -count=1 -timeout 20m

.PHONY: test-chaos
test-chaos: ## Toxiproxy-based chaos subset against the local stack
	$(GO) test -tags=chaos ./tests/chaos/... -count=1 -timeout 30m

.PHONY: test-all
test-all: test-race test-integration test-contract ## Everything except chaos and load

.PHONY: cover
cover: ## Coverage profile, HTML report, and the per-package gates
	$(GO) test ./... -short -covermode=atomic -coverprofile=coverage.out
	$(GO) tool cover -html=coverage.out -o coverage.html
	@$(GO) tool cover -func=coverage.out | tail -1
	@printf '  report: coverage.html\n'
	@# scripts/coverage.sh reuses the profile just produced, so the suite runs once. It fails
	@# on a drop below the per-scope floor and warns on the distance to the testing.md §1.1
	@# target, then checks that every property in tests/critical_paths.yaml still names a test
	@# that exists.
	./scripts/coverage.sh --profile coverage.out

.PHONY: critical-paths
critical-paths: ## Assert every property in tests/critical_paths.yaml still names a real test
	./scripts/coverage.sh --only-paths

.PHONY: check-docs
check-docs: ## Every repo-relative path cited by a document exists
	./scripts/check-doc-references.sh

## --- verify ---

.PHONY: lint
lint: ## golangci-lint over the whole module
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint run ./...; \
	else \
	  printf '  golangci-lint not installed; running it via go run\n'; \
	  $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0 run ./...; \
	fi

.PHONY: fmt
fmt: ## Format the tree with gofmt and goimports
	gofmt -w .
	@if command -v goimports >/dev/null 2>&1; then \
	  goimports -local $(MODULE) -w .; \
	else printf '\033[33m  goimports not installed; import grouping not applied\033[0m\n'; fi

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: verify
verify: ## Everything CI verifies, in CI's order — run this before pushing
	./scripts/verify.sh

.PHONY: verify-fast
verify-fast: ## verify without the race detector (~4 min tier)
	./scripts/verify.sh --fast

.PHONY: check-arch
check-arch: ## The §4 dependency-rule fitness function alone
	./scripts/check-architecture.sh

.PHONY: check-contracts
check-contracts: ## OpenAPI, event schemas and the error catalogue
	./scripts/check-openapi.sh
	./scripts/check-events.sh
	./scripts/check-error-catalog.sh

.PHONY: check-security
check-security: ## Secret scan and dependency licences
	./scripts/check-secrets.sh
	./scripts/check-licences.sh

.PHONY: check-runbooks
check-runbooks: ## Every alert runbook_url resolves, and every paging alert has one
	./scripts/check-runbook-links.sh

.PHONY: traceability
traceability: ## Regenerate docs/spec/09-traceability.md (§26)
	./scripts/traceability.sh

.PHONY: sbom
sbom: ## Generate a CycloneDX SBOM for the source tree
	@mkdir -p sbom
	@if command -v syft >/dev/null 2>&1; then \
	  syft dir:. -o cyclonedx-json=sbom/source.cyclonedx.json; \
	  printf '\033[32m✓\033[0m sbom/source.cyclonedx.json\n'; \
	else \
	  printf '\033[33m  syft is not installed — skipping (CI generates the release SBOM)\033[0m\n'; \
	fi

.PHONY: vuln
vuln: ## govulncheck against the linked dependency graph
	@if command -v govulncheck >/dev/null 2>&1; then govulncheck ./...; \
	else $(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...; fi

## --- run ---

# Every run-* target sources $(ENV_FILE) with PP_SERVICE_NAME already set, and that file's
# per-service block supplies the listener addresses and the variables only that binary needs.
#
# WHY A FILE AND NOT INLINE ASSIGNMENTS
#   Nine binaries share one variable set. Inline, the set drifts: `internal/platform/runtime`
#   marks PP_ENVIRONMENT, PP_REGION, PP_DATABASE_URL, PP_HTTP_ADDR, PP_PUBLIC_BASE_URL, the
#   three PP_AUTH_* variables and PP_MESH_TRUST_DOMAIN `required:"true"` with no defaults, and
#   a target missing one produces a process that exits before it binds a port. Stated once,
#   adding a required variable is one edit rather than nine.
#
#   `set -a` exports every assignment in the file, so the go process inherits them; `exec`
#   replaces the shell so Ctrl-C reaches the service rather than make.
define run_service
@test -f $(ENV_FILE) || { printf '\033[31m✗\033[0m missing $(ENV_FILE) — it is committed; check your checkout\n'; exit 2; }
@printf '  \033[36m%s\033[0m using $(ENV_FILE)\n' "$(1)"
@set -a; PP_SERVICE_NAME=$(1); . ./$(ENV_FILE); set +a; \
  exec $(GO) run ./cmd/$(1)
endef

# ---------------------------------------------------------------------------
# One command
# ---------------------------------------------------------------------------
# `make run` is the whole local path in one target: infrastructure, schema,
# synthetic data, credentials, the payment API, and a payment taken through it.
# It composes the pieces that already exist — dev-up.sh, dev-token.sh, the
# run_service macro — rather than reimplementing them, so there is one way for
# the local stack to be wrong instead of two.
#
# Everything it starts is synthetic: the gateway is the simulator, the issuer is
# the local dev issuer, the data is generated, and the credential is a fixture.

.PHONY: run
run: ## Clean clone to a live payment API on :8080, then take a payment through it
	@command -v docker >/dev/null 2>&1 || { echo "docker is required: https://docs.docker.com/get-docker/"; exit 1; }
	@command -v $(GO) >/dev/null 2>&1 || { echo "go is required: https://go.dev/dl/"; exit 1; }
	@command -v curl >/dev/null 2>&1 || { echo "curl is required"; exit 1; }
	@test -f $(ENV_FILE) || { printf '\033[31m✗\033[0m missing $(ENV_FILE) — it is committed; check your checkout\n'; exit 2; }
	./scripts/dev-up.sh
	@mkdir -p .dev
	@printf '  \033[36mpayment-api\033[0m using $(ENV_FILE) and .dev/secrets.yaml\n'
	@set -a; PP_SERVICE_NAME=payment-api; PP_SECRETS_FILE="$$PWD/.dev/secrets.yaml"; . ./$(ENV_FILE); set +a; \
		$(GO) run ./cmd/payment-api > .dev/payment-api.log 2>&1 & \
		api=$$!; \
		trap "kill $$api 2>/dev/null; exit 0" INT TERM; \
		trap "kill $$api 2>/dev/null" EXIT; \
		n=0; ready=""; \
		while [ $$n -lt 120 ]; do \
			if curl -sf -o /dev/null http://localhost:8081/readyz 2>/dev/null; then ready=yes; break; fi; \
			if ! kill -0 $$api 2>/dev/null; then break; fi; \
			n=$$((n+1)); sleep 0.5; \
		done; \
		if [ -z "$$ready" ]; then \
			echo; echo "  payment-api did not become ready. Last lines of .dev/payment-api.log:"; \
			tail -20 .dev/payment-api.log; exit 1; \
		fi; \
		$(MAKE) --no-print-directory demo; \
		echo "  payment-api is still running:"; \
		echo "    http://localhost:8080   /v1/payments, /healthz, /readyz"; \
		echo "    http://localhost:8081   admin: /livez /readyz /startupz /metrics"; \
		echo "    http://localhost:16686  traces        http://localhost:3000  dashboards"; \
		echo; \
		echo "    make demo             # take another payment"; \
		echo "    make dev-token        # mint a bearer token"; \
		echo "    make run-control-plane-api / run-webhook-ingress / run-workflow-worker"; \
		echo "    make dev-down         # stop the stack and remove its volumes"; \
		echo; \
		echo "  logs: .dev/payment-api.log"; \
		echo "  ctrl-c stops payment-api; the compose stack keeps running."; \
		echo; \
		wait $$api

.PHONY: demo
demo: ## Take one payment through a running stack and show what came back
	@command -v curl >/dev/null 2>&1 || { echo "curl is required"; exit 1; }
	@curl -sf -o /dev/null http://localhost:8080/healthz 2>/dev/null || { \
		echo "nothing on :8080 — run 'make run' first"; exit 1; }
	@test -f .dev/dev-env.sh || { echo "no .dev/dev-env.sh — run 'make run' or ./scripts/dev-up.sh first"; exit 1; }
	@. ./.dev/dev-env.sh; \
		token="$$(./scripts/dev-token.sh)"; \
		echo; \
		echo "1. the payment API is up and authenticating against the local issuer"; \
		printf '   GET /v1/payments -> HTTP '; \
		curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/v1/payments \
			-H "Authorization: Bearer $$token" -H 'Accept: application/json'; \
		echo; \
		echo "2. one payment, authorized and captured through the gateway simulator"; \
		key="$$(uuidgen 2>/dev/null || date +%s%N)"; \
		curl -s -o .dev/last-payment.json -w '   POST /v1/payments -> HTTP %{http_code}\n' \
			-X POST http://localhost:8080/v1/payments \
			-H "Authorization: Bearer $$token" \
			-H 'Content-Type: application/json' \
			-H "Idempotency-Key: $$key" \
			-d "{\"merchantId\":\"$$PP_TEST_MERCHANT_ID\",\"amount\":{\"amount\":1050,\"currency\":\"EUR\"},\"paymentMethod\":\"CARD\",\"paymentMethodReference\":{\"type\":\"GATEWAY_TOKEN\",\"gatewayCode\":\"simulator\",\"token\":\"tok_dev_visa\",\"brand\":\"VISA\",\"last4\":\"4242\",\"expiryMonth\":12,\"expiryYear\":2030},\"captureMode\":\"AUTOMATIC\"}"; \
		printf '   '; cat .dev/last-payment.json; echo; \
		echo; \
		echo "3. the same Idempotency-Key again returns the same payment, not a second one"; \
		curl -s -o .dev/last-payment-replay.json -w '   POST /v1/payments -> HTTP %{http_code}\n' \
			-X POST http://localhost:8080/v1/payments \
			-H "Authorization: Bearer $$token" \
			-H 'Content-Type: application/json' \
			-H "Idempotency-Key: $$key" \
			-d "{\"merchantId\":\"$$PP_TEST_MERCHANT_ID\",\"amount\":{\"amount\":1050,\"currency\":\"EUR\"},\"paymentMethod\":\"CARD\",\"paymentMethodReference\":{\"type\":\"GATEWAY_TOKEN\",\"gatewayCode\":\"simulator\",\"token\":\"tok_dev_visa\",\"brand\":\"VISA\",\"last4\":\"4242\",\"expiryMonth\":12,\"expiryYear\":2030},\"captureMode\":\"AUTOMATIC\"}"; \
		if cmp -s .dev/last-payment.json .dev/last-payment-replay.json; then \
			echo "   identical response — the idempotency record was replayed"; \
		else \
			echo "   responses differ; see .dev/last-payment.json and .dev/last-payment-replay.json"; \
		fi; \
		echo

.PHONY: run-payment-api
run-payment-api: ## Run payment-api against the local stack (:8080, admin :8081)
	$(call run_service,payment-api)

.PHONY: run-payment-orchestrator
run-payment-orchestrator: ## Run payment-orchestrator against the local stack (gRPC :9095, admin :8087)
	$(call run_service,payment-orchestrator)

.PHONY: run-control-plane-api
run-control-plane-api: ## Run control-plane-api against the local stack (:8082, admin :8092)
	$(call run_service,control-plane-api)

.PHONY: run-webhook-ingress
run-webhook-ingress: ## Run webhook-ingress against the local stack (:8083, admin :8093)
	$(call run_service,webhook-ingress)

.PHONY: run-workflow-worker
run-workflow-worker: ## Run workflow-worker against the local stack (admin :8084)
	$(call run_service,workflow-worker)

.PHONY: run-outbox-relay
run-outbox-relay: ## Run outbox-relay (admin :8085) — BLOCKED, see .env.dev's kafka note
	$(call run_service,outbox-relay)

.PHONY: run-event-consumer
run-event-consumer: ## Run event-consumer (admin :8086) — BLOCKED, see .env.dev's kafka note
	$(call run_service,event-consumer)

.PHONY: run-gateway-simulator
run-gateway-simulator: ## Run the gateway simulator (test-only deployable, :8090)
	$(call run_service,gateway-simulator)

.PHONY: run-dev-issuer
run-dev-issuer: ## Run the local OIDC issuer that PP_AUTH_JWKS_URL points at (:8088)
	@set -a; . ./$(ENV_FILE); set +a; \
	  exec $(GO) run ./scripts/devissuer

.PHONY: dev-token
dev-token: ## Mint a local bearer token and print it, with an example curl
	@./scripts/dev-token.sh --curl

.PHONY: dev-env
dev-env: ## Print the effective local variable set for one service: make dev-env SERVICE=payment-api
	@test -n "$(SERVICE)" || { echo "SERVICE is required, e.g. make dev-env SERVICE=payment-api"; exit 2; }
	@set -a; PP_SERVICE_NAME=$(SERVICE); . ./$(ENV_FILE); set +a; \
	  env | grep '^PP_' | sort

## --- local stack ---

.PHONY: dev-up
dev-up: ## Start the local stack and wait until it is usable
	./scripts/dev-up.sh

.PHONY: dev-down
dev-down: ## Stop the local stack and remove its volumes
	./scripts/dev-down.sh

.PHONY: dev-logs
dev-logs: ## Follow the local stack's logs
	$(DOCKER) compose -f deploy/docker-compose.dev.yml logs -f --tail=100

.PHONY: migrate
migrate: ## Apply pending migrations (runs the static checks first)
	./scripts/migrate.sh up --dsn "$(DSN)"

.PHONY: migrate-status
migrate-status: ## Show applied and pending migrations
	./scripts/migrate.sh status --dsn "$(DSN)"

.PHONY: seed
seed: ## Seed the deterministic synthetic dataset
	./scripts/seed.sh --dsn "$(DSN)"

## --- containers ---

.PHONY: docker
docker: ## Build one service image: make docker SERVICE=payment-api
	@test -n "$(SERVICE)" || { echo "SERVICE is required, e.g. make docker SERVICE=payment-api"; exit 2; }
	$(DOCKER) build \
	  --build-arg SERVICE=$(SERVICE) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  --build-arg SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) \
	  $(if $(filter gateway-simulator,$(SERVICE)),--build-arg ALLOW_TEST_SERVICE=1,) \
	  -t $(IMAGE_REPO)/$(SERVICE):$(VERSION) \
	  -t $(IMAGE_REPO)/$(SERVICE):$(COMMIT) \
	  .

.PHONY: docker-all
docker-all: ## Build every service image
	@for svc in $(SERVICES); do $(MAKE) docker SERVICE=$$svc; done

.PHONY: docker-multiarch
docker-multiarch: ## Build and push a multi-arch image (arm64 primary)
	@test -n "$(SERVICE)" || { echo "SERVICE is required"; exit 2; }
	$(DOCKER) buildx build \
	  --platform $(PLATFORMS) \
	  --build-arg SERVICE=$(SERVICE) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  --build-arg SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) \
	  --provenance=true --sbom=true \
	  -t $(IMAGE_REPO)/$(SERVICE):$(VERSION) \
	  --push .

## --- load ---

.PHONY: loadtest
loadtest: ## Run a k6 scenario: make loadtest SCENARIO=steady-state BASE=... TOKEN=...
	@test -n "$(SCENARIO)" || { echo "SCENARIO is required (steady-state|ramp|spike|soak|all)"; exit 2; }
	./scripts/loadtest.sh $(SCENARIO) --base "$(BASE)" --token "$(TOKEN)"

.PHONY: loadtest-smoke
loadtest-smoke: ## All four scenarios at 1% rate for 2 minutes each, against the local stack
	./scripts/loadtest.sh all --base "$${BASE:-http://localhost:8080}" \
	  --token "$${TOKEN:-dev}" --vus-scale 1 --duration 2m

## --- disaster recovery ---

.PHONY: dr-drill
dr-drill: ## RD-1 restore drill (needs AWS credentials for the dr-verify account)
	./scripts/dr-drill.sh

.PHONY: dr-drill-dry-run
dr-drill-dry-run: ## Print every command the RD-1 drill would run, in order
	./scripts/dr-drill.sh --dry-run

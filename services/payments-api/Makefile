.PHONY: deps build test test-integration lint vet run docker-build

deps:
	go mod tidy
	go mod verify

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/payments-api ./cmd/server

test:
	go test -race -cover ./...

# Requires Postgres reachable at DATABASE_DSN. In CI this is a service container (see
# .github/workflows/ci.yml); locally: `docker compose -f ../../deploy/docker-compose.test.yml up -d`
test-integration:
	go test -race -tags=integration ./tests/integration/...

lint:
	gofmt -l .
	go vet ./...

vet:
	go vet ./...

run:
	AUTH_DISABLED=true ENV=dev LOG_LEVEL=debug DATABASE_DSN=$${DATABASE_DSN} go run ./cmd/server

docker-build:
	docker build -t payments-api:local .

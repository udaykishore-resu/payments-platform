# syntax=docker/dockerfile:1.7

# ---- Build stage -----------------------------------------------------------
# Pinned, minimal Go build image. Multi-stage build so none of the Go toolchain, source, or
# module cache ends up in the final image — smaller image, smaller attack surface, faster pod
# startup and image pulls (all direct contributors to the availability/scaling NFRs).
FROM golang:1.22-bookworm AS build

WORKDIR /src

# Leverage Docker layer caching: dependency download only re-runs when go.mod/go.sum change, not
# on every source edit.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# CGO disabled: produces a fully static binary with no libc dependency, which is what makes the
# distroless "static" base image below possible (no shared libraries to satisfy).
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/payments-api ./cmd/server

# ---- Final stage ------------------------------------------------------------
# distroless/static: no shell, no package manager, no libc — nothing an attacker could use to
# pivot even with code execution inside the container. This is the concrete implementation of
# "minimal container attack surface" referenced in docs/adr/ADR-001-language-runtime.md.
FROM gcr.io/distroless/static-debian12:nonroot AS final

# distroless/*-nonroot already runs as a non-root UID (65532); re-stating explicitly for clarity
# and to satisfy pod security admission policies that check the image's declared user, not just
# the pod spec's runAsNonRoot (defense in depth against a misconfigured pod spec).
USER 65532:65532

COPY --from=build /out/payments-api /payments-api

EXPOSE 8080 9090

ENTRYPOINT ["/payments-api"]

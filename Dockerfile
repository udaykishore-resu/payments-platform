# syntax=docker/dockerfile:1.9
#
# Multi-stage build for all nine deployables (docs/spec/00-design-baseline.md §5).
#
#   docker build --build-arg SERVICE=payment-api -t payment-api:dev .
#
# One Dockerfile rather than nine, because nine would drift: the day one of them is built
# with a different Go version or forgets `-trimpath`, that service's images stop being
# reproducible and nobody notices until a provenance attestation is compared.
#
# ---------------------------------------------------------------------------------------
# The properties this file is responsible for
# ---------------------------------------------------------------------------------------
#
#   Static      CGO_ENABLED=0. The runtime image has no libc, so a dynamically-linked
#               binary would fail at exec with a message that says nothing useful. It also
#               removes the entire class of "works locally, glibc version mismatch in the
#               cluster" problems.
#
#   Distroless  gcr.io/distroless/static-debian12:nonroot. No shell, no package manager,
#               no busybox. That is not hardening theatre: `kubectl exec` into a payment
#               orchestrator is a PCI finding (§17.2), and the most reliable way to make
#               it impossible is for there to be nothing to exec.
#
#   Non-root    The :nonroot tag runs as uid 65532. Declared again in the Kubernetes
#               securityContext (deployment.md §1.10), because two independent controls is
#               the point — an image that lost its USER line must still be refused by the
#               admission policy.
#
#   Stamped     Version, commit and build date via -ldflags -X. A binary that cannot say
#               which commit it is turns every production investigation into archaeology,
#               and the `version` label on every metric and log line (§22.1) has to come
#               from somewhere.
#
#   Reproducible  -trimpath removes local filesystem paths; SOURCE_DATE_EPOCH pins the
#               timestamp. Two builds of one commit must produce the same digest or the
#               SLSA provenance attestation (deployment.md §4.1 stage 17) attests to
#               nothing.
#
# ---------------------------------------------------------------------------------------
# There is deliberately no HEALTHCHECK
# ---------------------------------------------------------------------------------------
#
# Kubernetes owns liveness and readiness for these workloads, and a Docker HEALTHCHECK
# would be a second, weaker, uncoordinated opinion about the same question:
#
#   * Kubernetes ignores it entirely. The kubelet reads livenessProbe/readinessProbe/
#     startupProbe from the pod spec and never looks at the image's HEALTHCHECK, so in the
#     environment that matters it is dead configuration that still has to be maintained.
#
#   * The three probes are not the same check, and a HEALTHCHECK cannot express the
#     difference. deployment.md §1.7 draws it sharply: /livez must touch nothing external
#     (a liveness probe that checks Postgres restarts the whole fleet during a failover —
#     turning a 60-second degradation into an outage), while /readyz must check exactly the
#     dependencies required to serve. Collapsing them into one HEALTHCHECK gets one of them
#     wrong, and the expensive way to be wrong is the liveness one.
#
#   * It would drift. The probe thresholds, timeouts and the terminationGracePeriodSeconds
#     budget (deployment.md §1.8) are tuned per service in the manifests and reviewed
#     there. A second copy in the image is a copy that will not be updated.
#
# For local `docker run`, use the binary's own flag: every service supports
# `-healthcheck`, which performs one probe and exits 0/1 — which is what
# deploy/docker-compose.dev.yml uses.

# ---------------------------------------------------------------------------------------
# Build arguments
# ---------------------------------------------------------------------------------------

# Pinned to the toolchain in go.mod. The digest is not pinned here because the CI job
# resolves and records it in the SBOM; pinning it in the file means a security update to
# the builder is a code change in nine PRs' worth of conflict.
ARG GO_VERSION=1.24.7
ARG ALPINE_VERSION=3.21

# ---------------------------------------------------------------------------------------
# Stage 1 — dependencies
#
# Split from the build stage so that a source-only change does not re-download modules.
# `go mod download` over this repository's dependency set is ~40 s cold; the difference
# between a 40 s and a 4 s inner loop is whether people build images locally at all.
# ---------------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS deps

# git: the module system needs it for some VCS-resolved dependencies.
# ca-certificates: needed by the proxy fetch, and copied into the final image below.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./

# The cache mounts are what make an incremental rebuild fast. `sharing=locked` because
# buildx can run several target platforms concurrently and the module cache is not
# safe for concurrent writers.
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download -x

# ---------------------------------------------------------------------------------------
# Stage 2 — build
# ---------------------------------------------------------------------------------------
FROM deps AS build

# SERVICE selects the binary. No default: a typo should fail the build with a clear
# message rather than silently produce whichever service happened to be first.
ARG SERVICE
# SERVICE_PATH overrides the conventional ./cmd/${SERVICE} package. It exists for exactly one
# case: a development-only tool whose main package deliberately does not live under cmd/, because
# cmd/ is the set of things that get deployed. scripts/devissuer is the only user, and the guard
# stage below refuses to build it without ALLOW_TEST_SERVICE=1 for the same reason it refuses the
# gateway simulator.
ARG SERVICE_PATH=""
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE
ARG TARGETOS
ARG TARGETARCH

# SOURCE_DATE_EPOCH makes the build reproducible: without it the embedded build timestamp
# differs between two builds of the same commit and so does the layer digest.
ARG SOURCE_DATE_EPOCH=0

WORKDIR /src
COPY . .

RUN test -n "${SERVICE}" || { \
      echo "ERROR: --build-arg SERVICE=<name> is required."; \
      echo "One of: payment-api payment-orchestrator control-plane-api webhook-ingress"; \
      echo "        workflow-worker outbox-relay event-consumer gateway-simulator platformctl"; \
      exit 1; \
    }

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    set -eux; \
    BUILD_DATE="${BUILD_DATE:-$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo 1970-01-01T00:00:00Z)}"; \
    # The tags:
    #   osusergo,netgo   force the pure-Go user and resolver implementations. Without
    #                    them a CGO_ENABLED=0 binary still tries to read /etc/nsswitch.conf
    #                    semantics it cannot honour, and DNS resolution behaves differently
    #                    in the image than on the developer's machine.
    #   timetzdata       embed the tzdata database. The distroless static image has no
    #                    /usr/share/zoneinfo, and settlement schedules (§23) are expressed
    #                    in merchant-local time — a time.LoadLocation that fails at 02:00
    #                    is a settlement run that does not happen.
    #
    # The ldflags:
    #   -s -w            strip the symbol and DWARF tables: ~30 % smaller, and a payment
    #                    binary is not debugged with a debugger attached in production.
    #   -X …             stamp version/commit/date into the well-known variables the
    #                    telemetry package reads for the `version` resource attribute.
    #   -extldflags      belt and braces on static linking.
    CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-arm64}" \
    go build \
      -trimpath \
      -buildvcs=false \
      -tags "osusergo,netgo,timetzdata" \
      -ldflags "-s -w \
        -extldflags '-static' \
        -X 'github.com/udaykishore-resu/payments-platform/internal/platform/config.Version=${VERSION}' \
        -X 'github.com/udaykishore-resu/payments-platform/internal/platform/config.Commit=${COMMIT}' \
        -X 'github.com/udaykishore-resu/payments-platform/internal/platform/config.BuildDate=${BUILD_DATE}' \
        -X 'github.com/udaykishore-resu/payments-platform/internal/platform/config.Service=${SERVICE}'" \
      -o /out/app \
      "${SERVICE_PATH:-./cmd/${SERVICE}}"

# Fail loudly on a dynamically-linked result rather than at container start, where the
# error is `exec format error` or `no such file or directory` on a file that plainly
# exists — one of the least informative messages in the ecosystem.
RUN set -eux; \
    if command -v file >/dev/null 2>&1; then file /out/app; fi; \
    ! ldd /out/app 2>/dev/null | grep -q '=>' || { \
      echo "ERROR: /out/app is dynamically linked; the distroless static base has no loader"; \
      exit 1; }

# ---------------------------------------------------------------------------------------
# Stage 3 — the gateway simulator guard
#
# baseline §5 and deployment.md §0: the simulator must never run in production. There are
# two independent controls, and this is the first: a build argument must explicitly permit
# it. The second is a Kyverno ClusterPolicy in the prod cluster that denies any Pod whose
# image name matches `gateway-simulator`.
#
# Two controls, because "it will never be deployed to prod" is a statement about intent,
# and intent is not a mechanism.
# ---------------------------------------------------------------------------------------
FROM build AS guard
ARG SERVICE
ARG ALLOW_TEST_SERVICE=0
RUN set -eux; \
    case "${SERVICE}" in \
      gateway-simulator) \
        if [ "${ALLOW_TEST_SERVICE}" != "1" ]; then \
          echo "ERROR: gateway-simulator is a test-only deployable (baseline §5)."; \
          echo "Build it with --build-arg ALLOW_TEST_SERVICE=1, and never promote the"; \
          echo "resulting image past staging. The prod cluster's Kyverno policy will"; \
          echo "refuse it regardless."; \
          exit 1; \
        fi ;; \
      devissuer) \
        if [ "${ALLOW_TEST_SERVICE}" != "1" ]; then \
          echo "ERROR: devissuer is a local development OIDC issuer, not a deployable."; \
          echo "It generates its signing key in memory with no protection and mints tokens"; \
          echo "for any tenant that asks. It refuses env=production at runtime as well, but"; \
          echo "an image that exists is an image someone can run — so it is refused here"; \
          echo "unless the build explicitly asks for a test service."; \
          exit 1; \
        fi ;; \
    esac

# ---------------------------------------------------------------------------------------
# Stage 4 — runtime
# ---------------------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE

# OCI annotations. `source` and `revision` are what an image digest found in a cluster is
# traced back to a commit with, which is the first question of every incident.
LABEL org.opencontainers.image.title="${SERVICE}" \
      org.opencontainers.image.description="payments-platform ${SERVICE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/udaykishore-resu/payments-platform" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      com.payments-platform.service="${SERVICE}"

# The distroless static image already carries ca-certificates and /etc/passwd for the
# nonroot user. Copying them again would only risk shadowing a fixed version with a stale
# one from the builder — so only the binary crosses the stage boundary.
COPY --from=guard --chown=65532:65532 /out/app /app/app

# uid:gid rather than the name. A numeric user is what
# `securityContext.runAsUser: 65532` and the `runAsNonRoot: true` admission check can
# actually verify; a name requires the kubelet to resolve it inside the image, which it
# will not do, and the pod is then rejected for an unrelated-looking reason.
USER 65532:65532

WORKDIR /app

# The service name is baked in so that `docker run <image>` needs no arguments and so that
# the process's own configuration (which reads PP_SERVICE_NAME for the `service` label on
# every metric, log and span — §22.1) has a correct default even if the manifest omits it.
ENV PP_SERVICE_NAME="${SERVICE}" \
    PP_VERSION="${VERSION}" \
    # Distroless has no /tmp writable by default in some configurations; being explicit
    # about where the process may write pairs with readOnlyRootFilesystem: true and an
    # emptyDir at /tmp in the pod spec.
    TMPDIR="/tmp"

# ENTRYPOINT, not CMD, and in exec form. Exec form means the binary is PID 1 and receives
# SIGTERM directly — which is what the graceful-shutdown sequence of deployment.md §1.8
# depends on. Shell form would interpose /bin/sh (which does not exist here) and, where it
# does exist, would swallow the signal and let the pod be SIGKILLed at the end of the grace
# period with in-flight gateway calls still open.
ENTRYPOINT ["/app/app"]

# NO HEALTHCHECK. See the long explanation at the top of this file: Kubernetes probes own
# liveness and readiness, they are three different checks that a single HEALTHCHECK cannot
# express, and the kubelet ignores this instruction anyway. For local runs the binary
# accepts `-healthcheck`.

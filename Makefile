# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-token-service
APP_ENV       ?= dev
# Observability identity (Enterprise Platform Observability Standard):
# required by platform-gincommon; APP_ENV must be local|dev|test|staging|prod.
OBSERVABILITY_DOMAIN ?= iam
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private modules — resolved via SSH (git@github.com:) using the global URL
# rewrite. No tokens needed for local dev; an SSH key registered with the
# BCBP-SOLUTIONS-FZC-LLC org is required.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

# Docker socket for testcontainers-go (Mac Docker Desktop uses a user socket).
DOCKER_SOCKET     := $(shell [ -S /Users/$(USER)/.docker/run/docker.sock ] && echo unix:///Users/$(USER)/.docker/run/docker.sock || echo unix:///var/run/docker.sock)
export DOCKER_HOST ?= $(DOCKER_SOCKET)

export APP_NAME APP_ENV OBSERVABILITY_DOMAIN BUILD_VERSION

# Test package groups (explicit to handle per-group build tags cleanly).
TEST_UNIT_PKGS     := ./test/unit/...
TEST_CONTRACT_PKGS := ./test/contract/...
TEST_POSTGRES_PKGS := ./test/postgres/...
TEST_INT_PKGS      := ./test/integration/...
TEST_E2E_PKGS      := ./test/e2e/...

# Every test/postgres/*.go test func calls t.Parallel() and spins its own
# testcontainers Postgres (create+migrate+grant, ~1-2s) — capped, not
# GOMAXPROCS-wide, so we don't spin 10+ containers at once on a dev machine
# and reintroduce Docker resource-contention flakes. Override on the CLI
# (e.g. `make test-postgres TEST_POSTGRES_PARALLEL=8`) on a bigger box.
TEST_POSTGRES_PARALLEL ?= 4

# White-box (package-internal) tests included in unit runs.
TEST_INTERNAL_PKGS := ./cmd/rotator/... \
                      ./cmd/scheduler/... \
                      ./cmd/server/... \
                      ./cmd/consumer/... \
                      ./internal/adapter/inbound/http/... \
                      ./internal/adapter/inbound/consumer/... \
                      ./internal/adapter/outbound/eventbus/... \
                      ./internal/adapter/outbound/openbao/... \
                      ./internal/adapter/outbound/postgres/... \
                      ./internal/adapter/outbound/metrics/... \
                      ./internal/adapter/outbound/realmprovisioner/... \
                      ./internal/adapter/outbound/httpx/... \
                      ./internal/core/port/... \
                      ./internal/core/domain/... \
                      ./internal/core/service/... \
                      ./pkg/...

# Coverage scope — internal/+pkg/ only, deliberately excluding cmd/: every
# cmd/*/main.go composition-root function cannot be unit-invoked at all
# (mirrors the sibling iam-org-membership service's own COVER_PKG_LIST) —
# including cmd/ in the denominator just dilutes the number with functions
# no test will ever reach, in exchange for no real signal. cmd/'s own
# helper functions (envOr, buildGlueCodec, ...) are still exercised by
# TEST_INTERNAL_PKGS above; they just aren't counted toward this
# percentage. test/e2e is what actually proves cmd/*/main.go's wiring
# works, black-box.
COVER_PKG_LIST := $(shell $(GO) list ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

# Every build tag a test file here declares (integration: test/integration + test/postgres, e2e: test/e2e). vet and lint run a
# second pass with all of them — CI's quality gate calls `make vet` /
# `make lint`, so without it the tagged test files were never checked.
ALL_TEST_TAGS := integration,e2e

SCHEMA_GOV_IMAGE ?= ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup:
	@test -f .env || cp .env-example .env
	@mkdir -p .git/hooks
	@test -f .githooks/pre-commit && cp .githooks/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit || true
	@echo "Environment ready (.env)"

.PHONY: install-hooks
install-hooks:
	@mkdir -p .git/hooks
	@cp .githooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed git hooks"

# godoc: serve package documentation locally using pkgsite.
# Opens http://localhost:8080 — browse to the module path in the UI.
.PHONY: godoc
godoc:
	@echo "Starting pkgsite at http://localhost:8080 — press Ctrl-C to stop"
	$(GO) run golang.org/x/pkgsite/cmd/pkgsite@latest -open .

# pin-base-images: fetch and pin the current SHA digests for Dockerfile base
# images. Writes the digests both to the Dockerfile FROM lines and to
# .docker-digests (a checked-in provenance record). CI verifies the two match.
.PHONY: pin-base-images
pin-base-images:
	@echo "Fetching SHA digests for Dockerfile base images..."
	@GOLANG_DIGEST=$$(docker buildx imagetools inspect golang:1.26.6-bookworm --format '{{.Manifest.Digest}}') && \
	 DISTROLESS_DIGEST=$$(docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}') && \
	 sed -i.bak \
	   -e "s|FROM golang:1.26.6-bookworm|FROM golang:1.26.6-bookworm@$$GOLANG_DIGEST|" \
	   -e "s|FROM gcr.io/distroless/static-debian12:nonroot|FROM gcr.io/distroless/static-debian12:nonroot@$$DISTROLESS_DIGEST|" \
	   Dockerfile && rm -f Dockerfile.bak && \
	 echo "golang:1.26.6-bookworm $$GOLANG_DIGEST" > .docker-digests && \
	 echo "gcr.io/distroless/static-debian12:nonroot $$DISTROLESS_DIGEST" >> .docker-digests && \
	 echo "Digests written to .docker-digests — commit both Dockerfile and .docker-digests"

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup           - copy .env-example to .env if missing"
	@echo "  make tidy            - go mod tidy"
	@echo "  make fmt             - go fmt ./..."
	@echo "  make fmt-check       - verify gofmt formatting (mirrors CI)"
	@echo "  make vet             - go vet (default build + every test build tag)"
	@echo "  make lint            - run golangci-lint (default build + every test build tag)"
	@echo "  make arch-lint       - run go-arch-lint against .go-arch-lint.yml"
	@echo "  make gates           - invariant gates (no-gocloak, no-secret-log, SET-LOCAL-only, gincommon-obs, metrics-taxonomy)"
	@echo "  make test            - unit + contract + postgres + integration tests (requires Docker)"
	@echo "  make test-ci         - test with race detector + coverage (used in CI)"
	@echo "  make test-unit       - unit tests only (no Docker required)"
	@echo "  make test-contract   - event-contract tests (no external services)"
	@echo "  make test-postgres   - Postgres + RLS integration tests (requires Docker)"
	@echo "  make test-integration - cross-layer integration tests (Postgres + OpenBao, testcontainers)"
	@echo "  make test-e2e        - end-to-end tests (requires Docker)"
	@echo "  make test-smoke      - CI-only image gate: size <=200MB + startup-gate check (requires IMAGE_TAG + BINARY)"
	@echo "  make race            - all tests with -race flag"
	@echo "  make run             - run the server locally (go run)"
	@echo "  make run-consumer    - run the consumer locally (go run; APP_PORT=8081)"
	@echo "  make run-scheduler   - run one cadence-scheduler scan pass locally (go run; METRICS_PORT=9092)"
	@echo "  make build           - compile all four binaries to bin/"
	@echo "  make cover           - coverage HTML report"
	@echo "  make cover-func      - coverage summary by function"
	@echo "  make ci              - tidy + fmt-check + vet + lint + arch-lint + gates + test-ci + build"
	@echo "  make docker-up       - start local infra (Postgres/PgBouncer/Floci/OpenBao) + floci-ui web console"
	@echo "  make docker-down     - stop local containers"
	@echo "  make docker-run-app  - build+run the containerized server+consumer (requires .go_private_token)"
	@echo "  make docker-run-rotator - run the rotator once, then exit (requires .go_private_token)"
	@echo "  make docker-run-scheduler - run the cadence scheduler once, then exit (requires .go_private_token)"
	@echo "  make mod-verify      - go mod verify"
	@echo "  make vuln-check      - govulncheck on internal + pkg"
	@echo "  make sast            - gosec static-analysis scan (SAST)"
	@echo "  make extract-schemas - derive internal/adapter/outbound/eventbus/schemas/*.json"
	@echo "  make swag            - regenerate docs/swagger/ from handler annotations (mirrors sibling iam-org-membership)"
	@echo "  make swag-check      - fail if Swagger regeneration would change docs/swagger/ (CI drift gate)"
	@echo "  make install-hooks   - install .githooks/pre-commit into .git/hooks"
	@echo "  make godoc           - serve local godoc/pkgsite at http://localhost:8080"
	@echo "  make pin-base-images - fetch + pin SHA digests for Dockerfile base images"
	@echo "  make clean           - remove build artefacts"
	@echo ""
	@echo "Schema governance (platform-schemagov 0.4):"
	@echo "  make schema-pull      - pull the schema-gov Docker image"
	@echo "  make schema-validate  - validate AsyncAPI + event schemas (no AWS needed)"
	@echo "  make schema-diff      - diff two schemas: CURRENT=<path> PROPOSED=<path>"
	@echo "  make schema-register  - register event schemas to Glue (requires AWS)"
	@echo "  make schema-verify    - pre-deploy check: each produced schema definition registered + AVAILABLE (requires AWS)"
	@echo "  make schema-prune     - dry-run: list orphaned Glue schemas"

# -----------------------------
# GO BASICS
# -----------------------------

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	@gofmt -l -w .

.PHONY: vet
vet:
	$(GO) vet ./...
	$(GO) vet -tags=$(ALL_TEST_TAGS) ./...

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "FAIL: unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "gofmt: all files formatted"

.PHONY: mod-verify
mod-verify:
	$(GO) mod verify

.PHONY: vuln-check
vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./internal/... ./pkg/...

# gosec is a Go-code SAST scanner — distinct from vuln-check (known-CVE Go
# module versions) and the release pipeline's Trivy scan (OS/container
# package CVEs): this is the one gate that looks at this service's own
# source for code-level issues (hardcoded credentials, unsafe
# deserialization, command injection, etc.). Pinned as a `go tool`
# directive (go.mod) for the same reason golangci-lint is, so local and CI
# runs never drift.
.PHONY: sast
sast:
	$(GO) tool gosec ./...

# -----------------------------
# LINT
# -----------------------------

.PHONY: lint
lint:
	@echo "Running linter..."
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=$(ALL_TEST_TAGS) ./...

# arch-lint: enforce .go-arch-lint.yml component boundaries — the same script
# CI runs (validate-test.yml).
.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

.PHONY: no-gocloak
no-gocloak:
	bash .github/scripts/check-no-gocloak.sh

.PHONY: no-secret-log
no-secret-log:
	bash .github/scripts/check-no-secret-log.sh

.PHONY: set-local-only
set-local-only:
	bash .github/scripts/check-set-local-only.sh

.PHONY: gincommon-obs
gincommon-obs:
	bash .github/scripts/check-gincommon-observability.sh

# Enterprise Platform Observability Standard — naming/namespace-classification
# gate (Tier-1/2/3 prefixes, counter _total / histogram _seconds suffixes,
# no service name leaked into a shared metric name). The required-label
# contract (domain/service/environment per tier) is verified by
# internal/adapter/outbound/metrics/metrics_test.go instead — that needs
# the real registered collectors, not static source text.
.PHONY: metrics-taxonomy
metrics-taxonomy:
	python3 .github/scripts/check-metrics-taxonomy.py

.PHONY: gates
gates: no-gocloak no-secret-log set-local-only gincommon-obs metrics-taxonomy

# -----------------------------
# TESTS
# -----------------------------

.PHONY: _test-unit
_test-unit: | .coverage
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) \
	  -race -count=1 -timeout 120s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/unit.out

.PHONY: _test-contract
_test-contract: | .coverage
	$(GO) test $(TEST_CONTRACT_PKGS) \
	  -race -count=1 -timeout 60s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/contract.out

.PHONY: _test-postgres
_test-postgres: | .coverage
	{ $(GO) test $(TEST_POSTGRES_PKGS) \
	  -tags=integration -race -count=1 -timeout 1500s -parallel $(TEST_POSTGRES_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/postgres.out \
	  2>&1; echo $$? >.coverage/postgres.exitcode; } | tee .coverage/postgres.raw; \
	_exit=$$(cat .coverage/postgres.exitcode 2>/dev/null || echo 1); \
	[ "$$_exit" = "0" ] || { \
	  printf '\n\n=== FAILING POSTGRES TESTS (see full log above for details) ===\n'; \
	  grep '^--- FAIL:' .coverage/postgres.raw || printf '(no --- FAIL lines — check for DATA RACE or panic above)\n'; \
	  printf '=============================================================\n\n'; \
	}; \
	exit "$$_exit"

.PHONY: _test-integration
_test-integration: | .coverage
	$(GO) test $(TEST_INT_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/integration.out

.PHONY: test
test:
	$(MAKE) -j4 _test-unit-plain _test-contract-plain _test-postgres-plain _test-integration-plain

.PHONY: _test-unit-plain _test-contract-plain _test-postgres-plain _test-integration-plain
_test-unit-plain:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 120s -v
_test-contract-plain:
	$(GO) test $(TEST_CONTRACT_PKGS) -count=1 -timeout 60s -v
_test-postgres-plain:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v
_test-integration-plain:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 300s -v

# Merge the four per-suite profiles into a single coverage.out (max-count
# strategy — any suite covering a block wins). Mirrors iam-org-membership.
.PHONY: _merge-coverage
_merge-coverage:
	@python3 scripts/merge_coverage.py \
	  .coverage/unit.out .coverage/contract.out .coverage/postgres.out .coverage/integration.out \
	  > coverage.out
	@echo "==> coverage.out merged from all suites (max-count strategy)"

.PHONY: test-ci
test-ci: | .coverage
	$(MAKE) -j4 _test-unit _test-contract _test-postgres _test-integration
	$(MAKE) _merge-coverage

.PHONY: test-unit
test-unit:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 60s -v

.PHONY: test-contract
test-contract:
	$(GO) test $(TEST_CONTRACT_PKGS) -count=1 -timeout 60s -v

.PHONY: test-postgres
test-postgres:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v

.PHONY: test-integration
test-integration:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 600s -v

.PHONY: test-e2e
test-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -count=1 -timeout 300s -v

.PHONY: test-smoke
test-smoke:
	@test -n "$(IMAGE_TAG)" || { echo "Usage: make test-smoke IMAGE_TAG=<tag> BINARY=server|consumer|rotator [ENTRYPOINT=/iam-token-service-<binary>]"; exit 1; }
	IMAGE_TAG=$(IMAGE_TAG) BINARY=$(BINARY) ENTRYPOINT=$(ENTRYPOINT) bash .github/scripts/smoke-tests.sh

.PHONY: race
race:
	$(MAKE) -j4 _test-unit _test-contract _test-postgres _test-integration

# -----------------------------
# RUN
# -----------------------------

.PHONY: run
run:
	@-lsof -ti :$${APP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/server'

# Native consumer alongside `make run`. Overrides APP_PORT/METRICS_PORT to
# 8081/9091 so it doesn't collide with cmd/server.
.PHONY: run-consumer
run-consumer:
	@-lsof -ti :8081 | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && APP_PORT=8081 METRICS_PORT=9091 BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/consumer'

# Native scheduler run — one due-list scan pass then exit (§16 TSQ-6
# Resolved, TS-D14), same run-to-completion shape as cmd/rotator.
# Overrides METRICS_PORT to 9092 so it doesn't collide with cmd/server.
.PHONY: run-scheduler
run-scheduler:
	bash -c 'set -a && source .env && set +a && METRICS_PORT=9092 BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/scheduler'

# -----------------------------
# BUILD
# -----------------------------

.PHONY: build
build:
	@echo "Building binaries..."
	@mkdir -p bin
	$(GO) build -ldflags "-X main.buildVersion=$(BUILD_VERSION)" -o bin/iam-token-service-server ./cmd/server
	$(GO) build -ldflags "-X main.buildVersion=$(BUILD_VERSION)" -o bin/iam-token-service-consumer ./cmd/consumer
	$(GO) build -ldflags "-X main.buildVersion=$(BUILD_VERSION)" -o bin/iam-token-service-rotator ./cmd/rotator
	$(GO) build -ldflags "-X main.buildVersion=$(BUILD_VERSION)" -o bin/iam-token-service-scheduler ./cmd/scheduler
	@echo "Verifying library packages compile..."
	$(GO) build ./internal/... ./pkg/...

# -----------------------------
# DOCKER
# -----------------------------
# `make docker-up` only starts infra (Postgres/PgBouncer/Floci/OpenBao) —
# NOT the server/consumer/rotator containers themselves, mirroring the
# sibling iam-org-membership service's convention: building the app image
# needs a private-module secret (.go_private_token) that local dev
# iterating on code doesn't otherwise need, and rebuilding a container on
# every change is slower than `make run`'s native `go run`. Use
# `make docker-run-app` when you specifically want the containerized
# server+consumer(+rotator) instead — e.g. to test the actual Dockerfile/
# image, not just business logic.
#
# OpenBao runs in dev-mode — Kubernetes auth cannot succeed against this
# stack either way, see docker-compose.yml's openbao service comment.

.PHONY: docker-up
docker-up:
	@echo "Starting local PostgreSQL + PgBouncer + floci (SNS/SQS/Glue) + floci-ui (http://localhost:4500) + OpenBao..."
	docker compose up -d postgres pgbouncer floci floci-ui openbao

.PHONY: docker-run-app
docker-run-app:
	@test -f .go_private_token || { echo "ERROR: .go_private_token missing — write a GitHub PAT with read access to BCBP-SOLUTIONS-FZC-LLC private repos into this file (gitignored)"; exit 1; }
	docker compose up -d --build server consumer

.PHONY: docker-run-rotator
docker-run-rotator:
	@test -f .go_private_token || { echo "ERROR: .go_private_token missing — write a GitHub PAT with read access to BCBP-SOLUTIONS-FZC-LLC private repos into this file (gitignored)"; exit 1; }
	docker compose --profile rotator run --rm rotator

.PHONY: docker-run-scheduler
docker-run-scheduler:
	@test -f .go_private_token || { echo "ERROR: .go_private_token missing — write a GitHub PAT with read access to BCBP-SOLUTIONS-FZC-LLC private repos into this file (gitignored)"; exit 1; }
	docker compose --profile scheduler run --rm scheduler

.PHONY: docker-down
docker-down:
	@echo "Stopping local containers..."
	docker compose down

# -----------------------------
# CI
# -----------------------------

.PHONY: ci
ci: tidy fmt-check vet lint arch-lint gates test-ci build

# -----------------------------
# COVERAGE
# -----------------------------

.PHONY: cover
cover: test-ci
	$(GO) tool cover -html=coverage.out

.PHONY: cover-func
cover-func: test-ci
	$(GO) tool cover -func=coverage.out

# -----------------------------
# SCHEMA GOVERNANCE
# -----------------------------
# api/asyncapi.yaml is the single source of truth for this service's 5
# produced event schemas. Unlike a sibling service's shared registry, the
# iam-serviceaccount-events Glue registry is single-producer — every name
# in it belongs to this service (§7.3.1).

.PHONY: schema-pull
schema-pull:
	docker pull "$(SCHEMA_GOV_IMAGE)"

.PHONY: extract-schemas
extract-schemas:
	@echo "Extracting event schemas from api/asyncapi.yaml..."
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" extract \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/adapter/outbound/eventbus/schemas
	@echo "Done. Run 'git add internal/adapter/outbound/eventbus/schemas/' to stage."

.PHONY: schema-validate
schema-validate: extract-schemas
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" validate \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/adapter/outbound/eventbus/schemas

.PHONY: schema-diff
schema-diff:
	@test -n "$(CURRENT)" && test -n "$(PROPOSED)" || { \
	  echo "Usage: make schema-diff CURRENT=<current.json> PROPOSED=<proposed.json>"; \
	  exit 1; \
	}
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" diff \
	  --current     "$(CURRENT)" \
	  --proposed    "$(PROPOSED)" \
	  --schema-name "$(or $(SCHEMA_NAME),$(notdir $(basename $(PROPOSED))))"

# schema-register: register this service's 5 event schemas to Glue (requires
# AWS credentials or floci). Only ever registers the files present under
# internal/adapter/outbound/eventbus/schemas/. Set AWS_ENDPOINT_URL=http://localhost:4568 in .env
# for floci. Note: `make docker-up` already registers schemas via
# scripts/init-floci.sh — this target is for re-registering after a schema
# change without a full container restart, or for registering against real AWS.
.PHONY: schema-register
schema-register:
	@test -n "$(GLUE_REGISTRY_SERVICEACCOUNT_NAME)" || { \
	  echo "GLUE_REGISTRY_SERVICEACCOUNT_NAME is not set — add it to .env"; \
	  exit 1; \
	}
	@rm -rf .tmp/glue-schemas
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" register \
	  --registry   "$(GLUE_REGISTRY_SERVICEACCOUNT_NAME)" \
	  --schema-dir .tmp/glue-schemas

# schema-verify: fail unless every produced schema has an AVAILABLE version
# whose definition matches this checkout's file — the exact lookup GlueCodec
# does at startup (GetSchemaByDefinition, compact + ASCII-escaped like
# schema-gov register uploads it). Reads the same PascalCase staged copy
# schema-register uploads, so it covers exactly the produced schemas. Surfaces
# "pod would CrashLoop on NewGlueCodec" pre-deploy, which a name-only
# get-schema check cannot. Requires GLUE_REGISTRY_SERVICEACCOUNT_NAME, AWS credentials (or
# AWS_ENDPOINT_URL for floci) and python3.
.PHONY: schema-verify
schema-verify:
	@test -n "$(GLUE_REGISTRY_SERVICEACCOUNT_NAME)" || { \
	  echo "GLUE_REGISTRY_SERVICEACCOUNT_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@rm -rf .tmp/glue-schemas-verify
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas-verify >/dev/null
	@failed=""; count=0; \
	for file in .tmp/glue-schemas-verify/*.json; do \
	  name=$$(basename "$$file" .json); count=$$((count + 1)); \
	  def=$$(python3 -c 'import json,sys; sys.stdout.write(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")))' "$$file"); \
	  status=$$(aws glue get-schema-by-definition \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_SERVICEACCOUNT_NAME),SchemaName=$$name" \
	      --schema-definition "$$def" \
	      --region "$(AWS_REGION)" --query Status --output text 2>/dev/null); \
	  if [ "$$status" != "AVAILABLE" ]; then \
	    failed="$$failed $$name($${status:-not-registered})"; \
	  fi; \
	done; \
	rm -rf .tmp/glue-schemas-verify; \
	if [ -n "$$failed" ]; then \
	  echo "FAIL: this checkout's schema definition is not registered+AVAILABLE in '$(GLUE_REGISTRY_SERVICEACCOUNT_NAME)':$$failed"; \
	  echo "     run 'make schema-register' (or wait for schema-registry.yml) to register it"; \
	  exit 1; \
	fi; \
	echo "OK: all $$count iam-token-service schema definitions registered and AVAILABLE in '$(GLUE_REGISTRY_SERVICEACCOUNT_NAME)'"

# schema-prune: dry-run scan for orphaned Glue schemas (exist in Glue, not in
# this repo). Pass EXECUTE=true to archive and delete:
# make schema-prune EXECUTE=true. Requires GLUE_REGISTRY_SERVICEACCOUNT_NAME
# and AWS credentials.
# schema-prune compares Glue against the PascalCase staged copy: prune treats a
# schema as orphaned when no FILE STEM in --schema-dir matches it, and the
# committed files are snake_case — against them (or the default dir) it lists
# every live schema as an orphan, which EXECUTE=true would delete.
.PHONY: schema-prune
schema-prune:
	@test -n "$(GLUE_REGISTRY_SERVICEACCOUNT_NAME)" || { echo "GLUE_REGISTRY_SERVICEACCOUNT_NAME is not set"; exit 1; }
	@rm -rf .tmp/glue-schemas
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas >/dev/null
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" prune \
	  --registry   "$(GLUE_REGISTRY_SERVICEACCOUNT_NAME)" \
	  --schema-dir .tmp/glue-schemas \
	  $(if $(filter true,$(EXECUTE)),--execute,)

# -----------------------------
# CLEAN
# -----------------------------

.coverage:
	@mkdir -p .coverage

.PHONY: clean
clean:
	rm -rf bin .coverage
	rm -f coverage.out coverage.html

# -----------------------------
# DOCS
# -----------------------------
# OpenAPI and AsyncAPI specs have different sources of truth:
#   - OpenAPI (docs/swagger/{docs.go,swagger.json,swagger.yaml}) is GENERATED
#     from swag `@Summary`/`@Tags`/`@Router` annotations on handler functions
#     via `make swag` — never hand-edited. `docs.go`'s init() registers it
#     for ginSwagger.WrapHandler to serve; there is no raw-file embed.
#   - AsyncAPI (api/asyncapi.yaml) IS hand-maintained; the service binary
#     embeds it directly via //go:embed in api/embed.go (apispec.AsyncAPISpec) —
#     no synced duplicate copy, so there is nothing to fall out of sync.
# Serves:
#   /              landing page (linking both doc surfaces)
#   /docs          same landing page
#   /swagger       Swagger UI rendering the generated OpenAPI spec (REST — Try it out enabled)
#   /asyncapi      AsyncAPI Studio rendering /asyncapi.yaml (Events — read-only)
#   /asyncapi.yaml raw spec
#
# Run `make swag` after changing handler annotations; api/asyncapi.yaml takes
# effect on the next build/run since it's a direct compile-time embed.

# swag: generate the OpenAPI/Swagger 2.0 spec from // @… annotations on handlers
# under cmd/server and internal/adapter/inbound/http. Mirrors iam-org-membership's
# pattern so developers moving between services see the same authoring workflow.
# The output under docs/swagger/ is checked into the repo; CI's swag-check target
# fails a PR whose annotations drift from what's on disk.
.PHONY: swag
swag:
	@echo "Generating Swagger docs..."
	$(GO) run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
	  -g swagger_info.go \
	  -d cmd/server,internal/adapter/inbound/http \
	  --output docs/swagger \
	  --parseDependency \
	  --parseInternal
	@echo "Swagger docs written to docs/swagger/"

.PHONY: swag-check
swag-check:
	bash .github/scripts/check-swagger-stale.sh

.PHONY: docs-serve
docs-serve: run
	@echo "Docs will be available at:"
	@echo "  http://localhost:8080/            — landing page"
	@echo "  http://localhost:8080/swagger     — Swagger UI (REST APIs)"
	@echo "  http://localhost:8080/asyncapi    — AsyncAPI Studio (Events)"

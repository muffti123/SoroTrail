BINARY := bin/sorotrail
MIGRATIONS := internal/store/migrations
# Keep in sync with the postgres service in docker-compose.yml (port, user, password, db).
# CI overrides TEST_DATABASE_URL to localhost:5433 because it maps the container
# to a non-default host port.
DATABASE_URL ?= postgres://sorotrail:sorotrail@localhost:5432/sorotrail?sslmode=disable

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "unknown")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo "unknown")

LDFLAGS := -ldflags="-X github.com/sorotrail/sorotrail/internal/buildinfo.Version=$(VERSION) -X github.com/sorotrail/sorotrail/internal/buildinfo.Commit=$(COMMIT) -X github.com/sorotrail/sorotrail/internal/buildinfo.BuildDate=$(BUILD_DATE)"

.PHONY: build build-all build-all-integration run test test-fast test-db test-integration vet vet-integration test-ci lint cover cover-html migrate-up migrate-down docker-up docker-down simtest simtest-long clean bench bench-ci seed spec ci

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/sorotrail

# Compile every package, like the CI test job's build step, rather than
# only the binary. Kept separate from `build` so `ci` can mirror CI
# closely without changing what `build` produces.
build-all:
	go build ./...

# Compile every package with the `integration` build tag, mirroring the
# CI integration job's build step so an untagged build can't hide a break
# in the tagged code.
build-all-integration:
	go build -tags=integration ./...

run: build
	./$(BINARY)

# Run the unit suite with the race detector enabled, matching CI's
# race-enabled run so a data race can't pass locally and fail in CI.
# -race requires cgo and a C toolchain (gcc); see CONTRIBUTING.md for
# the Windows note. If the slowdown is unwelcome, use `test-fast`.
test:
	go test -race ./...

# Plain unit-suite run without the race detector — the previous `test`
# behavior, for when the -race overhead (or a missing C toolchain, e.g.
# on Windows) makes the race-enabled run impractical.
test-fast:
	go test ./...

# Run the full test suite including Postgres integration tests.
# Requires a running Postgres, e.g. `make docker-up` first.
# -p 1 serializes packages: the integration tests in internal/store and
# internal/replay share one database and truncate the same tables.
test-db:
	TEST_DATABASE_URL=$(DATABASE_URL) go test -p 1 ./...

vet:
	go vet ./...

# Vet the integration-tagged code too; `go vet ./...` alone skips files
# behind the `integration` build tag.
vet-integration:
	go vet -tags=integration ./...

# Integration test suite only, gated behind the `integration` build tag so
# `go test ./...` stays fast. The suite honors TEST_DATABASE_URL when set
# (CI's services-postgres path), and otherwise spins up an ephemeral
# Postgres 16-alpine via testcontainers-go per `internal/testdb.Setup`
# call. Either way, the four required coverage areas — migration-up from
# empty, event upsert idempotency, ingestion_state save/resume across
# ingester restarts, GET /events filter combinations against seeded
# data — are asserted against a real PostgreSQL.
#
# -p 1 because the integration tests in internal/store and internal/replay
# share one database and truncate the same tables.
test-integration:
	go test -tags=integration -p 1 ./... -count=1

# Mirror the CI test job's suite: the full untagged test run, serialized
# across packages and race-enabled, as CI runs it. The DB-backed tests
# t.Skip when TEST_DATABASE_URL is unset (see internal/store/postgres_test.go),
# so this passes without a database and silently scales up to the full CI
# gate the moment TEST_DATABASE_URL (or Postgres behind the default
# DATABASE_URL) is available.
test-ci:
	go test -p 1 ./... -count=1 -race -timeout=120s

# Run the deterministic simulation test suite (mock store, fast).
simtest:
	go test ./internal/simtest/... -count=1 -timeout 120s

# Run the simulation test suite with randomized mode extended budget.
# Uses a higher iteration count and prints seeds for reproducibility.
simtest-long:
	go test ./internal/simtest/... -count=1 -timeout 600s -v -run "TestAllCuratedScenarios|TestRandomizedMode"

bench:
	@echo "=================================================================="
	@echo " SoroTrail Benchmark Environment Capture"
	@echo "=================================================================="
	@echo "Date: $$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo "unknown")"
	@echo "Go Version: $$(go version 2>/dev/null || echo "go version unknown")"
	@echo "OS/Arch: $$(go env GOOS 2>/dev/null || echo "unknown")/$$(go env GOARCH 2>/dev/null || echo "unknown")"
	@echo "Postgres URL: $(DATABASE_URL)"
	@echo "=================================================================="
	@echo " Running Benchmarks..."
	@echo "=================================================================="
	TEST_DATABASE_URL=$(DATABASE_URL) go test -bench=. -benchmem ./...

bench-ci:
	go test -bench=. -benchtime=10ms ./...

seed: ## Seed the database with sample events
	go run ./cmd/seed -db="$(DATABASE_URL)" -count=1000000

lint:
	golangci-lint run

# Reproduce the CI gate locally: the test + integration + lint jobs from
# .github/workflows/ci.yml run build, vet (plain and integration-tagged),
# the tagged and untagged test suites, the benchmark smoke run, and
# golangci-lint. Each piece delegates to the existing target above so the
# target list and CI can't drift, and runs as a sub-make with the default
# `-k` unset, so the first failing step stops the run — one error at a
# time, in CI's order. DB-backed suites skip gracefully when
# TEST_DATABASE_URL (or Docker) is unavailable rather than fail.
ci:
	$(MAKE) build-all
	$(MAKE) vet
	$(MAKE) test-ci
	$(MAKE) bench-ci
	$(MAKE) build-all-integration
	$(MAKE) vet-integration
	$(MAKE) test-integration
	$(MAKE) lint

# Regenerate the JSON copy of the OpenAPI spec that internal/api embeds and
# serves at /openapi.json. api/openapi.yaml is the source of truth; run this
# after editing it, or pkg/docs.TestSpecCopiesAreIdentical fails the build.
spec:
	go run ./cmd/specgen

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out

cover-html: cover
	go tool cover -html=coverage.out

# Migrations run automatically on startup; these targets are for manual control.
# Requires the migrate CLI: https://github.com/golang-migrate/migrate
migrate-up:
	migrate -path $(MIGRATIONS) -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path $(MIGRATIONS) -database "$(DATABASE_URL)" down 1

# ── Docker ───────────────────────────────────────────────────────────────────

docker-up: ## Start Postgres and the indexer via docker compose
	docker compose --profile dev up -d --build

docker-down: ## Tear down docker compose services
	# `down` is profile-agnostic: it stops every sorotrail project
	# container regardless of which profile started it.
	docker compose down

clean:
	rm -rf bin coverage.out


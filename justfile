# Tasks for synthwerk-llm. CI runs the same commands (.github/workflows/ci.yml).
set shell := ["bash", "-euo", "pipefail", "-c"]

# Lint and test.
default: lint test

zizmor_version := "1.30.1"

# Compile every package.
build:
    go build ./...

# gofmt, golangci-lint (with and without the integration tag), actionlint + zizmor.
lint:
    test -z "$(gofmt -l .)"
    golangci-lint run ./...
    golangci-lint run --build-tags integration ./...
    actionlint .github/workflows/*.yml
    uvx zizmor@{{ zizmor_version }} --offline .github/workflows

# Unit and integration tests.
test: test-unit test-int

# Unit and contract tests. No Docker.
test-unit:
    go test -race ./...

# Chat slice against real Postgres and NATS. Needs Docker (OrbStack).
test-int:
    go test -tags integration -race -count=1 ./tests/integration/...

# Container image as CI builds it.
image:
    docker build --build-arg COMMIT="$(git rev-parse HEAD)" -t synthwerk-llm:local .

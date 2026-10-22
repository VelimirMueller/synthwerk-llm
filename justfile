# Tasks for synthwerk-llm. CI runs the same commands (.github/workflows/ci.yml).
set shell := ["bash", "-euo", "pipefail", "-c"]

# Lint and test.
default: lint test

zizmor_version := "1.30.1"

# Compile every package.
build:
    go build ./...

# gofmt, golangci-lint, actionlint + zizmor.
lint:
    test -z "$(gofmt -l .)"
    golangci-lint run ./...
    actionlint .github/workflows/*.yml
    uvx zizmor@{{ zizmor_version }} --offline .github/workflows

# Unit tests. No Docker.
test:
    go test -race ./...

# Container image as CI builds it.
image:
    docker build --build-arg COMMIT="$(git rev-parse HEAD)" -t synthwerk-llm:local .

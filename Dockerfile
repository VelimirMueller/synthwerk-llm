# synthwerk-llm: distroless static image, non-root, read-only friendly.
# Build context: the repo root.
#   docker build --build-arg VERSION=0.1.0 --build-arg COMMIT=$(git rev-parse HEAD) -t synthwerk-llm .
FROM golang:1.27.3-alpine3.24@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY migrations/ migrations/
# /readyz shows both values. COMMIT must be 7 to 40 lowercase hex characters.
ARG VERSION=0.1.0
ARG COMMIT=0000000
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/llm ./cmd/llm

FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087
COPY --from=build /out/llm /llm
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/llm"]

FROM mirror.gcr.io/library/golang:1.27.2-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
# Set by CI to the release tag (e.g. 1.14.1) and short commit SHA; both
# default to internal/version's own zero-values for local/manual builds.
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w -X github.com/amalgamated-tools/sheetserve/internal/version.Version=${VERSION} -X github.com/amalgamated-tools/sheetserve/internal/version.Commit=${COMMIT}" -o /out/sheetserve ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot AS runner
WORKDIR /app
COPY --from=builder /out/sheetserve /sheetserve
ENTRYPOINT ["/sheetserve"]

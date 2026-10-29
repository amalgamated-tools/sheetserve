FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# The commit to stamp into the binary; on Render, version.Commit falls back to RENDER_GIT_COMMIT at runtime.
ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags="-s -w -X github.com/amalgamated-tools/sheetserve/internal/version.Commit=${GIT_COMMIT}" \
	-o /sheetserve ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sheetserve /sheetserve
ENTRYPOINT ["/sheetserve"]

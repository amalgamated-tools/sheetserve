# Builds on the build machine's own platform and cross-compiles, so a multi-platform build doesn't
# run Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# The release and commit to stamp into the binary; on Render, version.Commit falls back to
# RENDER_GIT_COMMIT at runtime.
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
	-ldflags="-s -w -X github.com/amalgamated-tools/sheetserve/internal/version.Version=${VERSION} -X github.com/amalgamated-tools/sheetserve/internal/version.Commit=${GIT_COMMIT}" \
	-o /sheetserve ./cmd/server
# An empty directory for the database, owned by the image's nonroot user.
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sheetserve /sheetserve
COPY --from=build --chown=nonroot:nonroot /data /data
# Mount a volume here to keep users, spreadsheets and keys across restarts.
VOLUME /data
ENV SHEETSERVE_DB=/data/sheetserve.db
EXPOSE 8085
ENTRYPOINT ["/sheetserve"]

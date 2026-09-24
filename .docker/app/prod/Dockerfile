FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /orpheus-mattermost ./cmd/orpheus-mattermost
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /orpheus-mattermost /orpheus-mattermost
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s CMD ["/orpheus-mattermost", "healthcheck"]
ENTRYPOINT ["/orpheus-mattermost"]
CMD ["serve"]

FROM hadolint/hadolint:v2.15.1 AS hadolint
FROM aquasec/trivy:0.74.0 AS trivy
FROM golang:1.27.1-bookworm
COPY --from=hadolint /bin/hadolint /usr/local/bin/hadolint
COPY --from=trivy /usr/local/bin/trivy /usr/local/bin/trivy
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 && \
    go install golang.org/x/vuln/cmd/govulncheck@v1.8.0 && \
    go install golang.org/x/tools/cmd/goimports@v0.48.0
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
ARG ORPHEUS_UID=1000
ARG ORPHEUS_GID=1000
RUN (getent group "${ORPHEUS_GID}" || groupadd --gid "${ORPHEUS_GID}" orpheus) && \
    useradd --create-home --uid "${ORPHEUS_UID}" --gid "${ORPHEUS_GID}" orpheus && \
    mkdir -p /go/pkg/mod /home/orpheus/.cache/go-build && chown -R "${ORPHEUS_UID}:${ORPHEUS_GID}" /go /home/orpheus
ENV GOCACHE=/home/orpheus/.cache/go-build
USER ${ORPHEUS_UID}:${ORPHEUS_GID}
CMD ["go", "test", "./..."]

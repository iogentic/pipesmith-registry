# Image of Pipesmith Registry for the container and release stages of
# Pipesmith CI. The build stage uses the Go image of tools/go.sh; the binary
# is static, so the runtime image has no shell and no package manager.
FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/mirosso25/pipesmith-registry/internal/version.Version=$(cat VERSION)" \
    -o /out/pipesmith-registry ./cmd/pipesmith-registry

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/pipesmith-registry /usr/local/bin/pipesmith-registry
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pipesmith-registry"]
CMD ["serve"]

# Static Go binaries on an empty base image (~10 MB total).
# Multi-arch: build for your NAS with e.g.
#   docker buildx build --platform linux/amd64 -t nad-remote .   # Intel/AMD Synology (+, xs)
#   docker buildx build --platform linux/arm64 -t nad-remote .   # ARM Synology (j, value)

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM scratch
COPY --from=build /out/ /usr/local/bin/
# Run as an unprivileged user; nothing here needs root.
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/tv-amp-sync"]

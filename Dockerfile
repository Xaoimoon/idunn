# syntax=docker/dockerfile:1

# The binary is cross-compiled on the build platform, so a multi-arch build
# needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/idunn ./cmd/idunn

FROM gcr.io/distroless/static-debian13
COPY --from=build /out/idunn /idunn
# Starts as root only to read the Docker socket's group, then switches at
# once to uid 65532 with that group (see README, Security). Setting an
# explicit non-root user skips the switch.
HEALTHCHECK --interval=1m --timeout=10s --start-period=30s --retries=3 CMD ["/idunn", "health"]
ENTRYPOINT ["/idunn"]

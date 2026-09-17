ARG GOLANG_VERSION=1.26

ARG BUILDPLATFORM
ARG TARGETPLATFORM

FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:${GOLANG_VERSION} AS builder

ARG CGO_ENABLED=0
ARG GOEXPERIMENT=strictfipsruntime
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

# Keep the build input limited to production source. In particular,
# internal/pricing/model_prices.json is included for its go:embed directive.
COPY cmd/ cmd/
COPY internal/ internal/

# The Go toolset image defaults to a non-root user; compiling writes the
# executable into /app, which is root-owned in that image.
USER root

RUN CGO_ENABLED=${CGO_ENABLED} \
    GOEXPERIMENT=${GOEXPERIMENT} \
    GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH:-amd64} \
    go build -a -trimpath -ldflags="-s -w" -o metering-service ./cmd

FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9/ubi-minimal:latest

WORKDIR /

COPY --from=builder /app/metering-service /metering-service
RUN chmod +x /metering-service

# Compatible with OpenShift's arbitrary UID model.
USER 1001

ENTRYPOINT ["/metering-service"]

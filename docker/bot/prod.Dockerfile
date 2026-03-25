ARG BUILDER_IMAGE_VERSION=1.25-alpine3.23
FROM --platform=$BUILDPLATFORM golang:${BUILDER_IMAGE_VERSION} AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download

COPY . .

ARG BUILD_ENVIRONMENT="production"
ARG BUILD_VERSION="nightly"
ARG BUILD_TIME="unknown"
ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,id=go-build-${TARGETARCH} \
    mkdir -p ./bin && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags "-s -w \
    -X main.buildVersion=${BUILD_VERSION} \
    -X main.buildTime=${BUILD_TIME} \
    -X main.buildEnvironment=${BUILD_ENVIRONMENT}" \
    -o ./bin/bot \
    cmd/bot/main.go

# ---

FROM alpine:3.23

# For fsnotify
RUN apk add --no-cache inotify-tools

COPY --from=builder /app/bin/bot /usr/local/bin/bot

EXPOSE 4200
CMD ["bot"]

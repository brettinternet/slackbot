ARG BUILDER_IMAGE_VERSION=1.24-alpine3.21
FROM golang:${BUILDER_IMAGE_VERSION} AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG BUILD_ENVIRONMENT="production"
ARG BUILD_VERSION="nightly"
ARG BUILD_TIME="unknown"
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p ./bin && \
    go build -ldflags "-s -w \
    -X main.buildVersion=${BUILD_VERSION} \
    -X main.buildTime=${BUILD_TIME} \
    -X main.buildEnvironment=${BUILD_ENVIRONMENT}" \
    -o ./bin/bot \
    cmd/bot/main.go

# ---

FROM alpine:3.21

# For fsnotify
RUN apk add --no-cache inotify-tools

COPY --from=builder /app/bin/bot /usr/local/bin/bot

EXPOSE 4200
CMD ["bot"]

ARG BUILDER_IMAGE_VERSION=1.24-alpine3.21
FROM golang:${BUILDER_IMAGE_VERSION} as builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_ENVIRONMENT="production"
ARG BUILD_VERSION="dev"
ARG BUILD_DATE="unknown"
RUN go build -ldflags "-s -w \
    -X main.buildVersion=${BUILD_VERSION} \
    -X main.buildTime=${BUILD_DATE} \
    -X main.buildEnvironment=${BUILD_ENVIRONMENT}" \
    -a \
    -o ./main \
    cmd/bot/main.go

# ---

ARG RUNTIME_IMAGE_VERSION=3.21
FROM alpine:${RUNTIME_IMAGE_VERSION}

# For fsnotify
RUN apk add --no-cache inotify-tools

COPY --from=builder /app/main /app/

EXPOSE 8080
CMD ["/app/main"]

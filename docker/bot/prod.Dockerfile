ARG BUILDER_IMAGE_VERSION=1.24-alpine3.21
FROM golang:${BUILDER_IMAGE_VERSION} AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_ENVIRONMENT="production"
ARG BUILD_VERSION="dev"
ARG BUILD_DATE="unknown"
RUN mkdir -p ./bin && \
  go build -ldflags "-s -w \
  -X main.buildVersion=${BUILD_VERSION} \
  -X main.buildTime=${BUILD_DATE} \
  -X main.buildEnvironment=${BUILD_ENVIRONMENT}" \
  -a \
  -o ./bin/bot \
  cmd/bot/main.go

# ---

FROM alpine:3.21

# For fsnotify
RUN apk add --no-cache inotify-tools

COPY --from=builder /app/bin/bot /app/

EXPOSE 4200
CMD ["/app/bot"]

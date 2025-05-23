ARG BUILDER_IMAGE_VERSION=1.24-alpine3.21
FROM golang:${BUILDER_IMAGE_VERSION} as builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev

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

FROM alpine:3.21

# For fsnotify
RUN apk add --no-cache inotify-tools

COPY --from=builder /app/main /app/

EXPOSE 4200
CMD ["/app/main"]

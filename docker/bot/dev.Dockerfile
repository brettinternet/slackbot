ARG IMAGE_VERSION=1.24-alpine
FROM golang:${IMAGE_VERSION}

WORKDIR /app

RUN apk add --no-cache gcc musl-dev inotify-tools

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_ENVIRONMENT="production"
ARG BUILD_VERSION="dev"
ARG BUILD_TIME="unknown"
RUN mkdir -p ./bin && \
  go build -ldflags "-s -w \
  -X main.buildVersion=${BUILD_VERSION} \
  -X main.buildTime=${BUILD_TIME} \
  -X main.buildEnvironment=${BUILD_ENVIRONMENT}" \
  -a \
  -o ./bin/bot \
  cmd/bot/main.go

CMD ["/app/bin/bot"]
EXPOSE 4200

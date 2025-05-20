ARG IMAGE_VERSION=1.24-alpine3.21
FROM golang:${IMAGE_VERSION}


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
    -o ./main \
    cmd/bot/main.go

CMD ["./main"]

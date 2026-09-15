FROM golang:1.25 AS builder

ARG GOPROXY=https://goproxy.cn,direct

WORKDIR /src

COPY go.mod go.sum ./
RUN GOPROXY="${GOPROXY}" go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/ai-business-service \
    ./cmd/ai-business-service

FROM debian:stable-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl netbase \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --create-home app

COPY --from=builder --chown=app:app /out/ai-business-service /app/ai-business-service

WORKDIR /app

USER app

EXPOSE 18000 19000
VOLUME /data/conf

CMD ["./ai-business-service", "-conf", "/data/conf/config.yaml"]

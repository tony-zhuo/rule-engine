FROM golang:1.25 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Two binaries share this image: the rule-admin API (control plane) and one
# engine shard (data plane). docker-compose picks between them via entrypoint.
RUN go build -o /rule-engine-api ./cmd/apis
RUN go build -o /rule-engine-core ./cmd/rule-engine-core

FROM debian:bookworm-slim
WORKDIR /app
RUN apt-get update && apt-get install -y ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
COPY --from=builder /rule-engine-api /rule-engine-api
COPY --from=builder /rule-engine-core /rule-engine-core
EXPOSE 8080
ENTRYPOINT ["/rule-engine-api"]

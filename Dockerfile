FROM golang:1.25 AS builder
WORKDIR /app
RUN apt-get update && apt-get install -y librdkafka-dev && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /rule-engine-api ./cmd/apis
RUN go build -o /rule-engine-worker ./cmd/worker

FROM debian:bookworm-slim
WORKDIR /app
RUN apt-get update && apt-get install -y ca-certificates tzdata librdkafka1 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /rule-engine-api /rule-engine-api
COPY --from=builder /rule-engine-worker /rule-engine-worker
EXPOSE 8080
ENTRYPOINT ["/rule-engine-api"]

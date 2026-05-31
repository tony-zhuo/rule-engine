FROM golang:1.25 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /rule-engine-api ./cmd/apis

FROM debian:bookworm-slim
WORKDIR /app
RUN apt-get update && apt-get install -y ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
COPY --from=builder /rule-engine-api /rule-engine-api
EXPOSE 8080
ENTRYPOINT ["/rule-engine-api"]

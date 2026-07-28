.PHONY: build run-api run-core run-producer test test-short migrate migrate-down docker-up docker-down docker-reset tidy kafka-topics kafka-consume kafka-describe

# Database
DB_USER ?= rule_engine
DB_PASSWORD ?= rule_engine
DB_HOST ?= localhost
DB_PORT ?= 5432
DB_NAME ?= rule_engine
MIGRATE_DIR = database/migrate/init

# Kafka (alternative engine backend; NATS is the default)
KAFKA_CONTAINER ?= rule-engine-kafka-1
KAFKA_BOOTSTRAP ?= localhost:9092
KAFKA_TOPIC ?= rule-events

# Build
build:
	go build -o bin/apis ./cmd/apis
	go build -o bin/rule-engine-core ./cmd/rule-engine-core
	go build -o bin/event-producer ./cmd/event-producer

# Run — rule-admin API (control plane)
run-api:
	go run ./cmd/apis

# Run — one engine shard (data plane). SHARD_ID / NATS_URL / SNAPSHOT_DIR via env.
run-core:
	go run ./cmd/rule-engine-core

# Run — synthetic load generator. BACKEND=nats|kafka, RATE, COUNT via env.
run-producer:
	go run ./cmd/event-producer

# Test
test:
	go test ./...

# Skips Kafka + shadow tests, so no Docker needed.
test-short:
	go test -short ./...

# Migration
migrate:
	@for f in $(MIGRATE_DIR)/*.up.sql; do \
		echo "Running $$f ..."; \
		PGPASSWORD=$(DB_PASSWORD) psql -h $(DB_HOST) -p $(DB_PORT) -U $(DB_USER) -d $(DB_NAME) -f $$f; \
	done
	@echo "Migration done."

migrate-down:
	@echo "Dropping tables..."
	PGPASSWORD=$(DB_PASSWORD) psql -h $(DB_HOST) -p $(DB_PORT) -U $(DB_USER) -d $(DB_NAME) \
		-c "DROP TABLE IF EXISTS cep_patterns, rule_strategies CASCADE;"
	@echo "Done."

# Docker
docker-up:
	docker compose up -d

docker-down:
	docker compose down

docker-reset:
	docker compose down -v
	docker compose up -d --build

# Kafka helpers
# No kafka-lag target: the engine pins partitions manually rather than joining a
# consumer group (it owns the source offset via snapshots), so there is no group
# cursor for kafka-consumer-groups to report on.
kafka-topics:
	docker exec $(KAFKA_CONTAINER) kafka-topics --bootstrap-server $(KAFKA_BOOTSTRAP) --list

kafka-describe:
	docker exec $(KAFKA_CONTAINER) kafka-topics --bootstrap-server $(KAFKA_BOOTSTRAP) --topic $(KAFKA_TOPIC) --describe

kafka-consume:
	docker exec -it $(KAFKA_CONTAINER) kafka-console-consumer --bootstrap-server $(KAFKA_BOOTSTRAP) --topic $(KAFKA_TOPIC) --from-beginning

# Go
tidy:
	go mod tidy

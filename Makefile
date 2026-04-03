.PHONY: build run-api run-worker migrate migrate-down docker-up docker-down docker-reset tidy kafka-topics kafka-consume kafka-lag kafka-describe

# Database
DB_USER ?= rule_engine
DB_PASSWORD ?= rule_engine
DB_HOST ?= localhost
DB_PORT ?= 5432
DB_NAME ?= rule_engine
MIGRATE_DIR = database/migrate/init

# Kafka
KAFKA_CONTAINER ?= rule-engine-kafka-1
KAFKA_BOOTSTRAP ?= localhost:9092
KAFKA_TOPIC ?= rule-engine-events
KAFKA_GROUP ?= rule-engine-worker

# Build
build:
	go build -o bin/apis ./cmd/apis
	go build -o bin/worker ./cmd/worker

run-api:
	go run ./cmd/apis

run-worker:
	go run ./cmd/worker

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
		-c "DROP TABLE IF EXISTS behavior_logs, rule_strategies CASCADE;"
	@echo "Done."

# Docker
docker-up:
	docker compose up -d

docker-down:
	docker compose down

docker-reset:
	docker compose down -v
	docker compose up -d --build

# Kafka
kafka-topics:
	docker exec $(KAFKA_CONTAINER) kafka-topics --bootstrap-server $(KAFKA_BOOTSTRAP) --list

kafka-describe:
	docker exec $(KAFKA_CONTAINER) kafka-topics --bootstrap-server $(KAFKA_BOOTSTRAP) --topic $(KAFKA_TOPIC) --describe

kafka-consume:
	docker exec -it $(KAFKA_CONTAINER) kafka-console-consumer --bootstrap-server $(KAFKA_BOOTSTRAP) --topic $(KAFKA_TOPIC) --from-beginning

kafka-lag:
	docker exec $(KAFKA_CONTAINER) kafka-consumer-groups --bootstrap-server $(KAFKA_BOOTSTRAP) --group $(KAFKA_GROUP) --describe

# Go
tidy:
	go mod tidy

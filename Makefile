.PHONY: help install dev build check fmt

help:
	@echo "install  Install frontend dependencies"
	@echo "dev      Start the frontend development server"
	@echo "build    Build backend and frontend"
	@echo "check    Check backend and frontend"
	@echo "fmt      Format Go source files"
	@echo "infra-up / infra-down / infra-ps / infra-logs / infra-config  Manage local infrastructure"

install:
	npm --prefix frontend ci

dev:
	npm --prefix frontend run dev

build:
	cd backend && go build ./...
	npm --prefix frontend run build

check:
	cd backend && go test ./...
	npm --prefix frontend run lint
	npm --prefix frontend run build

fmt:
	cd backend && go fmt ./...

# Always resolve .env from the repository root.
COMPOSE = docker compose --env-file .env -f deployments/docker-compose.yml
.PHONY: infra-config infra-up infra-down infra-ps infra-logs
infra-config:
	$(COMPOSE) config --quiet
infra-up:
	$(COMPOSE) up -d --wait --wait-timeout 240
infra-down:
	$(COMPOSE) down
infra-ps:
	$(COMPOSE) ps
infra-logs:
	$(COMPOSE) logs --tail=100 -f

.PHONY: proto run-user run-product
proto:
	protoc -I api/proto --go_out=backend --go_opt=module=remarket/backend --go-grpc_out=backend --go-grpc_opt=module=remarket/backend api/proto/user/v1/user.proto api/proto/product/v1/product.proto api/proto/order/v1/order.proto api/proto/search/v1/search.proto api/proto/feed/v1/feed.proto
run-user:
	cd backend && go run ./cmd/user
run-product:
	cd backend && go run ./cmd/product

.PHONY: run-gateway
run-gateway:
	cd backend && go run ./cmd/gateway

.PHONY: run-order
run-order:
	cd backend && go run ./cmd/order

.PHONY: kafka-up kafka-topics kafka-check
kafka-up:
	$(COMPOSE) up -d --wait --wait-timeout 240 kafka
	$(COMPOSE) run --rm kafka-init
kafka-topics:
	$(COMPOSE) run --rm kafka-init
kafka-check:
	set -a; . ./.env; set +a; cd backend && go run ./cmd/kafka-check

.PHONY: redis-check
redis-check:
	set -a; . ./.env; set +a; cd backend && go run ./cmd/redis-check

.PHONY: run-event-consumer
run-event-consumer:
	cd backend && go run ./cmd/event-consumer

# Fault infrastructure has its own project, network, ports and volumes.
DAY3_COMPOSE = docker compose --env-file .env -p remarket-day3-test -f deployments/docker-compose.day3-test.yml
.PHONY: day3-test-config day3-test-up day3-test-down day3-test
day3-test-config:
	$(DAY3_COMPOSE) config --quiet
day3-test-up:
	$(DAY3_COMPOSE) up -d --wait --wait-timeout 240 redis kafka
	$(DAY3_COMPOSE) run --rm kafka-init
day3-test-down:
	$(DAY3_COMPOSE) down
day3-test:
	@test -n "$$REMARKET_TEST_MYSQL_DSN" || (echo "Set REMARKET_TEST_MYSQL_DSN to a test database administrator"; exit 1)
	cd backend && REDIS_INTEGRATION=1 KAFKA_INTEGRATION=1 go test -race ./internal/eventtest ./internal/product/service ./internal/product/cache ./internal/product/repository ./internal/product/handler ./internal/gateway ./pkg/redis ./pkg/ratelimit ./pkg/kafka ./pkg/outbox -count=1

.PHONY: run-search-consumer
run-search-consumer:
	cd backend && go run ./cmd/search-consumer

.PHONY: run-search
run-search:
	cd backend && go run ./cmd/search

# Feed 基础查询进程。
run-feed:
	cd backend && go run ./cmd/feed

.PHONY: run-feed-consumer
run-feed-consumer:
	cd backend && go run ./cmd/feed-consumer

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

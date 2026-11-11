.PHONY: help install dev build check fmt

help:
	@echo "install  Install frontend dependencies"
	@echo "dev      Start the frontend development server"
	@echo "build    Build backend and frontend"
	@echo "check    Check backend and frontend"
	@echo "fmt      Format Go source files"

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

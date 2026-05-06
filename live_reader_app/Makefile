APP := go run .

.PHONY: help deps run dev

help:
	@echo "Usage:"
	@echo "  make deps   # install/update Go dependencies"
	@echo "  make run    # start reader app on localhost:8080"
	@echo "  make dev    # deps + run"

deps:
	@go mod tidy

run:
	@$(APP)

dev: deps run

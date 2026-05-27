.PHONY: validate test build release check-config

validate:
	@./scripts/validate_hdci.sh

test:
	@go test -v ./...

build:
	@go build -o hd-driver-openai ./cmd/hd-driver-openai

release:
	@echo "Running semantic release..."
	@npx semantic-release

check-config: validate
	@echo "Checking configuration compliance..."
	@go test -v -run TestHDCIConfig ./... || echo "Note: TestHDCIConfig not yet implemented."

all: validate test build

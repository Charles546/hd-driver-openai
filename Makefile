validate:
	@./scripts/validate_hdci.sh
test:
	@go test -v ./...
build:
	@go build -o hd-driver-openai ./cmd/hd-driver-openai
all: validate test build

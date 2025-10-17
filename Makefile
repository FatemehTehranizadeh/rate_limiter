# Run all tests
test:
	go test ./... -v

# Run race detector
race:
	go test ./... -race -v

# Run linter
lint:
	golangci-lint run

# Format code
fmt:
	go fmt ./...

# Run demo app
run:
	go run ./cmd/demo

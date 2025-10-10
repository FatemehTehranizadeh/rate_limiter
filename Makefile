.PHONY: test vet lint

test:
  go test ./...

vet:
  go vet ./...

lint:
  golangci-lint run


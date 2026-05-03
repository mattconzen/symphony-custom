.PHONY: test lint vet run tidy

test:; go test ./... -race -count=1
vet:;  go vet ./...
lint:; golangci-lint run
run:;  go run ./cmd/symphony
tidy:; go mod tidy

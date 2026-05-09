.PHONY: test lint vet run tidy e2e

test:; go test ./... -race -count=1
e2e:;  go test ./e2e/... -race -count=1 -timeout=120s -v
vet:;  go vet ./...
lint:; golangci-lint run
run:;  go run ./cmd/symphony
tidy:; go mod tidy

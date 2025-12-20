clean:
	go clean
	go mod tidy

test:
	go test ./...

test/cover:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

test/json:
	go test -json ./... > test_results.json

benchmark:
	go test -bench=. ./... -benchmem

deps:
	go mod download
	go mod verify

vet:
	go vet ./...

fmt:
	go fmt ./...
	golangci-lint fmt

lint: fmt vet
	golangci-lint run
	make vet

BINARY := bin/gobackup

.PHONY: all build test test-integration vet fmt clean release-check release-snapshot

all: vet test build

build:
	go build -o $(BINARY) ./cmd/gobackup

test:
	go test -race ./...

# Runs the S3 tests against a local MinIO server built from source on first use.
test-integration:
	./scripts/integration-test.sh

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin

release-check:
	goreleaser check

release-snapshot:
	goreleaser release --snapshot --clean

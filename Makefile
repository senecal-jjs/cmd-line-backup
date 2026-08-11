BINARY := bin/gobackup

.PHONY: all build test vet fmt clean release-check release-snapshot

all: vet test build

build:
	go build -o $(BINARY) ./cmd/gobackup

test:
	go test -race ./...

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

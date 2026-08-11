BINARY := bin/gobackup

.PHONY: all build test vet fmt clean

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

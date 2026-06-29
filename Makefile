BINARY := omni-notify
PKG    := ./...

.PHONY: all build run test vet fmt fmt-check tidy clean cover docker compose-up compose-down

all: fmt vet test build

build:
	go build -o $(BINARY) ./cmd/omni-notify

run: build
	./$(BINARY) -config config.yaml

test:
	go test $(PKG)

cover:
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

vet:
	go vet $(PKG)

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy

genkey: build
	./$(BINARY) genkey

clean:
	rm -f $(BINARY) coverage.out *.db *.db-wal *.db-shm

docker:
	docker build -t omni-notify:latest .

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down

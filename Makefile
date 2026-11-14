VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test vet integration docker release-check clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gitmirror ./cmd/gitmirror

vet:
	go vet ./...

test: vet
	go test -race ./...

integration:
	GITMIRROR_REQUIRE_DOCKER=1 go test -tags integration -count=1 -timeout 30m ./internal/integration/

docker:
	docker build -t gitmirror:dev --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

release-check:
	go run github.com/goreleaser/goreleaser/v2@latest check

clean:
	rm -rf bin dist

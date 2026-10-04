.PHONY: build generate test lint fmt ci install-test kind-test kind-sandbox-test release-snapshot

build:
	@mkdir -p dist
	go build -o dist/overload ./cmd/overload

generate:
	go generate ./...

test:
	go test ./...

lint:
	go tool golangci-lint run

fmt:
	gofmt -w .

ci: generate
	@test -z "$$(gofmt -l .)" || (echo 'unformatted Go files'; exit 1)
	go vet ./...
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) build

install-test:
	OVERLOAD_INSTALL_TEST=1 go test -count=1 -timeout 15m -v ./deploy/installtest/

kind-test:
	OVERLOAD_KIND_TEST=1 go test -count=1 -timeout 20m -run TestKindInstall -v ./deploy/kindtest/

kind-sandbox-test:
	OVERLOAD_KIND_SANDBOX_TEST=1 go test -count=1 -timeout 30m -run TestKindSandboxReview -v ./deploy/kindtest/

release-snapshot:
	goreleaser release --snapshot --clean --skip=publish

fmt:
    gofmt -w cmd internal
    nixfmt flake.nix

fmt-check:
    test -z "$(gofmt -l cmd internal)"
    nixfmt --check flake.nix

lint:
    go vet ./cmd/... ./internal/...
    staticcheck ./...
    actionlint

test:
    go test -race ./...

check: fmt-check lint test

build:
    go build -o bin/google-messages-multidevice-bridge ./cmd/google-messages-multidevice-bridge

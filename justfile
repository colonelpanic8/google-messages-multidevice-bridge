fmt:
    gofmt -w cmd internal third_party/mautrix-gmessages/pkg/libgm
    nixfmt flake.nix
    prettier --write internal/api/web

fmt-check:
    test -z "$(gofmt -l cmd internal third_party/mautrix-gmessages/pkg/libgm)"
    nixfmt --check flake.nix
    prettier --check internal/api/web

lint:
    go vet ./cmd/... ./internal/... go.mau.fi/mautrix-gmessages/pkg/libgm/...
    staticcheck ./...
    actionlint
    node --check internal/api/web/app.js
    node --check internal/api/web/stream.mjs

test:
    go test -race ./... go.mau.fi/mautrix-gmessages/pkg/libgm/...
    node --test internal/api/web/*.test.mjs

check: fmt-check lint test

build:
    go build -o bin/google-messages-multidevice-bridge ./cmd/google-messages-multidevice-bridge

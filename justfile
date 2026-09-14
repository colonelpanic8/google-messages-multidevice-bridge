fmt:
    gofmt -w cmd internal third_party/mautrix-gmessages/pkg/libgm
    nixfmt flake.nix nix/*.nix
    prettier --write internal/api/web internal/api/pairinghelper tools

fmt-check:
    test -z "$(gofmt -l cmd internal third_party/mautrix-gmessages/pkg/libgm)"
    nixfmt --check flake.nix nix/*.nix
    prettier --check internal/api/web internal/api/pairinghelper tools

lint:
    go vet ./cmd/... ./internal/... go.mau.fi/mautrix-gmessages/pkg/libgm/...
    staticcheck ./...
    actionlint
    node --check internal/api/web/app.js
    node --check internal/api/web/stream.mjs
    node --check internal/api/web/view.mjs
    node --check internal/api/web/emoji.mjs
    node --check internal/api/web/emoji-data.mjs
    node --check internal/api/pairinghelper/setup.mjs
    node --check internal/api/pairinghelper/service-worker.js

test:
    go test -race ./... go.mau.fi/mautrix-gmessages/pkg/libgm/...
    node --test internal/api/web/*.test.mjs internal/api/pairinghelper/*.test.mjs

check: fmt-check lint test

# Refetches Unicode and CLDR data, so it needs network access.
gen-emoji:
    node tools/gen-emoji.mjs
    prettier --write internal/api/web/emoji-data.mjs

build:
    go build -o bin/google-messages-multidevice-bridge ./cmd/google-messages-multidevice-bridge

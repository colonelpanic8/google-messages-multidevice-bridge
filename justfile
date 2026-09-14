fmt:
    gofmt -w cmd internal third_party/mautrix-gmessages/pkg/libgm
    nixfmt flake.nix nix/*.nix
    prettier --write client internal/api/pairinghelper tools

fmt-check:
    test -z "$(gofmt -l cmd internal third_party/mautrix-gmessages/pkg/libgm)"
    nixfmt --check flake.nix nix/*.nix
    prettier --check client internal/api/pairinghelper tools

lint:
    go vet ./cmd/... ./internal/... go.mau.fi/mautrix-gmessages/pkg/libgm/...
    staticcheck ./...
    actionlint
    node --check client/app.js
    node --check client/stream.mjs
    node --check client/view.mjs
    node --check client/emoji.mjs
    node --check client/emoji-data.mjs
    node --check internal/api/pairinghelper/setup.mjs
    node --check internal/api/pairinghelper/service-worker.js

test:
    go test -race ./... go.mau.fi/mautrix-gmessages/pkg/libgm/...
    node --test client/*.test.mjs internal/api/pairinghelper/*.test.mjs

check: fmt-check lint test

# Rasterizes the shipped PNG icons from the SVG sources. Needs rsvg-convert.
gen-icons:
    ./tools/gen-icons.sh

# Refetches Unicode and CLDR data, so it needs network access.
gen-emoji:
    node tools/gen-emoji.mjs
    prettier --write client/emoji-data.mjs

build:
    go build -o bin/google-messages-multidevice-bridge ./cmd/google-messages-multidevice-bridge

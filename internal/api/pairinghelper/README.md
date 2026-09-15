# Google Messages Bridge Pairing Helper

This is an unpacked Chromium Manifest V3 extension. It has no build step and does not store Google cookies, pairing tickets, API bearer tokens, or telemetry. Optional background recovery stores the bridge origin and a pairing-only token in extension storage.

## Install from source

1. Extract the pairing-helper source ZIP supplied by the bridge.
2. Open `chrome://extensions` in Chrome or Chromium.
3. Enable **Developer mode**.
4. Click **Load unpacked** and select the extracted directory containing `manifest.json`.
5. Pin **Google Messages Bridge Pairing Helper** and click its toolbar action.

Leave **Enable background recovery** checked during a successful handoff to let any bridge client initiate future reconnections. Chrome can run the pairing helper's service worker without keeping the setup tab open.

The runtime ZIP should copy these files unchanged: `manifest.json`, `service-worker.js`, `setup.html`, `setup.css`, `setup.mjs`, and `core.mjs`. `README.md` may also be included. No compilation, dependency installation, or generated artifact is needed.

## Test

From the repository root:

```sh
node --test internal/api/pairinghelper/core.test.mjs
node --check internal/api/pairinghelper/service-worker.js
node --check internal/api/pairinghelper/setup.mjs
node --check internal/api/pairinghelper/core.mjs
```

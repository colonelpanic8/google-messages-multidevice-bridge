# Google Messages Bridge Pairing Helper

This is an unpacked Chromium Manifest V3 extension. It has no build step and does not store credentials, cookies, tickets, settings, or telemetry.

## Install from source

1. Extract the pairing-helper source ZIP supplied by the bridge.
2. Open `chrome://extensions` in Chrome or Chromium.
3. Enable **Developer mode**.
4. Click **Load unpacked** and select the extracted directory containing `manifest.json`.
5. Pin **Google Messages Bridge Pairing Helper**, click its toolbar action, and keep the setup tab open.

The runtime ZIP should copy these files unchanged: `manifest.json`, `service-worker.js`, `setup.html`, `setup.css`, `setup.mjs`, and `core.mjs`. `README.md` may also be included. No compilation, dependency installation, or generated artifact is needed.

## Test

From the repository root:

```sh
node --test internal/api/pairinghelper/core.test.mjs
node --check internal/api/pairinghelper/service-worker.js
node --check internal/api/pairinghelper/setup.mjs
node --check internal/api/pairinghelper/core.mjs
```

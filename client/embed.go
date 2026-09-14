// Package client carries the web client. The bridge embeds this tree so a
// browser can install the app from it, and the desktop app bundles the same
// tree so its window never loads code from the network.
package client

import "embed"

//go:embed index.html style.css app.js stream.mjs view.mjs emoji.mjs emoji-data.mjs sw.js manifest.webmanifest favicon.svg icons
var Files embed.FS

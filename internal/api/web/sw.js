// Version is substituted by the server from a hash of the embedded assets, so a
// new build always invalidates the cached shell.
const VERSION = "__ASSET_VERSION__";
const SHELL = `shell-${VERSION}`;
const ASSETS = [
  "/",
  "/style.css",
  "/app.js",
  "/stream.mjs",
  "/view.mjs",
  "/emoji.mjs",
  "/emoji-data.mjs",
  "/manifest.webmanifest",
  "/favicon.svg",
  "/icons/icon-192.png",
  "/icons/icon-512.png",
  "/icons/icon-maskable-512.png",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(SHELL)
      .then((cache) => cache.addAll(ASSETS))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys.filter((key) => key !== SHELL).map((key) => caches.delete(key)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

// Only the static shell is cached. Authenticated /v1/ traffic always goes to the
// network, so message content is never written to the browser cache.
self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (event.request.method !== "GET" || url.origin !== self.location.origin)
    return;
  if (url.pathname.startsWith("/v1/")) return;
  if (event.request.mode === "navigate") {
    event.respondWith(
      fetch(event.request).catch(() => caches.match("/", { cacheName: SHELL })),
    );
    return;
  }
  if (!ASSETS.includes(url.pathname)) return;
  event.respondWith(
    caches
      .match(event.request, { cacheName: SHELL })
      .then((hit) => hit || fetch(event.request)),
  );
});

self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    data = {};
  }
  event.waitUntil(
    self.registration.showNotification(data.title || "New message", {
      body: data.body || "",
      tag: data.tag || undefined,
      icon: "/icons/icon-192.png",
      badge: "/icons/icon-192.png",
      data: { conversation: data.conversation || "" },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const conversation = event.notification.data?.conversation || "";
  event.waitUntil(
    self.clients
      .matchAll({ type: "window", includeUncontrolled: true })
      .then((clients) => {
        for (const client of clients) {
          if (new URL(client.url).origin !== self.location.origin) continue;
          client.postMessage({ type: "open-conversation", conversation });
          return client.focus();
        }
        return self.clients.openWindow(
          conversation
            ? `/?conversation=${encodeURIComponent(conversation)}`
            : "/",
        );
      }),
  );
});

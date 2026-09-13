export const GOOGLE_SIGN_IN_URL =
  "https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config";

export const COOKIE_SPECS = Object.freeze([
  Object.freeze({ name: "SID", url: "https://google.com/", required: true }),
  Object.freeze({ name: "HSID", url: "https://google.com/", required: true }),
  Object.freeze({
    name: "OSID",
    url: "https://messages.google.com/",
    required: true,
  }),
  Object.freeze({ name: "SSID", url: "https://google.com/", required: true }),
  Object.freeze({ name: "APISID", url: "https://google.com/", required: true }),
  Object.freeze({
    name: "SAPISID",
    url: "https://google.com/",
    required: true,
  }),
  Object.freeze({
    name: "__Secure-1PSIDTS",
    url: "https://google.com/",
    required: false,
  }),
]);

export class PairingHelperError extends Error {
  constructor(code) {
    super(code);
    this.name = "PairingHelperError";
    this.code = code;
  }
}

function parseIPv4(hostname) {
  const parts = hostname.split(".");
  if (parts.length !== 4 || parts.some((part) => !/^\d{1,3}$/.test(part))) {
    return null;
  }
  const octets = parts.map(Number);
  return octets.every((octet) => octet >= 0 && octet <= 255) ? octets : null;
}

function isAllowedHTTPHost(hostname) {
  const lower = hostname.toLowerCase();
  if (lower === "localhost" || lower === "[::1]" || lower === "::1") {
    return true;
  }
  const octets = parseIPv4(lower);
  if (!octets) {
    return false;
  }
  return (
    octets[0] === 127 ||
    (octets[0] === 100 && octets[1] >= 64 && octets[1] <= 127)
  );
}

export function validateBridgeURL(rawURL) {
  let url;
  try {
    url = new URL(rawURL.trim());
  } catch {
    throw new PairingHelperError("invalid-bridge-url");
  }
  if (
    (url.protocol !== "https:" && url.protocol !== "http:") ||
    url.username ||
    url.password ||
    url.href !== `${url.origin}/`
  ) {
    throw new PairingHelperError("invalid-bridge-url");
  }
  if (url.protocol === "http:" && !isAllowedHTTPHost(url.hostname)) {
    throw new PairingHelperError("insecure-bridge-url");
  }
  return url.origin;
}

export function requestedOrigins(bridgeOrigin) {
  const bridge = new URL(bridgeOrigin);
  return [
    ...new Set([
      "https://google.com/*",
      "https://messages.google.com/*",
      `${bridge.protocol}//${bridge.host}/*`,
    ]),
  ];
}

export async function collectAllowedCookies(getCookie) {
  const cookies = {};
  for (const spec of COOKIE_SPECS) {
    const cookie = await getCookie({ name: spec.name, url: spec.url });
    if (cookie && typeof cookie.value === "string" && cookie.value !== "") {
      cookies[spec.name] = cookie.value;
    } else if (spec.required) {
      throw new PairingHelperError("missing-google-cookies");
    }
  }
  return cookies;
}

export async function handoffCredentials({
  bridgeURL,
  ticket,
  requestPermissions,
  removePermissions,
  getCookie,
  fetchImpl,
}) {
  const origin = validateBridgeURL(bridgeURL);
  if (typeof ticket !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(ticket)) {
    throw new PairingHelperError("invalid-ticket");
  }
  const origins = requestedOrigins(origin);
  let granted = false;
  let cookies;
  let body;
  try {
    granted = await requestPermissions({ origins });
    if (!granted) {
      throw new PairingHelperError("permission-denied");
    }
    cookies = await collectAllowedCookies(getCookie);
    body = JSON.stringify({ ticket, cookies });
    const response = await fetchImpl(`${origin}/v1/pairing/credentials`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body,
      redirect: "error",
      credentials: "omit",
      cache: "no-store",
      referrerPolicy: "no-referrer",
    });
    if (response.status !== 202) {
      throw new PairingHelperError("handoff-rejected");
    }
    return { origin };
  } finally {
    cookies = undefined;
    body = undefined;
    ticket = "";
    if (granted) {
      try {
        await removePermissions({ origins });
      } catch {}
    }
  }
}

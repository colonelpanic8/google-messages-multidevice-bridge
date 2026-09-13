import assert from "node:assert/strict";
import test from "node:test";

import {
  COOKIE_SPECS,
  PairingHelperError,
  collectAllowedCookies,
  handoffCredentials,
  requestedOrigins,
  validateBridgeURL,
} from "./core.mjs";

const TEST_TICKET = "A".repeat(43);

test("bridge URL validation canonicalizes safe origins", () => {
  const accepted = new Map([
    ["https://bridge.example", "https://bridge.example"],
    ["https://bridge.example:8443/", "https://bridge.example:8443"],
    ["http://localhost:8080/", "http://localhost:8080"],
    ["http://127.23.45.67:8080/", "http://127.23.45.67:8080"],
    ["http://[::1]:8080/", "http://[::1]:8080"],
    ["http://100.64.0.1:8080/", "http://100.64.0.1:8080"],
    ["http://100.127.255.254/", "http://100.127.255.254"],
  ]);
  for (const [input, expected] of accepted) {
    assert.equal(validateBridgeURL(input), expected, input);
  }
});

test("bridge URL validation rejects authority confusion and unsafe HTTP", () => {
  const rejected = [
    "",
    "bridge.example",
    "ftp://bridge.example/",
    "https://user@bridge.example/",
    "https://bridge.example/path",
    "https://bridge.example/?",
    "https://bridge.example/#",
    "https://bridge.example/?#",
    "https://bridge.example/?ticket=secret",
    "https://bridge.example/#secret",
    "http://bridge.example/",
    "http://tailscale-host/",
    "http://100.63.255.255/",
    "http://100.128.0.1/",
    "http://192.168.1.10/",
  ];
  for (const input of rejected) {
    assert.throws(() => validateBridgeURL(input), PairingHelperError, input);
  }
});

test("permission origins are exact and deduplicated", () => {
  assert.deepEqual(requestedOrigins("https://bridge.example:8443"), [
    "https://google.com/*",
    "https://messages.google.com/*",
    "https://bridge.example:8443/*",
  ]);
  assert.deepEqual(requestedOrigins("https://google.com"), [
    "https://google.com/*",
    "https://messages.google.com/*",
  ]);
});

test("cookie collection queries only the upstream allowlist", async () => {
  const queries = [];
  const cookies = await collectAllowedCookies(async (details) => {
    queries.push(details);
    if (details.name === "__Secure-1PSIDTS") {
      return undefined;
    }
    return { value: `value-${details.name}` };
  });

  assert.deepEqual(
    queries,
    COOKIE_SPECS.map(({ name, url }) => ({ name, url })),
  );
  assert.deepEqual(Object.keys(cookies), [
    "SID",
    "HSID",
    "OSID",
    "SSID",
    "APISID",
    "SAPISID",
  ]);
  assert.equal(cookies.OSID, "value-OSID");
  assert.equal(cookies["__Secure-1PSIDTS"], undefined);
});

test("cookie collection rejects a missing required cookie generically", async () => {
  await assert.rejects(
    collectAllowedCookies(async ({ name }) =>
      name === "HSID" ? undefined : { value: "present" },
    ),
    (error) => {
      assert.equal(error.code, "missing-google-cookies");
      assert.doesNotMatch(error.message, /HSID|cookie name/i);
      return true;
    },
  );
});

test("handoff requests exact permissions and posts no bearer credentials", async () => {
  const calls = [];
  let postedURL;
  let postedOptions;
  const result = await handoffCredentials({
    bridgeURL: "https://bridge.example:8443/",
    ticket: TEST_TICKET,
    requestPermissions: async (request) => {
      calls.push(["permissions", request]);
      return true;
    },
    removePermissions: async (request) => {
      calls.push(["remove", request]);
      return true;
    },
    getCookie: async ({ name, url }) => {
      calls.push(["cookie", { name, url }]);
      return name === "__Secure-1PSIDTS"
        ? undefined
        : { value: `secret-${name}` };
    },
    fetchImpl: async (url, options) => {
      calls.push(["fetch"]);
      postedURL = url;
      postedOptions = options;
      return { status: 202 };
    },
  });

  const origins = [
    "https://google.com/*",
    "https://messages.google.com/*",
    "https://bridge.example:8443/*",
  ];
  assert.deepEqual(result, { origin: "https://bridge.example:8443" });
  assert.deepEqual(calls[0], ["permissions", { origins }]);
  assert.deepEqual(calls.at(-1), ["remove", { origins }]);
  assert.equal(postedURL, "https://bridge.example:8443/v1/pairing/credentials");
  assert.equal(postedOptions.method, "POST");
  assert.deepEqual(postedOptions.headers, {
    "Content-Type": "application/json",
  });
  assert.equal(Object.hasOwn(postedOptions.headers, "Authorization"), false);
  assert.equal(postedOptions.redirect, "error");
  assert.equal(postedOptions.credentials, "omit");
  assert.equal(postedOptions.cache, "no-store");
  assert.equal(postedOptions.referrerPolicy, "no-referrer");
  assert.deepEqual(JSON.parse(postedOptions.body), {
    ticket: TEST_TICKET,
    cookies: {
      SID: "secret-SID",
      HSID: "secret-HSID",
      OSID: "secret-OSID",
      SSID: "secret-SSID",
      APISID: "secret-APISID",
      SAPISID: "secret-SAPISID",
    },
  });
});

test("handoff never reads cookies when permission is denied", async () => {
  let cookieReads = 0;
  let fetches = 0;
  await assert.rejects(
    handoffCredentials({
      bridgeURL: "https://bridge.example/",
      ticket: TEST_TICKET,
      requestPermissions: async () => false,
      removePermissions: async () => true,
      getCookie: async () => {
        cookieReads++;
      },
      fetchImpl: async () => {
        fetches++;
      },
    }),
    (error) => error.code === "permission-denied",
  );
  assert.equal(cookieReads, 0);
  assert.equal(fetches, 0);
});

test("handoff revokes permissions and does not post when cookies are missing", async () => {
  let removals = 0;
  let fetches = 0;
  await assert.rejects(
    handoffCredentials({
      bridgeURL: "http://100.100.10.20:8080/",
      ticket: TEST_TICKET,
      requestPermissions: async () => true,
      removePermissions: async () => {
        removals++;
        return true;
      },
      getCookie: async ({ name }) =>
        name === "SID" ? undefined : { value: "present" },
      fetchImpl: async () => {
        fetches++;
      },
    }),
    (error) => error.code === "missing-google-cookies",
  );
  assert.equal(removals, 1);
  assert.equal(fetches, 0);
});

test("handoff treats every non-202 response as a generic rejection", async () => {
  let removals = 0;
  await assert.rejects(
    handoffCredentials({
      bridgeURL: "https://bridge.example/",
      ticket: TEST_TICKET,
      requestPermissions: async () => true,
      removePermissions: async () => {
        removals++;
        return true;
      },
      getCookie: async () => ({ value: "present" }),
      fetchImpl: async () => ({ status: 401 }),
    }),
    (error) => error.code === "handoff-rejected",
  );
  assert.equal(removals, 1);
});

test("handoff does not retry a failed one-time request", async () => {
  let fetches = 0;
  let removals = 0;
  await assert.rejects(
    handoffCredentials({
      bridgeURL: "https://bridge.example/",
      ticket: TEST_TICKET,
      requestPermissions: async () => true,
      removePermissions: async () => {
        removals++;
        return true;
      },
      getCookie: async () => ({ value: "present" }),
      fetchImpl: async () => {
        fetches++;
        throw new TypeError("synthetic network failure");
      },
    }),
    TypeError,
  );
  assert.equal(fetches, 1);
  assert.equal(removals, 1);
});

test("handoff rejects malformed tickets before requesting permissions", async () => {
  let permissionRequests = 0;
  await assert.rejects(
    handoffCredentials({
      bridgeURL: "https://bridge.example/",
      ticket: "not-a-pairing-ticket",
      requestPermissions: async () => {
        permissionRequests++;
        return true;
      },
      removePermissions: async () => true,
      getCookie: async () => ({ value: "present" }),
      fetchImpl: async () => ({ status: 202 }),
    }),
    (error) => error.code === "invalid-ticket",
  );
  assert.equal(permissionRequests, 0);
});

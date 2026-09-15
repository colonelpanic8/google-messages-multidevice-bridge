import {
  GOOGLE_SIGN_IN_URL,
  PairingHelperError,
  handoffCredentials,
} from "./core.mjs";

const form = document.querySelector("#pairing-form");
const bridgeInput = document.querySelector("#bridge-url");
const ticketInput = document.querySelector("#pairing-ticket");
const signInButton = document.querySelector("#sign-in");
const connectButton = document.querySelector("#connect");
const backgroundRecovery = document.querySelector("#background-recovery");
const status = document.querySelector("#status");

function setStatus(message, kind = "") {
  status.textContent = message;
  status.dataset.kind = kind;
}

function publicError(error) {
  if (!(error instanceof PairingHelperError)) {
    return "The secure handoff failed. Check the bridge and try again.";
  }
  switch (error.code) {
    case "invalid-bridge-url":
      return "Enter only the bridge origin, without credentials, a path, query, or fragment.";
    case "insecure-bridge-url":
      return "Use HTTPS. HTTP is allowed only for loopback or a literal Tailscale 100.64.0.0/10 address.";
    case "invalid-ticket":
      return "Enter the one-time pairing ticket shown by the bridge.";
    case "permission-denied":
      return "Permission was not granted. No cookies were read.";
    case "missing-google-cookies":
      return "Required Google sign-in cookies are unavailable. Sign in to Google Messages and try again.";
    case "handoff-rejected":
      return "The bridge rejected the handoff. Start pairing again to obtain a fresh ticket.";
    default:
      return "The secure handoff failed. Check the bridge and try again.";
  }
}

signInButton.addEventListener("click", () => {
  void chrome.tabs.create({ url: GOOGLE_SIGN_IN_URL });
});

form.addEventListener("submit", (event) => {
  event.preventDefault();
});

connectButton.addEventListener("click", async () => {
  if (connectButton.disabled) {
    return;
  }

  const bridgeURL = bridgeInput.value;
  let ticket = ticketInput.value;
  ticketInput.value = "";
  connectButton.disabled = true;
  signInButton.disabled = true;
  setStatus("Requesting access for this one handoff…");

  try {
    const keepBackground = backgroundRecovery.checked;
    const result = await handoffCredentials({
      bridgeURL,
      ticket,
      requestPermissions: (request) => chrome.permissions.request(request),
      removePermissions: (request) => chrome.permissions.remove(request),
      getCookie: (details) => chrome.cookies.get(details),
      fetchImpl: fetch,
      enrollAgent: keepBackground,
      retainPermissions: keepBackground,
    });
    if (keepBackground) {
      await chrome.storage.local.set({
        pairingAgent: {
          bridgeURL: result.origin,
          agentToken: result.agentToken,
        },
      });
      await chrome.alarms.create("pairing-agent", { periodInMinutes: 1 });
    } else {
      await chrome.storage.local.remove("pairingAgent");
      await chrome.alarms.clear("pairing-agent");
    }
    bridgeInput.value = "";
    setStatus(
      keepBackground
        ? "Credentials accepted. Background recovery is enabled; future reconnects can start from any bridge client."
        : "Credentials accepted. Return to the bridge and follow its phone emoji prompt.",
      "success",
    );
  } catch (error) {
    setStatus(publicError(error), "error");
  } finally {
    ticket = "";
    connectButton.disabled = false;
    signInButton.disabled = false;
  }
});

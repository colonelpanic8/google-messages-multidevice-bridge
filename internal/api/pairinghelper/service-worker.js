import { pollPairingAgent } from "./core.mjs";

async function ensurePairingAlarm() {
  const { pairingAgent } = await chrome.storage.local.get("pairingAgent");
  if (pairingAgent) {
    await chrome.alarms.create("pairing-agent", { periodInMinutes: 1 });
  }
}

void ensurePairingAlarm();
chrome.runtime.onStartup.addListener(() => void ensurePairingAlarm());
chrome.runtime.onInstalled.addListener(() => void ensurePairingAlarm());

chrome.action.onClicked.addListener(() => {
  void chrome.tabs.create({ url: chrome.runtime.getURL("setup.html") });
});

chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name !== "pairing-agent") return;
  void chrome.storage.local.get("pairingAgent").then(({ pairingAgent }) => {
    if (!pairingAgent) return;
    return pollPairingAgent({
      ...pairingAgent,
      hasPermissions: (request) => chrome.permissions.contains(request),
      removePermissions: (request) => chrome.permissions.remove(request),
      getCookie: (details) => chrome.cookies.get(details),
      fetchImpl: fetch,
    }).catch(() => {});
  });
});

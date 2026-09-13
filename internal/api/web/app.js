import {
  createParser,
  mergeMessages,
  newConversationRequest,
  newReactionRequest,
  splitRequests,
  parseRecipients,
  validateAttachments,
} from "/stream.mjs";

const $ = (id) => document.getElementById(id);
const reactionChoices = ["👍", "❤️", "😂", "😮", "😢", "😠"];
const drafts = new Map();
const draftFiles = new Map();
const messageUpdates = new Map();
const pendingReactions = new Map();

let token = "",
  abort,
  selected = "",
  conversations = [],
  outbox = [],
  messages = [],
  historyJobs = [],
  before = "";
let cursor = 0,
  targetCursor = 0,
  refreshTask,
  generation = 0;
let sending = false,
  creatingConversation = false,
  pendingSend,
  pendingConversation,
  createdConversation,
  selectedFiles = [];
let providerState = "offline",
  typingUntil = 0,
  typingConversation = "",
  typingTimer,
  lastTypingAt = 0,
  importing = 0;
let pairingState = {},
  pairingPoll,
  pairingPanelOpen = false;

const notice = (message) => {
  $("notice").textContent = message;
};
function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}
function formatSize(size) {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${Math.ceil(size / 1024)} KiB`;
  return `${(size / (1024 * 1024)).toFixed(1)} MiB`;
}
async function request(path, options = {}) {
  const headers = { Authorization: `Bearer ${token}`, ...options.headers };
  if (
    typeof options.body === "string" &&
    !Object.keys(headers).some((key) => key.toLowerCase() === "content-type")
  )
    headers["Content-Type"] = "application/json";
  const response = await fetch(path, {
    ...options,
    signal: abort?.signal,
    headers,
  });
  if (!response.ok) {
    const detail = (await response.text()).trim();
    const error = new Error(
      response.status === 401
        ? "Token rejected. Lock and unlock with your bridge token."
        : detail || `Request failed (${response.status}).`,
    );
    error.status = response.status;
    throw error;
  }
  if (response.status === 204) return null;
  const body = await response.text();
  return body ? JSON.parse(body) : null;
}
// Google repeats a participant record per SIM or per self-chat leg.
function others(conversation) {
  const seen = new Set();
  return (conversation.participants || []).filter((participant) => {
    const key = participant.address || participant.name || participant.id;
    if (participant.is_me || seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}
function name(conversation) {
  return (
    conversation.name ||
    others(conversation)
      .map(
        (participant) =>
          participant.name || participant.address || participant.id,
      )
      .join(", ") ||
    conversation.id
  );
}
function renderConversations() {
  const search = $("search").value.toLowerCase();
  $("conversations").replaceChildren();
  for (const conversation of conversations.filter((item) =>
    name(item).toLowerCase().includes(search),
  )) {
    const button = el(
      "button",
      undefined,
      `conversation ${conversation.id === selected ? "active" : ""} ${conversation.unread ? "unread" : ""}`,
    );
    button.type = "button";
    button.append(
      el("strong", name(conversation)),
      el("small", conversation.preview || "No recent preview"),
    );
    button.onclick = () => select(conversation.id);
    $("conversations").append(button);
  }
  if (!conversations.length)
    $("conversations").append(el("p", "No conversations stored yet.", "hint"));
}
function reactionKey(conversationID, messageID, emoji, remove = false) {
  return [conversationID, messageID, emoji, remove].join("\u0000");
}
function renderReactionActions(container, message, conversation) {
  if (message.deleted || conversation.read_only) return;
  const picker = el("details", undefined, "reaction-picker");
  picker.append(el("summary", "React"));
  const choices = el("div", undefined, "reaction-choices");
  for (const emoji of reactionChoices) {
    const button = el("button", emoji);
    button.type = "button";
    button.setAttribute("aria-label", `React ${emoji}`);
    button.onclick = () => {
      picker.open = false;
      void startReaction(message.id, emoji);
    };
    choices.append(button);
  }
  picker.append(choices);
  container.append(picker);
  for (const [key, pending] of pendingReactions) {
    if (
      pending.conversationID !== selected ||
      pending.request.body.message_id !== message.id
    )
      continue;
    const row = el("div", undefined, "pending-reaction");
    row.append(
      el("span", `${pending.request.body.emoji} reaction not acknowledged.`),
    );
    const retry = el("button", "Retry same request");
    retry.type = "button";
    retry.disabled = pending.sending;
    retry.onclick = () => void submitReaction(key);
    const discard = el("button", "Discard");
    discard.type = "button";
    discard.disabled = pending.sending;
    discard.onclick = () => {
      pendingReactions.delete(key);
      renderThread();
    };
    row.append(retry, discard);
    container.append(row);
  }
}
const previewCache = new Map();
const previewObserver =
  typeof IntersectionObserver === "function"
    ? new IntersectionObserver((entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          previewObserver.unobserve(entry.target);
          loadPreview(entry.target);
        }
      })
    : null;
async function loadPreview(img) {
  const id = img.dataset.attachmentId;
  let pending = previewCache.get(id);
  if (!pending) {
    pending = fetch(`/v1/attachments/${encodeURIComponent(id)}`, {
      signal: abort.signal,
      headers: { Authorization: `Bearer ${token}` },
    }).then((response) => {
      if (!response.ok) throw new Error("preview unavailable");
      return response.blob().then((blob) => URL.createObjectURL(blob));
    });
    previewCache.set(id, pending);
  }
  try {
    img.src = await pending;
  } catch {
    previewCache.delete(id);
    img.remove();
  }
}
function renderPreview(container, attachment) {
  if (!attachment.available || !/^image\//.test(attachment.mime || "")) return;
  const img = el("img", undefined, "preview");
  img.alt = attachment.name || "Image attachment";
  img.loading = "lazy";
  img.dataset.attachmentId = attachment.id;
  container.append(img);
  if (previewObserver) previewObserver.observe(img);
  else loadPreview(img);
}
function renderMediaRequest(container, attachment) {
  if (!attachment.requestable) return;
  const button = el("button", "Request full media from phone", "attachment");
  button.type = "button";
  button.onclick = async () => {
    button.disabled = true;
    try {
      await request(
        `/v1/attachments/${encodeURIComponent(attachment.id)}/request`,
        { method: "POST" },
      );
      notice("Requested. The message updates when the phone uploads it.");
    } catch (error) {
      notice(error.message);
      button.disabled = false;
    }
  };
  container.append(button);
}
function renderAttachment(container, attachment) {
  renderPreview(container, attachment);
  const quality = attachment.preview ? " · Preview" : "";
  const label = `${attachment.name || "Attachment"} · ${formatSize(attachment.size || 0)}${quality}${attachment.available ? " · Download" : " · Unavailable"}`;
  const button = el("button", label, "attachment");
  button.type = "button";
  button.disabled = !attachment.available;
  button.onclick = async () => {
    button.disabled = true;
    try {
      const response = await fetch(
        `/v1/attachments/${encodeURIComponent(attachment.id)}`,
        {
          signal: abort.signal,
          headers: { Authorization: `Bearer ${token}` },
        },
      );
      if (!response.ok)
        throw new Error(
          "Attachment unavailable. It may require the phone or exceed 20 MiB.",
        );
      const url = URL.createObjectURL(await response.blob());
      const link = el("a");
      link.href = url;
      link.download = attachment.name || "attachment";
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 60000);
    } catch (error) {
      notice(error.message);
    } finally {
      button.disabled = false;
    }
  };
  container.append(button);
  renderMediaRequest(container, attachment);
}
function outboxDescription(item) {
  const body = item.request || {};
  if (body.kind === "reaction")
    return `${body.remove ? "Remove" : "React"} ${body.emoji}`;
  if (body.kind === "conversation")
    return `New conversation with ${(body.recipients || []).join(", ")}`;
  const attachments = body.attachment_ids?.length
    ? ` · ${body.attachment_ids.length} attachment${body.attachment_ids.length === 1 ? "" : "s"}`
    : "";
  return `${body.text || "Message with attachments"}${attachments}`;
}
function renderOutbox() {
  $("outbox").replaceChildren();
  for (const item of outbox
    .filter((candidate) => {
      const body = candidate.request || {};
      return (
        (body.conversation_id || candidate.conversation_id) === selected &&
        (candidate.state !== "confirmed" ||
          !messages.some((message) => message.id === candidate.message_id))
      );
    })
    .sort((a, b) => (a.created || "").localeCompare(b.created || ""))) {
    const node = el("div", undefined, `outbox-item ${item.state}`);
    node.append(
      el(
        "strong",
        item.state === "confirmed" ? "Observed on phone" : item.state,
      ),
      el("p", outboxDescription(item)),
      el(
        "div",
        item.detail ||
          (item.state === "queued"
            ? "Waiting for the phone connection. This can be canceled before sending starts."
            : ""),
        "hint",
      ),
    );
    if (item.state === "queued") {
      const cancel = el("button", "Cancel queued request");
      cancel.type = "button";
      cancel.onclick = async () => {
        try {
          await request(`/v1/outbox/${encodeURIComponent(item.id)}/cancel`, {
            method: "POST",
          });
          await refresh();
        } catch (error) {
          notice(error.message);
        }
      };
      node.append(cancel);
    }
    $("outbox").append(node);
  }
}
function renderSelectedAttachments() {
  const files = pendingSend?.files || selectedFiles;
  $("selected-attachments").replaceChildren();
  for (const file of files)
    $("selected-attachments").append(
      el("span", `${file.name} (${formatSize(file.size)})`, "file-chip"),
    );
  if (files.length) {
    const total = files.reduce((sum, file) => sum + file.size, 0);
    $("selected-attachments").append(
      el(
        "span",
        `${files.length}/10 · ${formatSize(total)}/20 MiB`,
        "file-total",
      ),
    );
  }
  if (pendingSend?.progress)
    $("selected-attachments").append(el("span", pendingSend.progress));
}
function renderThread() {
  const conversation = conversations.find((item) => item.id === selected);
  $("empty").hidden = !!conversation;
  $("thread").hidden = !conversation;
  if (!conversation) return;
  $("thread-title").textContent = name(conversation);
  $("thread-info").textContent =
    `${(conversation.protocol || "message").toUpperCase()} · ${
      others(conversation)
        .map((participant) => participant.address || participant.name)
        .join(", ") || "Phone conversation"
    }`;
  const container = $("messages");
  const bottom =
    container.scrollHeight - container.scrollTop - container.clientHeight < 70;
  container.replaceChildren();
  for (const message of [...messages].sort(
    (a, b) =>
      (a.time || "").localeCompare(b.time || "") || a.id.localeCompare(b.id),
  )) {
    const node = el("article", undefined, `message ${message.direction}`);
    const sender = conversation.participants?.find(
      (participant) => participant.id === message.sender_id,
    );
    if (sender && !sender.is_me)
      node.append(el("div", sender.name || sender.address, "sender"));
    if (message.subject) node.append(el("strong", message.subject));
    node.append(
      el(
        "p",
        message.deleted
          ? "Message deleted on phone"
          : message.text ||
              (message.attachments?.length
                ? ""
                : "Message content unavailable"),
      ),
    );
    for (const attachment of message.deleted ? [] : message.attachments || [])
      renderAttachment(node, attachment);
    for (const reaction of message.reactions || [])
      node.append(
        el(
          "span",
          `${reaction.emoji} ${reaction.participants?.length || 0}`,
          "reaction",
        ),
      );
    renderReactionActions(node, message, conversation);
    node.append(
      el(
        "div",
        `${new Date(message.time).toLocaleString()} · ${(message.status || "stored").replaceAll("_", " ")}`,
        "meta",
      ),
    );
    container.append(node);
  }
  if (!messages.length)
    container.append(el("p", "No messages in stored history yet.", "hint"));
  if (bottom) container.scrollTop = container.scrollHeight;
  $("older").hidden = !before;
  const hasPending = !!pendingSend;
  $("text").disabled = conversation.read_only || hasPending;
  $("attachments").disabled = conversation.read_only || hasPending;
  $("send").disabled = conversation.read_only || sending;
  $("send").textContent = hasPending ? "Retry same request" : "Send message";
  $("discard-send").hidden = !hasPending;
  $("discard-send").disabled = sending;
  $("mark-read").disabled = !messages.length;
  $("import-conversation").disabled = importing > 0;
  renderSelectedAttachments();
  renderOutbox();
}
function renderConnection(status) {
  providerState = status.state || "offline";
  $("status").textContent = providerState.replaceAll("_", " ");
  $("provider-detail").textContent = status.detail || "";
  $("connection-actions").hidden = ![
    "connection_failed",
    "authentication_required",
  ].includes(providerState);
  $("sync-status").textContent = status.last_sync
    ? `Recent history checked ${new Date(status.last_sync).toLocaleTimeString()}. ${status.sync_state === "failed" ? "Latest check failed." : ""}`
    : "Recent history has not been reconciled yet.";
  $("send-hint").textContent =
    providerState === "connected"
      ? "Your phone handles delivery."
      : providerState === "authentication_required"
        ? "Pair through the CLI before queued requests can run."
        : providerState === "connection_failed"
          ? "Retry the bridge connection before queued requests can run."
          : "Offline: requests queue until your phone connects.";
}
function pairingActive() {
  return ["waiting_for_login", "connecting", "confirm_on_phone"].includes(
    pairingState.state,
  );
}
function renderPairing() {
  const state = pairingState.state || "not started";
  $("bridge-url").value = pairingState.ticket ? window.location.origin : "";
  $("pairing-ticket").value = pairingState.ticket || "";
  $("pairing-fields").hidden = !pairingState.ticket;
  $("pairing-emoji").hidden =
    state !== "confirm_on_phone" || !pairingState.emoji;
  $("pairing-emoji").textContent = pairingState.emoji
    ? `Confirm ${pairingState.emoji} on your phone`
    : "";
  $("pairing-status").textContent = `${state.replaceAll("_", " ")}${
    pairingState.detail ? ` · ${pairingState.detail}` : ""
  }${pairingActive() && pairingState.expires ? ` · expires ${new Date(pairingState.expires).toLocaleTimeString()}` : ""}`;
  $("start-pairing").disabled = pairingActive();
  $("start-pairing").textContent =
    providerState === "connected" || state === "paired"
      ? "Start re-pairing"
      : "Start pairing";
  $("cancel-pairing").hidden = !pairingActive();
}
function schedulePairingPoll() {
  clearTimeout(pairingPoll);
  pairingPoll = undefined;
  if (!pairingPanelOpen || !pairingActive()) return;
  pairingPoll = setTimeout(async () => {
    const pollGeneration = generation;
    try {
      const state = await request("/v1/pairing");
      if (pollGeneration !== generation) return;
      pairingState = state;
      renderPairing();
      if (pairingState.state === "paired")
        void refresh().catch((error) => notice(error.message));
    } catch (error) {
      if (pollGeneration !== generation) return;
      $("pairing-status").textContent = error.message;
    }
    schedulePairingPoll();
  }, 1000);
}
async function loadPairingState() {
  const requestGeneration = generation;
  try {
    const state = await request("/v1/pairing");
    if (requestGeneration !== generation) return;
    pairingState = state;
    renderPairing();
    schedulePairingPoll();
  } catch (error) {
    if (requestGeneration !== generation) return;
    $("pairing-status").textContent = error.message;
  }
}
function jobLabel(job) {
  if (job.conversation_id) {
    const conversation = conversations.find(
      (item) => item.id === job.conversation_id,
    );
    return conversation ? name(conversation) : "One conversation";
  }
  return `${job.folder || job.kind || "history"} folder`;
}
function historyBody(job, restart = false) {
  const body = job.conversation_id
    ? { conversation_id: job.conversation_id }
    : { folder: job.folder };
  if (restart) body.restart = true;
  return body;
}
function renderHistory() {
  $("history-jobs").replaceChildren();
  $("import-all").disabled = importing > 0;
  if (!historyJobs.length) {
    $("history-jobs").append(
      el("p", "No all-history imports requested yet.", "hint"),
    );
    return;
  }
  for (const job of [...historyJobs].sort((a, b) =>
    (b.updated || "").localeCompare(a.updated || ""),
  )) {
    const node = el("div", undefined, `history-job ${job.state}`);
    node.append(
      el("strong", jobLabel(job)),
      el(
        "span",
        `${(job.state || "queued").replaceAll("_", " ")} · ${job.pages || 0} pages · ${job.records || 0} records`,
      ),
    );
    if (job.detail) node.append(el("p", job.detail, "hint"));
    const actions = el("div", undefined, "button-row");
    if (job.state === "queued") {
      const pause = el("button", "Pause");
      pause.type = "button";
      pause.onclick = () => void pauseHistory(job.id);
      actions.append(pause);
    }
    if (["paused", "failed"].includes(job.state)) {
      const resume = el("button", "Resume");
      resume.type = "button";
      resume.onclick = () => void queueHistory(job, false);
      actions.append(resume);
    }
    if (["complete", "failed"].includes(job.state)) {
      const restart = el("button", "Fresh scan");
      restart.type = "button";
      restart.onclick = () => void queueHistory(job, true);
      actions.append(restart);
    }
    if (actions.childElementCount) node.append(actions);
    $("history-jobs").append(node);
  }
}
function maybeSelectCreatedConversation() {
  if (!createdConversation || createdConversation.selecting) return;
  const tracked = outbox.find(
    (item) => item.id === createdConversation.outboxID,
  );
  if (
    tracked &&
    ["canceled", "rejected", "ambiguous"].includes(tracked.state)
  ) {
    const recipients = tracked.request?.recipients || [];
    $("recipients").value = recipients.join(", ");
    $("new-conversation-form").hidden = false;
    $("conversation-status").textContent = `${tracked.state}: ${
      tracked.detail || "Conversation was not created."
    } Review the recipients before submitting a new request.`;
    createdConversation = undefined;
    return;
  }
  const conversationID =
    createdConversation.conversationID || tracked?.conversation_id;
  if (!conversationID) {
    $("conversation-status").textContent =
      tracked?.detail || "Waiting for the phone to create the conversation…";
    return;
  }
  createdConversation.conversationID = conversationID;
  if (!conversations.some((item) => item.id === conversationID)) {
    $("conversation-status").textContent =
      "Conversation accepted; waiting for its stored record…";
    return;
  }
  createdConversation.selecting = true;
  queueMicrotask(() => {
    createdConversation = undefined;
    $("conversation-status").textContent = "";
    $("new-conversation-form").hidden = true;
    void select(conversationID);
  });
}
let pendingConversationFromLink = "";
// Re-post the browser's subscription once per unlock so a bridge that lost its
// database, or a subscription the browser rotated, heals without user action.
let pushSynced = false;
async function select(id) {
  if (pendingSend && id !== selected) {
    notice(
      "Retry or discard the pending message request before switching conversations.",
    );
    return;
  }
  if (selected) {
    drafts.set(selected, $("text").value);
    draftFiles.set(selected, selectedFiles);
  }
  selected = id;
  messageUpdates.clear();
  messages = [];
  before = "";
  selectedFiles = draftFiles.get(id) || [];
  $("text").value = drafts.get(id) || "";
  $("attachments").value = "";
  renderConversations();
  renderThread();
  try {
    if (refreshTask) await refreshTask;
    await refresh();
    $("messages").scrollTop = $("messages").scrollHeight;
  } catch (error) {
    notice(error.message);
  }
}
async function refresh() {
  if (refreshTask) return refreshTask;
  const currentGeneration = generation;
  refreshTask = (async () => {
    const target = targetCursor;
    const id = selected;
    const historyResult = request("/v1/history").catch((error) => ({
      jobs: historyJobs,
      error,
    }));
    const [status, cs, os, ms, hs] = await Promise.all([
      request("/v1/status"),
      request("/v1/conversations"),
      request("/v1/outbox"),
      id
        ? request(
            `/v1/conversations/${encodeURIComponent(id)}/messages?limit=100`,
          )
        : null,
      historyResult,
    ]);
    if (currentGeneration !== generation) return;
    conversations = cs.conversations || [];
    outbox = os.outbox || [];
    historyJobs = hs.jobs || [];
    renderConnection(status);
    if (!pushSynced && $("notify").checked) {
      pushSynced = true;
      void enablePush().catch(() => {});
    }
    if (
      pendingConversationFromLink &&
      conversations.some((c) => c.id === pendingConversationFromLink)
    ) {
      const wanted = pendingConversationFromLink;
      pendingConversationFromLink = "";
      void select(wanted);
    }
    if (id === selected && ms) {
      const expanded = messages.length > 100;
      messages = mergeMessages(
        messages,
        ms.messages || [],
        ms.cursor,
        messageUpdates,
      );
      if (!expanded) before = ms.next_before || "";
    }
    if (!cursor) cursor = Math.min(cs.cursor || 0, os.cursor || 0);
    cursor = Math.max(cursor, target);
    renderConversations();
    renderHistory();
    if (hs.error)
      $("history-jobs").prepend(
        el("p", `History status unavailable: ${hs.error.message}`, "hint"),
      );
    renderThread();
    maybeSelectCreatedConversation();
  })().finally(() => {
    refreshTask = undefined;
  });
  return refreshTask;
}
async function stream(currentGeneration) {
  while (generation === currentGeneration && token) {
    try {
      const response = await fetch(`/v1/stream?after=${cursor}`, {
        signal: abort.signal,
        headers: { Authorization: `Bearer ${token}` },
      });
      if (!response.ok) throw new Error("Live updates unavailable");
      const reader = response.body.getReader(),
        decoder = new TextDecoder();
      let live = false;
      const parse = createParser((event) => {
        if (event.type === "live") {
          live = true;
          return;
        }
        if (event.type === "typing" && event.data.active) {
          typingUntil = Date.now() + 5000;
          typingConversation = event.entity_id;
        } else if (event.type === "typing") typingUntil = 0;
        else {
          targetCursor = Math.max(targetCursor, event.id || 0);
          if (
            event.type === "message" &&
            event.data.conversation_id === selected
          )
            messageUpdates.set(event.entity_id, event);
          if (live && event.type === "message") notifyIncoming(event.data);
        }
      });
      try {
        for (;;) {
          const chunk = await reader.read();
          if (chunk.done) break;
          parse(decoder.decode(chunk.value, { stream: true }));
        }
      } finally {
        await reader.cancel().catch(() => {});
      }
    } catch (error) {
      if (generation === currentGeneration && !abort.signal.aborted)
        notice(
          "Live connection interrupted. Reconnecting; stored history remains available.",
        );
    }
    if (generation !== currentGeneration || !token) return;
    await new Promise((resolve) => setTimeout(resolve, 2000));
  }
}
const notified = new Set();
function notifyIncoming(message) {
  if (
    !$("notify").checked ||
    typeof Notification !== "function" ||
    Notification.permission !== "granted" ||
    message.direction !== "incoming" ||
    message.deleted ||
    notified.has(message.id) ||
    (!document.hidden && message.conversation_id === selected)
  )
    return;
  notified.add(message.id);
  const conversation = conversations.find(
    (c) => c.id === message.conversation_id,
  );
  const sender = conversation?.participants?.find(
    (p) => p.id === message.sender_id,
  );
  const title = conversation?.name || sender?.name || "New message";
  const body =
    message.text ||
    (message.attachments?.length ? "Attachment" : "New message");
  const notification = new Notification(title, {
    body:
      sender && conversation?.participants?.length > 2
        ? `${sender.name || sender.address}: ${body}`
        : body,
    tag: message.id,
  });
  notification.onclick = () => {
    window.focus();
    void select(message.conversation_id);
    notification.close();
  };
}

// Base64url as the Push API wants it for the server's VAPID public key.
function decodeKey(value) {
  const padded = value.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(padded.padEnd(Math.ceil(padded.length / 4) * 4, "="));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}
async function pushRegistration() {
  if (!("serviceWorker" in navigator) || !("PushManager" in window))
    throw new Error(
      "This browser cannot deliver notifications in the background.",
    );
  return navigator.serviceWorker.ready;
}
async function enablePush() {
  if (typeof Notification !== "function")
    throw new Error("This browser has no notification support.");
  if (Notification.permission === "default")
    await Notification.requestPermission();
  if (Notification.permission !== "granted")
    throw new Error("Browser notifications were not allowed.");
  const state = await request("/v1/push");
  if (!state.enabled || !state.public_key)
    throw new Error("This bridge has notifications turned off.");
  const registration = await pushRegistration();
  const subscription =
    (await registration.pushManager.getSubscription()) ||
    (await registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: decodeKey(state.public_key),
    }));
  await request("/v1/push/subscriptions", {
    method: "POST",
    body: JSON.stringify(subscription),
  });
}
async function disablePush() {
  if (!("serviceWorker" in navigator)) return;
  const registration = await navigator.serviceWorker.getRegistration();
  const subscription = await registration?.pushManager.getSubscription();
  if (!subscription) return;
  await request("/v1/push/subscriptions", {
    method: "DELETE",
    body: JSON.stringify({ endpoint: subscription.endpoint }),
  }).catch(() => {});
  await subscription.unsubscribe().catch(() => {});
}
$("notify").onchange = async () => {
  const wanted = $("notify").checked;
  $("notify").disabled = true;
  try {
    if (wanted) {
      await enablePush();
      notice("Notifications on, including while this app is closed.");
    } else {
      await disablePush();
      notice("Notifications off on this device.");
    }
    localStorage.setItem("notify", wanted ? "1" : "");
  } catch (error) {
    $("notify").checked = false;
    localStorage.setItem("notify", "");
    notice(error.message);
  } finally {
    $("notify").disabled = false;
    renderNotifyControls();
  }
};
$("test-notification").onclick = async () => {
  $("test-notification").disabled = true;
  try {
    await request("/v1/push/test", { method: "POST" });
    notice("Test notification sent to every subscribed device.");
  } catch (error) {
    notice(error.message);
  } finally {
    $("test-notification").disabled = false;
  }
};
function renderNotifyControls() {
  $("test-notification").hidden = !$("notify").checked;
}
$("notify").checked =
  !!localStorage.getItem("notify") &&
  typeof Notification === "function" &&
  Notification.permission === "granted";
renderNotifyControls();
function clearPrivateUI() {
  pushSynced = false;
  for (const pending of previewCache.values())
    pending.then((url) => URL.revokeObjectURL(url)).catch(() => {});
  previewCache.clear();
  notified.clear();
  pendingSend = pendingConversation = createdConversation = undefined;
  selectedFiles = [];
  conversations = outbox = messages = historyJobs = [];
  selected = before = "";
  cursor = targetCursor = 0;
  typingUntil = 0;
  typingConversation = "";
  providerState = "offline";
  sending = false;
  creatingConversation = false;
  importing = 0;
  clearTimeout(typingTimer);
  clearTimeout(pairingPoll);
  typingTimer = undefined;
  pairingPoll = undefined;
  pairingPanelOpen = false;
  pairingState = {};
  lastTypingAt = 0;
  drafts.clear();
  draftFiles.clear();
  messageUpdates.clear();
  pendingReactions.clear();
  for (const id of [
    "notice",
    "provider-detail",
    "sync-status",
    "conversation-status",
    "thread-title",
    "thread-info",
    "typing",
    "pairing-status",
    "pairing-emoji",
  ])
    $(id).textContent = "";
  for (const id of [
    "search",
    "recipients",
    "text",
    "attachments",
    "bridge-url",
    "pairing-ticket",
  ])
    $(id).value = "";
  $("new-conversation-form").hidden = true;
  $("pairing-panel").hidden = true;
  $("pairing-fields").hidden = true;
  $("connection-actions").hidden = true;
  $("messages").replaceChildren();
  $("outbox").replaceChildren();
  $("history-jobs").replaceChildren();
  $("selected-attachments").replaceChildren();
  renderConversations();
  renderThread();
}
const normalizeOutbox = (response) => response?.outbox || response;
async function createConversation() {
  if (!pendingConversation) {
    try {
      pendingConversation = newConversationRequest(
        parseRecipients($("recipients").value),
      );
    } catch (error) {
      $("conversation-status").textContent = error.message;
      return;
    }
  }
  const pending = pendingConversation;
  creatingConversation = true;
  $("recipients").disabled = true;
  $("create-conversation").disabled = true;
  $("conversation-status").textContent = "Requesting conversation…";
  try {
    const result = normalizeOutbox(
      await request("/v1/conversations", {
        method: "POST",
        headers: { "Idempotency-Key": pending.key },
        body: JSON.stringify(pending.body),
      }),
    );
    if (pending !== pendingConversation) return;
    pendingConversation = undefined;
    createdConversation = {
      outboxID: result.id,
      conversationID: result.conversation_id || "",
    };
    $("recipients").value = "";
    $("conversation-status").textContent =
      "Conversation request accepted. Waiting for its stored record…";
    await refresh();
  } catch (error) {
    if (pending !== pendingConversation) return;
    $("conversation-status").textContent =
      `${error.message} Retry preserves the same idempotency key and recipients.`;
  } finally {
    creatingConversation = false;
    if (pending === pendingConversation) {
      $("recipients").disabled = true;
      $("create-conversation").textContent = "Retry same request";
    } else {
      $("recipients").disabled = false;
      $("create-conversation").textContent = "Create";
    }
    $("create-conversation").disabled = false;
  }
}
async function startReaction(messageID, emoji) {
  const key = reactionKey(selected, messageID, emoji);
  if (!pendingReactions.has(key))
    pendingReactions.set(key, {
      conversationID: selected,
      request: newReactionRequest(messageID, emoji),
      sending: false,
    });
  await submitReaction(key);
}
async function submitReaction(key) {
  const pending = pendingReactions.get(key);
  if (!pending || pending.sending) return;
  pending.sending = true;
  renderThread();
  try {
    await request(
      `/v1/conversations/${encodeURIComponent(pending.conversationID)}/reactions`,
      {
        method: "POST",
        headers: { "Idempotency-Key": pending.request.key },
        body: JSON.stringify(pending.request.body),
      },
    );
    if (pendingReactions.get(key) !== pending) return;
    pendingReactions.delete(key);
    notice("Reaction queued.");
    await refresh();
  } catch (error) {
    if (pendingReactions.get(key) !== pending) return;
    notice(
      `${error.message} The reaction was not retried automatically; Retry same request reuses its key.`,
    );
  } finally {
    pending.sending = false;
    renderThread();
  }
}
async function uploadFiles(pending) {
  while (pending.uploadIDs.length < pending.files.length) {
    const file = pending.files[pending.uploadIDs.length];
    pending.progress = `Uploading ${pending.uploadIDs.length + 1} of ${pending.files.length}: ${file.name}`;
    renderSelectedAttachments();
    const upload = await request(
      `/v1/uploads?name=${encodeURIComponent(file.name)}`,
      {
        method: "POST",
        headers: { "Content-Type": file.type || "application/octet-stream" },
        body: file,
      },
    );
    if (!upload?.id) throw new Error("Upload response did not include an ID.");
    pending.uploadIDs.push(upload.id);
  }
}
async function sendMessage() {
  if (sending) return;
  if (!pendingSend) {
    const text = $("text").value;
    try {
      validateAttachments(selectedFiles);
    } catch (error) {
      notice(error.message);
      return;
    }
    if (!text.trim() && !selectedFiles.length) {
      notice("Write a message or choose at least one attachment.");
      return;
    }
    pendingSend = {
      conversationID: selected,
      files: [...selectedFiles],
      uploadIDs: [],
      text,
      requests: undefined,
      progress: "",
    };
  }
  const pending = pendingSend,
    sendGeneration = generation;
  sending = true;
  renderThread();
  try {
    await uploadFiles(pending);
    pending.requests ||= splitRequests(
      pending.conversationID,
      pending.text,
      pending.uploadIDs,
    );
    for (const item of pending.requests) {
      if (item.queued) continue;
      pending.progress =
        pending.requests.length > 1
          ? `Queueing ${item.body.attachment_ids.length ? "attachments" : "caption"}…`
          : "Queueing message…";
      renderSelectedAttachments();
      await request("/v1/messages", {
        method: "POST",
        headers: { "Idempotency-Key": item.key },
        body: JSON.stringify(item.body),
      });
      item.queued = true;
    }
    if (sendGeneration !== generation || pendingSend !== pending) return;
    pendingSend = undefined;
    selectedFiles = [];
    $("attachments").value = "";
    $("text").value = "";
    drafts.delete(selected);
    draftFiles.delete(selected);
    notice("");
    await refresh();
  } catch (error) {
    if (sendGeneration !== generation || pendingSend !== pending) return;
    pending.progress = "";
    notice(
      `${error.message} Retry same request preserves message keys, bodies, completed uploads, and already queued parts.`,
    );
  } finally {
    if (sendGeneration === generation) {
      sending = false;
      renderThread();
    }
  }
}
async function queueHistory(job, restart) {
  const actionGeneration = generation;
  importing++;
  renderHistory();
  renderThread();
  try {
    await request("/v1/history", {
      method: "POST",
      body: JSON.stringify(historyBody(job, restart)),
    });
    if (actionGeneration !== generation) return;
    notice(restart ? "Fresh history scan queued." : "History import queued.");
    await refresh();
  } catch (error) {
    if (actionGeneration !== generation) return;
    notice(error.message);
  } finally {
    if (actionGeneration === generation) {
      importing--;
      renderHistory();
      renderThread();
    }
  }
}
async function pauseHistory(id) {
  const actionGeneration = generation;
  try {
    await request(`/v1/history/${encodeURIComponent(id)}/pause`, {
      method: "POST",
    });
    if (actionGeneration !== generation) return;
    notice(
      "History import paused. An in-flight page may finish; resuming safely re-reads it.",
    );
    await refresh();
  } catch (error) {
    if (actionGeneration !== generation) return;
    notice(error.message);
  }
}
function typingContentPresent() {
  return !!$("text").value.trim() || selectedFiles.length > 0;
}
function scheduleTyping() {
  if (
    !token ||
    !selected ||
    providerState !== "connected" ||
    !$("send-typing").checked ||
    !typingContentPresent() ||
    pendingSend
  )
    return;
  clearTimeout(typingTimer);
  const delay = Math.max(0, 4000 - (Date.now() - lastTypingAt));
  const conversationID = selected;
  typingTimer = setTimeout(async () => {
    const typingGeneration = generation;
    if (
      conversationID !== selected ||
      providerState !== "connected" ||
      !$("send-typing").checked ||
      !typingContentPresent()
    )
      return;
    lastTypingAt = Date.now();
    try {
      await request(
        `/v1/conversations/${encodeURIComponent(conversationID)}/typing`,
        { method: "POST" },
      );
    } catch (error) {
      if (typingGeneration === generation && error.status !== 503)
        notice(error.message);
    }
  }, delay);
}

$("login-form").onsubmit = async (event) => {
  event.preventDefault();
  token = $("token").value.trim();
  abort?.abort();
  abort = new AbortController();
  generation++;
  try {
    if (refreshTask) await refreshTask.catch(() => {});
    await refresh();
    $("token").value = "";
    $("login").hidden = true;
    $("app").hidden = false;
    $("logout").hidden = false;
    notice("");
    void stream(generation);
  } catch (error) {
    token = "";
    alert(error.message);
  }
};
$("logout").onclick = () => {
  abort?.abort();
  generation++;
  token = "";
  clearPrivateUI();
  $("app").hidden = true;
  $("login").hidden = false;
  $("logout").hidden = true;
  $("status").textContent = "Locked";
};
$("search").oninput = renderConversations;
$("open-pairing").onclick = () => {
  pairingPanelOpen = true;
  $("pairing-panel").hidden = false;
  void loadPairingState();
};
$("close-pairing").onclick = () => {
  pairingPanelOpen = false;
  clearTimeout(pairingPoll);
  pairingPoll = undefined;
  $("pairing-panel").hidden = true;
};
$("start-pairing").onclick = async () => {
  if (
    sending ||
    creatingConversation ||
    [...pendingReactions.values()].some((pending) => pending.sending)
  ) {
    $("pairing-status").textContent =
      "Wait for the active browser request to finish, then start pairing.";
    return;
  }
  const actionGeneration = generation;
  $("start-pairing").disabled = true;
  try {
    const state = await request("/v1/pairing/start", {
      method: "POST",
      body: "{}",
    });
    if (actionGeneration !== generation) return;
    pairingState = state;
    const clearedLocalRetry =
      !!pendingSend || !!pendingConversation || pendingReactions.size > 0;
    pendingSend = undefined;
    pendingConversation = undefined;
    pendingReactions.clear();
    $("recipients").disabled = false;
    $("create-conversation").textContent = "Create";
    renderThread();
    renderPairing();
    if (clearedLocalRetry)
      notice(
        "Pairing changed; review the recipient and submit each operation again.",
      );
    schedulePairingPoll();
  } catch (error) {
    if (actionGeneration !== generation) return;
    $("pairing-status").textContent = error.message;
    $("start-pairing").disabled = false;
  }
};
$("cancel-pairing").onclick = async () => {
  const actionGeneration = generation;
  $("cancel-pairing").disabled = true;
  try {
    const state = await request("/v1/pairing/cancel", { method: "POST" });
    if (actionGeneration !== generation) return;
    pairingState = state;
    renderPairing();
    schedulePairingPoll();
  } catch (error) {
    if (actionGeneration !== generation) return;
    $("pairing-status").textContent = error.message;
  } finally {
    $("cancel-pairing").disabled = false;
  }
};
for (const button of document.querySelectorAll("[data-copy]"))
  button.onclick = async () => {
    const input = $(button.dataset.copy);
    try {
      await navigator.clipboard.writeText(input.value);
      $("pairing-status").textContent = "Copied.";
    } catch {
      input.select();
      document.execCommand("copy");
      input.setSelectionRange(0, 0);
      $("pairing-status").textContent = "Copied.";
    }
  };
$("new-conversation").onclick = () => {
  $("new-conversation-form").hidden = false;
  $("recipients").focus();
};
$("cancel-conversation").onclick = () => {
  pendingConversation = undefined;
  $("recipients").disabled = false;
  $("recipients").value = "";
  $("conversation-status").textContent = "";
  $("create-conversation").textContent = "Create";
  $("new-conversation-form").hidden = true;
};
$("new-conversation-form").onsubmit = (event) => {
  event.preventDefault();
  void createConversation();
};
$("sync").onclick = async () => {
  try {
    await request("/v1/sync", { method: "POST" });
    notice("Recent-history check requested.");
  } catch (error) {
    notice(error.message);
  }
};
$("retry-connection").onclick = async () => {
  $("retry-connection").disabled = true;
  try {
    await request("/v1/connection/restart", { method: "POST" });
    notice(
      "Connection restart requested. Complete pairing through the CLI if prompted.",
    );
    await refresh();
  } catch (error) {
    notice(error.message);
  } finally {
    $("retry-connection").disabled = false;
  }
};
$("import-all").onclick = async () => {
  const actionGeneration = generation;
  importing++;
  renderHistory();
  renderThread();
  const results = await Promise.allSettled(
    ["inbox", "archive", "spam"].map((folder) =>
      request("/v1/history", {
        method: "POST",
        body: JSON.stringify({ folder }),
      }),
    ),
  );
  if (actionGeneration !== generation) return;
  importing--;
  const failed = results.filter((result) => result.status === "rejected");
  notice(
    failed.length
      ? `${3 - failed.length} history folders queued; ${failed.length} failed to queue.`
      : "Inbox, archive, and spam imports queued.",
  );
  await refresh().catch((error) => notice(error.message));
  renderHistory();
  renderThread();
};
$("import-conversation").onclick = () =>
  void queueHistory({ conversation_id: selected }, false);
$("compose").onsubmit = (event) => {
  event.preventDefault();
  void sendMessage();
};
$("discard-send").onclick = () => {
  if (sending) return;
  const uploaded = pendingSend?.uploadIDs.length || 0;
  pendingSend = undefined;
  notice(
    uploaded
      ? "Pending message discarded. Already uploaded bytes are no longer attached to a draft."
      : "Pending message discarded.",
  );
  renderThread();
};
$("text").oninput = () => {
  if (selected) drafts.set(selected, $("text").value);
  scheduleTyping();
};
$("attachments").onchange = () => {
  const files = [...$("attachments").files];
  try {
    validateAttachments(files);
    selectedFiles = files;
    draftFiles.set(selected, selectedFiles);
    notice("");
    renderSelectedAttachments();
    scheduleTyping();
  } catch (error) {
    $("attachments").value = "";
    selectedFiles = [];
    draftFiles.delete(selected);
    notice(error.message);
    renderSelectedAttachments();
  }
};
$("older").onclick = async () => {
  const id = selected,
    pageBefore = before;
  $("older").disabled = true;
  try {
    const page = await request(
      `/v1/conversations/${encodeURIComponent(id)}/messages?before=${encodeURIComponent(pageBefore)}&limit=100`,
    );
    if (selected !== id || before !== pageBefore) return;
    messages = mergeMessages(
      messages,
      page.messages || [],
      page.cursor,
      messageUpdates,
    );
    before = page.next_before || "";
    renderThread();
  } catch (error) {
    notice(error.message);
  } finally {
    $("older").disabled = false;
  }
};
$("mark-read").onclick = async () => {
  const latest = [...messages].sort((a, b) =>
    (b.time || "").localeCompare(a.time || ""),
  )[0];
  if (!latest) return;
  try {
    await request(`/v1/conversations/${encodeURIComponent(selected)}/read`, {
      method: "POST",
      body: JSON.stringify({ message_id: latest.id }),
    });
    notice("Read receipt requested.");
  } catch (error) {
    notice(error.message);
  }
};
let lastRefresh = 0;
setInterval(() => {
  $("typing").textContent =
    typingUntil > Date.now() && typingConversation === selected
      ? "Typing…"
      : "";
  if (!token || $("app").hidden) return;
  if (targetCursor > cursor || Date.now() - lastRefresh > 10000) {
    lastRefresh = Date.now();
    void refresh().catch((error) => notice(error.message));
  }
}, 500);

// The service worker backs the installed app: an offline shell and, once a
// subscription exists, notifications delivered while no window is open.
if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js").catch(() => {
    notice("Background updates are unavailable in this browser.");
  });
  navigator.serviceWorker.addEventListener("message", (event) => {
    if (event.data?.type === "open-conversation" && event.data.conversation)
      void select(event.data.conversation);
  });
}
{
  const requested = new URLSearchParams(location.search).get("conversation");
  if (requested) {
    history.replaceState(null, "", location.pathname);
    pendingConversationFromLink = requested;
  }
}

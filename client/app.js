import {
  createParser,
  mergeMessages,
  walkOlder,
  newConversationRequest,
  addParticipantsRequest,
  newReactionRequest,
  pastedImageName,
  pastedImages,
  splitRequests,
  validateAttachments,
} from "/stream.mjs";
import {
  avatarColor,
  composeHint,
  contactDetail,
  contactKey,
  contactName,
  displayName,
  filterConversations,
  formatSize,
  importStatus,
  initials,
  isGroup,
  layoutThread,
  linkify,
  listTime,
  matchContacts,
  messageStatus,
  others,
  participantList,
  outboxAttachmentCount,
  outboxStatus,
  outboxText,
  previewLine,
  queuePosition,
  reactedByMe,
  reactionTitle,
  sortConversations,
  summarizeHistory,
  threadOutbox,
  typedRecipient,
} from "/view.mjs";

const $ = (id) => document.getElementById(id);
// Present inside the Tauri desktop window, which keeps the token and shows
// notifications on the client's behalf.
const desktop = window.__TAURI__?.core?.invoke;
// Empty when the bridge serves this client, so every API path stays
// same-origin. The desktop app bundles the client instead and supplies the
// bridge it was pointed at, which is the only thing it loads over the network.
let apiBase = "";
const api = (path) => apiBase + path;
const normalizeBase = (url) => url.trim().replace(/\/+$/, "");
const DEFAULT_REACTIONS = ["👍", "❤️", "😂", "😮", "😢", "😠"];
const RECENT_REACTIONS = "reaction-recents";
const QUICK_REACTIONS = 6;
const THREAD_CACHE = 16;

let token = "",
  abort,
  generation = 0;
let selected = "",
  composing = false;
const conversations = new Map();
const outbox = new Map();
const historyJobs = new Map();
const threads = new Map();
let conversationCursor = 0,
  outboxCursor = 0,
  historyCursor = 0,
  cursor = 0;
const drafts = new Map();
const draftFiles = new Map();
const pendingReactions = new Map();
let emojiCatalog, emojiTarget, emojiGroup;
let sending = false,
  creatingConversation = false,
  pendingSend,
  pendingConversation,
  createdConversation,
  selectedFiles = [];
let status = {},
  providerState = "offline",
  currentSessionEpoch = 0,
  typingUntil = 0,
  typingConversation = "",
  typingTimer,
  lastTypingAt = 0,
  importing = 0;
let pairingState = {},
  pairingPoll,
  pairingPanelOpen = false;
let pendingConversationFromLink = "";
let contactBook = { contacts: [], stale: false };
let contactsLoading;
// Set while a conversation request is in flight or waiting to be retried, when
// its idempotency key is already bound to the recipients that were submitted.
let composeLocked = false;
let peopleConversation = "";
// Re-post the browser's subscription once per unlock so a bridge that lost its
// database, or a subscription the browser rotated, heals without user action.
let pushSynced = false;

// --- DOM helpers ------------------------------------------------------------

function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}
function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", `#i-${name}`);
  svg.append(use);
  return svg;
}
function button(text, className, onClick) {
  const node = el("button", text, className);
  node.type = "button";
  node.onclick = onClick;
  return node;
}
function fillAvatar(node, conversation) {
  const name = displayName(conversation);
  node.style.background = avatarColor(name);
  const letter = isGroup(conversation) ? "" : initials(name);
  node.replaceChildren(
    letter || icon(isGroup(conversation) ? "group" : "person"),
  );
}
const stillMotion = matchMedia("(prefers-reduced-motion: reduce)");

// Entrance classes are removed once they play so a later redraw of the same
// node does not replay them.
function animateIn(node) {
  if (stillMotion.matches) return;
  node.classList.add("entering");
  node.addEventListener(
    "animationend",
    () => node.classList.remove("entering"),
    { once: true },
  );
}

// FLIP: measure before the reorder, then play the old position back so rows
// that moved slide into place instead of jumping.
function measureRows(nodes) {
  if (stillMotion.matches) return undefined;
  const before = new Map();
  for (const node of nodes) before.set(node, node.getBoundingClientRect().top);
  return before;
}
function slideRows(before) {
  if (!before) return;
  for (const [node, top] of before) {
    const delta = top - node.getBoundingClientRect().top;
    if (!delta || Math.abs(delta) < 2) continue;
    node.animate(
      [{ transform: `translateY(${delta}px)` }, { transform: "none" }],
      { duration: 260, easing: "cubic-bezier(0.05, 0.7, 0.1, 1)" },
    );
  }
}

let noticeTimer;
function notice(message) {
  clearTimeout(noticeTimer);
  $("notice").textContent = message;
  $("notice").hidden = !message;
  if (message) noticeTimer = setTimeout(() => notice(""), 6000);
}
function autogrow() {
  const text = $("text");
  text.style.height = "auto";
  text.style.height = `${Math.min(text.scrollHeight, 160)}px`;
}

// Rendering is batched per animation frame; state changes mark parts dirty.
const dirty = new Set();
let frame;
function invalidate(...parts) {
  for (const part of parts) dirty.add(part);
  if (!frame) frame = requestAnimationFrame(render);
}
function render() {
  frame = undefined;
  const parts = new Set(dirty);
  dirty.clear();
  if (parts.has("status")) renderStatus();
  if (parts.has("list")) renderList();
  if (parts.has("thread")) renderThread();
  if (parts.has("compose")) renderCompose();
  if (parts.has("newchat")) renderNewChat();
  if (parts.has("history")) renderHistory();
}

// --- API --------------------------------------------------------------------

async function request(path, options = {}) {
  const headers = { Authorization: `Bearer ${token}`, ...options.headers };
  if (
    typeof options.body === "string" &&
    !Object.keys(headers).some((key) => key.toLowerCase() === "content-type")
  )
    headers["Content-Type"] = "application/json";
  const response = await fetch(api(path), {
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

function thread(id) {
  let entry = threads.get(id);
  if (!entry) {
    entry = {
      messages: new Map(),
      updates: new Map(),
      cursor: 0,
      before: "",
      fetched: false,
      loading: undefined,
      touched: 0,
    };
    threads.set(id, entry);
    if (threads.size > THREAD_CACHE) {
      const oldest = [...threads.entries()]
        .filter(([key]) => key !== selected)
        .sort((a, b) => a[1].touched - b[1].touched)[0];
      if (oldest) threads.delete(oldest[0]);
    }
  }
  entry.touched = Date.now();
  return entry;
}

function loadThread(id, before = "", { limit = 100, quiet = false } = {}) {
  const entry = thread(id);
  if (entry.loading) return entry.loading;
  const requestGeneration = generation;
  entry.loading = (async () => {
    try {
      const page = await request(
        `/v1/conversations/${encodeURIComponent(id)}/messages?limit=${limit}${
          before ? `&before=${encodeURIComponent(before)}` : ""
        }`,
      );
      if (requestGeneration !== generation || threads.get(id) !== entry) return;
      const merged = mergeMessages(
        [...entry.messages.values()],
        page.messages || [],
        page.cursor || 0,
        entry.updates,
      );
      entry.messages = new Map(merged.map((message) => [message.id, message]));
      entry.cursor = Math.max(entry.cursor, page.cursor || 0);
      if (before || !entry.fetched) entry.before = page.next_before || "";
      entry.fetched = true;
      if (id === selected && !quiet) invalidate("thread", "compose");
    } finally {
      entry.loading = undefined;
    }
  })();
  return entry.loading;
}

let loadingAll;
function loadAll() {
  if (loadingAll) return loadingAll;
  const requestGeneration = generation;
  loadingAll = (async () => {
    stopFullHistory();
    const [st, cs, os, hs] = await Promise.all([
      request("/v1/status"),
      request("/v1/conversations"),
      request("/v1/outbox"),
      request("/v1/history").catch((error) => ({ error })),
    ]);
    if (requestGeneration !== generation) return;
    conversations.clear();
    for (const conversation of cs.conversations || [])
      conversations.set(conversation.id, conversation);
    conversationCursor = cs.cursor || 0;
    outbox.clear();
    for (const item of os.outbox || []) outbox.set(item.id, item);
    outboxCursor = os.cursor || 0;
    if (!hs.error) {
      historyJobs.clear();
      for (const job of hs.jobs || []) historyJobs.set(job.id, job);
      historyCursor = hs.cursor || 0;
    }
    const cursors = [conversationCursor, outboxCursor];
    if (!hs.error) cursors.push(historyCursor);
    cursor = Math.min(cursor || Infinity, ...cursors);
    for (const [id, entry] of threads)
      if (id !== selected || entry.loading) threads.delete(id);
    if (selected) {
      thread(selected).fetched = false;
      void loadThread(selected).catch((error) => notice(error.message));
    }
    applyStatus(st);
    if (hs.error) notice(`History status unavailable: ${hs.error.message}`);
    if (!desktop && !pushSynced && $("notify").checked) {
      pushSynced = true;
      void enablePush().catch(() => {});
    }
    if (
      pendingConversationFromLink &&
      conversations.has(pendingConversationFromLink)
    ) {
      const wanted = pendingConversationFromLink;
      pendingConversationFromLink = "";
      void select(wanted);
    }
    invalidate("status", "list", "thread", "compose", "history");
    maybeSelectCreatedConversation();
  })().finally(() => {
    loadingAll = undefined;
  });
  return loadingAll;
}

function applyStatus(next) {
  const previousEpoch = currentSessionEpoch;
  status = next || {};
  providerState = status.state || "offline";
  currentSessionEpoch = status.session_epoch || 0;
  invalidate("status", "compose");
  scheduleReadReceipt();
  if (previousEpoch && currentSessionEpoch !== previousEpoch)
    void loadAll().catch((error) => notice(error.message));
}

async function pollStatus() {
  if (!token || $("app").hidden || loadingAll) return;
  const requestGeneration = generation;
  try {
    const next = await request("/v1/status");
    if (requestGeneration === generation) applyStatus(next);
  } catch {
    // The stream reconnect path reports connectivity problems.
  }
}

function applyEvent(event) {
  const id = event.id || 0;
  const data = event.data || {};
  switch (event.type) {
    case "conversation":
      if (id > conversationCursor) {
        conversations.set(event.entity_id, data);
        invalidate("list");
        if (event.entity_id === selected) {
          invalidate("thread", "compose");
          scheduleReadReceipt();
        }
        maybeSelectCreatedConversation();
      }
      break;
    case "message": {
      const entry = threads.get(data.conversation_id);
      if (!entry) break;
      if (entry.loading || !entry.fetched)
        entry.updates.set(event.entity_id, event);
      else if (id > entry.cursor) {
        entry.messages.set(event.entity_id, data);
        if (data.conversation_id === selected) {
          invalidate("thread", "compose");
          scheduleReadReceipt();
        }
      }
      break;
    }
    case "outbox":
      if (id > outboxCursor) {
        outbox.set(event.entity_id, data);
        invalidate("thread");
        maybeSelectCreatedConversation();
      }
      break;
    case "history":
      if (id > historyCursor) {
        historyJobs.set(event.entity_id, data);
        invalidate("history", "thread");
      }
      break;
    case "typing":
      if (data.active) {
        typingUntil = Date.now() + 5000;
        typingConversation = data.conversation_id || event.entity_id;
      } else typingUntil = 0;
      return;
    default:
      break;
  }
  if (id) cursor = Math.max(cursor, id);
}

async function stream(currentGeneration) {
  while (generation === currentGeneration && token) {
    let failed = false;
    try {
      const response = await fetch(api(`/v1/stream?after=${cursor}`), {
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
        applyEvent(event);
        if (live && event.type === "message") notifyIncoming(event.data);
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
    } catch {
      failed = true;
      if (generation === currentGeneration && !abort.signal.aborted)
        notice("Live connection interrupted. Reconnecting…");
    }
    if (generation !== currentGeneration || !token) return;
    await new Promise((resolve) => setTimeout(resolve, 2000));
    if (generation !== currentGeneration || !token) return;
    if (failed) await loadAll().catch(() => {});
  }
}

// --- Conversation list ------------------------------------------------------

const listNodes = new Map();
let listRendered = false;
function buildConversationNode(conversation) {
  const node = el("button", undefined, "conversation");
  node.type = "button";
  node.append(
    el("div", undefined, "avatar"),
    el("span", undefined, "name"),
    el("span", undefined, "time"),
    el("span", undefined, "preview"),
    el("span", undefined, "badge"),
  );
  node.onclick = () => select(conversation.id);
  return node;
}
function fillConversationNode(node, conversation) {
  node.className = `conversation${conversation.id === selected ? " active" : ""}${
    conversation.unread ? " unread" : ""
  }${conversation.read_only ? " previous-session" : ""}`;
  const [avatar, name, time, preview, badge] = node.children;
  fillAvatar(avatar, conversation);
  name.textContent = displayName(conversation);
  time.textContent = listTime(conversation.updated);
  preview.textContent = previewLine(conversation);
  badge.hidden = !conversation.unread;
}
function renderList() {
  const items = filterConversations(
    sortConversations(conversations.values()),
    $("search").value,
  );
  const container = $("conversations");
  const seen = new Set();
  const order = [];
  for (const conversation of items) {
    seen.add(conversation.id);
    let entry = listNodes.get(conversation.id);
    if (!entry) {
      entry = {
        node: buildConversationNode(conversation),
        signature: "",
        fresh: true,
      };
      listNodes.set(conversation.id, entry);
    }
    const signature = [
      displayName(conversation),
      conversation.preview,
      conversation.preview_direction,
      conversation.preview_sender_id,
      conversation.updated,
      conversation.unread,
      conversation.read_only,
      conversation.id === selected,
      conversation.participants?.length,
    ].join("|");
    if (entry.signature !== signature) {
      fillConversationNode(entry.node, conversation);
      entry.signature = signature;
    }
    order.push(entry.node);
  }
  for (const [id, entry] of listNodes)
    if (!seen.has(id)) {
      entry.node.remove();
      listNodes.delete(id);
    }
  reportUnread();
  const placed = measureRows(
    order.filter((node) => node.parentNode === container),
  );
  let child = container.firstElementChild;
  for (const node of order) {
    if (child === node) {
      child = child.nextElementSibling;
      continue;
    }
    container.insertBefore(node, child);
  }
  while (child) {
    const next = child.nextElementSibling;
    child.remove();
    child = next;
  }
  slideRows(placed);
  if (listRendered)
    for (const conversation of items) {
      const entry = listNodes.get(conversation.id);
      if (entry?.fresh) animateIn(entry.node);
    }
  for (const entry of listNodes.values()) entry.fresh = false;
  listRendered = true;
  if (!items.length)
    container.append(
      el(
        "p",
        conversations.size
          ? "No conversations match."
          : "No conversations yet.",
        "list-empty",
      ),
    );
}

let reportedUnread = -1;
function reportUnread() {
  if (!desktop) return;
  let count = 0;
  for (const conversation of conversations.values())
    if (conversation.unread && !conversation.read_only) count++;
  if (count === reportedUnread) return;
  reportedUnread = count;
  void desktop("set_unread", { count }).catch(() => {});
}

// --- Thread -----------------------------------------------------------------

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
    pending = fetch(api(`/v1/attachments/${encodeURIComponent(id)}`), {
      signal: abort.signal,
      headers: { Authorization: `Bearer ${token}` },
    }).then((response) => {
      if (!response.ok) throw new Error("preview unavailable");
      return response
        .blob()
        .then((blob) =>
          URL.createObjectURL(new Blob([blob], { type: img.dataset.mime })),
        );
    });
    previewCache.set(id, pending);
  }
  try {
    img.src = await pending;
  } catch {
    previewCache.delete(id);
    img.closest(".bubble")?.remove();
  }
}
async function downloadAttachment(attachment) {
  const response = await fetch(
    api(`/v1/attachments/${encodeURIComponent(attachment.id)}`),
    { signal: abort.signal, headers: { Authorization: `Bearer ${token}` } },
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
}
function renderAttachment(attachment, readOnly) {
  if (attachment.available && /^image\//.test(attachment.mime || "")) {
    const bubble = el("div", undefined, "bubble media");
    const img = el("img", undefined, "preview");
    img.alt = attachment.name || "Image attachment";
    img.loading = "lazy";
    img.dataset.attachmentId = attachment.id;
    img.dataset.mime = attachment.mime;
    img.onclick = () => img.src && showImage(img);
    bubble.append(img);
    if (previewObserver) previewObserver.observe(img);
    else void loadPreview(img);
    return bubble;
  }
  const node = el("div", undefined, "attachment");
  const info = el("div", undefined, "file-info");
  info.append(
    el("strong", attachment.name || "Attachment"),
    el(
      "span",
      `${formatSize(attachment.size || 0)}${attachment.preview ? " · Preview" : ""}${
        attachment.available ? "" : " · On phone"
      }`,
    ),
  );
  node.append(icon("file"), info);
  if (attachment.available) {
    const download = button(undefined, "icon-button", async () => {
      download.disabled = true;
      try {
        await downloadAttachment(attachment);
      } catch (error) {
        notice(error.message);
      } finally {
        download.disabled = false;
      }
    });
    download.setAttribute("aria-label", "Download");
    download.append(icon("download"));
    node.append(download);
  }
  if (attachment.requestable && !readOnly) {
    const requestButton = button("Get from phone", "request", async () => {
      requestButton.disabled = true;
      try {
        await request(
          `/v1/attachments/${encodeURIComponent(attachment.id)}/request`,
          { method: "POST" },
        );
        notice("Requested. The message updates when the phone uploads it.");
      } catch (error) {
        notice(error.message);
        requestButton.disabled = false;
      }
    });
    info.append(el("br"), requestButton);
  }
  return node;
}
function showImage(img) {
  const viewer = $("image-viewer");
  viewer.src = img.src;
  viewer.alt = img.alt;
  $("image-dialog").showModal();
}
// The desktop window has no new-window handler, so WebKitGTK silently drops
// target="_blank" and window.open. There the shell opens the link instead.
function openExternal(url) {
  if (desktop && /^https?:/i.test(url)) {
    void desktop("open_external", { url }).catch(() => {});
    return;
  }
  window.open(url, "_blank", "noopener");
}
function textBubble(text, extraClass = "") {
  const bubble = el("div", undefined, `bubble ${extraClass}`.trim());
  const paragraph = el("p");
  for (const segment of linkify(text)) {
    if (!segment.href) {
      paragraph.append(segment.text);
      continue;
    }
    const link = el("a", segment.text);
    link.href = segment.href;
    link.target = "_blank";
    link.rel = "noreferrer noopener";
    paragraph.append(link);
  }
  bubble.append(paragraph);
  return bubble;
}
function reactionKey(conversationID, messageID, emoji, remove = false) {
  return [conversationID, messageID, emoji, remove].join("|");
}
function recentReactions() {
  try {
    const stored = JSON.parse(localStorage.getItem(RECENT_REACTIONS) || "[]");
    return Array.isArray(stored)
      ? stored.filter((emoji) => typeof emoji === "string" && emoji)
      : [];
  } catch {
    return [];
  }
}
function rememberReaction(emoji) {
  const recents = [
    emoji,
    ...recentReactions().filter((recent) => recent !== emoji),
  ].slice(0, 24);
  try {
    localStorage.setItem(RECENT_REACTIONS, JSON.stringify(recents));
  } catch {
    // A browser that refuses storage just loses the recents list.
  }
}
// The quick row keeps what this browser reaches for, padded with the defaults.
function quickReactions() {
  return [...new Set([...recentReactions(), ...DEFAULT_REACTIONS])].slice(
    0,
    QUICK_REACTIONS,
  );
}
function reactionPicker(message) {
  const picker = el("details", undefined, "react");
  const summary = el("summary");
  summary.setAttribute("aria-label", "Add reaction");
  summary.append(icon("emoji"));
  const choices = el("div", undefined, "react-choices");
  for (const emoji of quickReactions()) {
    const choice = button(emoji, undefined, () => {
      picker.open = false;
      void startReaction(message.id, emoji);
    });
    choice.setAttribute("aria-label", `React ${emoji}`);
    choices.append(choice);
  }
  const more = button(undefined, "react-more", () => {
    picker.open = false;
    void openEmojiPicker(message.id);
  });
  more.append(icon("search"));
  more.title = "Search all emoji";
  more.setAttribute("aria-label", "Search all emoji");
  choices.append(more);
  picker.append(summary, choices);
  return picker;
}
function pendingReactionRows(message) {
  const rows = [];
  for (const [key, pending] of pendingReactions) {
    if (
      pending.conversationID !== selected ||
      pending.request.body.message_id !== message.id
    )
      continue;
    const row = el("div", undefined, "pending-reaction");
    row.append(
      el(
        "span",
        `${pending.request.body.emoji} ${
          pending.request.body.remove ? "removal" : "reaction"
        } not acknowledged.`,
      ),
      button("Retry", undefined, () => void submitReaction(key)),
      button("Discard", undefined, () => {
        pendingReactions.delete(key);
        invalidate("thread");
      }),
    );
    for (const control of row.querySelectorAll("button"))
      control.disabled = pending.sending;
    rows.push(row);
  }
  return rows;
}
function messageSignature(row, conversation) {
  const message = row.message;
  const pending = [...pendingReactions.entries()]
    .filter(([, item]) => item.request.body.message_id === message.id)
    .map(([key, item]) => `${key}:${item.sending}`)
    .join(",");
  return JSON.stringify([
    message,
    row.first,
    row.last,
    pending,
    conversation.read_only,
    conversation.participants,
    quickReactions(),
  ]);
}
function buildMessageRow(row, conversation) {
  const message = row.message;
  const direction =
    message.direction === "outgoing" || message.direction === "incoming"
      ? message.direction
      : "system";
  const node = el(
    "div",
    undefined,
    `row ${direction}${row.first ? " first" : ""}${row.last ? " last" : ""}`,
  );
  const sender = conversation.participants?.find(
    (participant) => participant.id === message.sender_id,
  );
  if (direction === "incoming" && row.first && isGroup(conversation) && sender)
    node.append(el("div", sender.name || sender.address, "sender"));
  const line = el("div", undefined, "bubble-line");
  const bubbles = el("div", undefined, "bubbles");
  const attachments = message.deleted ? [] : message.attachments || [];
  const readOnly = message.read_only || conversation.read_only;
  const bubbleClass = `${message.read_only ? "previous-session" : ""}${
    message.deleted ? " deleted" : ""
  }`;
  if (message.deleted)
    bubbles.append(textBubble("Message deleted", bubbleClass));
  else if (message.text || message.subject || !attachments.length) {
    const bubble = textBubble(
      message.text || (attachments.length ? "" : "Message content unavailable"),
      bubbleClass,
    );
    if (message.subject)
      bubble.prepend(el("strong", message.subject, "subject"));
    bubbles.append(bubble);
  }
  for (const attachment of attachments)
    bubbles.append(renderAttachment(attachment, message.read_only));
  for (const bubble of bubbles.children)
    bubble.title = new Date(message.time).toLocaleString();
  line.append(bubbles);
  if (!message.deleted && !readOnly && direction !== "system")
    line.append(reactionPicker(message));
  node.append(line);
  if (message.reactions?.length) {
    const reactions = el("div", undefined, "reactions");
    for (const reaction of message.reactions) {
      const count = reaction.participants?.length || 0;
      const label = `${reaction.emoji}${count > 1 ? ` ${count}` : ""}`;
      const mine = reactedByMe(reaction, conversation);
      const title = reactionTitle(reaction, conversation);
      // Only a reaction of your own can be taken back, so everything else
      // stays a plain chip that still names who reacted on hover.
      const chip =
        readOnly || !mine
          ? el("span", label, "reaction")
          : button(label, "reaction mine", () =>
              startReaction(message.id, reaction.emoji, true),
            );
      chip.title = title;
      chip.setAttribute(
        "aria-label",
        mine && !readOnly ? `Remove your reaction · ${title}` : title,
      );
      reactions.append(chip);
    }
    node.append(reactions);
  }
  node.append(...pendingReactionRows(message));
  const meta = [];
  let failed = false;
  if (message.read_only) meta.push("Previous pairing · read only");
  if (direction === "outgoing" && row.last) {
    const status = messageStatus(message.status);
    if (status.label) {
      meta.push(status.label);
      failed = status.error;
    }
  }
  if (meta.length)
    node.append(el("div", meta.join(" · "), `meta${failed ? " error" : ""}`));
  return node;
}
function buildOutboxRow(row) {
  const item = row.item;
  const kind = item.request?.kind;
  const status = outboxStatus(item, providerState === "connected");
  const node = el(
    "div",
    undefined,
    `row ${kind === "reaction" ? "system" : "outgoing"}${row.first ? " first" : ""}${
      row.last ? " last" : ""
    }`,
  );
  const line = el("div", undefined, "bubble-line");
  const bubbles = el("div", undefined, "bubbles");
  const text = outboxText(item);
  const count = outboxAttachmentCount(item);
  if (text || !count) bubbles.append(textBubble(text || "Message"));
  if (count)
    bubbles.append(textBubble(`${count} attachment${count === 1 ? "" : "s"}`));
  for (const bubble of bubbles.children)
    bubble.title = new Date(item.created).toLocaleString();
  line.append(bubbles);
  node.append(line);
  const meta = el(
    "div",
    `${item.session_epoch !== currentSessionEpoch ? "Previous pairing · " : ""}${status.label}`,
    `meta${status.error ? " error" : ""}`,
  );
  // The bridge's own words about the send stay available without spending a
  // line of the thread on them.
  if (item.detail) meta.title = item.detail;
  if (item.state === "queued")
    meta.append(
      button("Cancel", undefined, async () => {
        try {
          await request(`/v1/outbox/${encodeURIComponent(item.id)}/cancel`, {
            method: "POST",
          });
        } catch (error) {
          notice(error.message);
        }
      }),
    );
  node.append(meta);
  return node;
}

const threadNodes = new Map();
let threadNodesFor = "";
let preserveScroll = false;
let threadSwitching = false;
let fullHistory;
let bulkRows = false;
function renderThread() {
  const bulk = bulkRows;
  bulkRows = false;
  const conversation = conversations.get(selected);
  $("empty").hidden = !!conversation || composing;
  $("new-chat").hidden = !composing;
  $("thread").hidden = !conversation || composing;
  document.body.classList.toggle("thread-open", !!conversation || composing);
  if ($("people-dialog").open) renderPeople();
  if (!conversation) return;
  fillAvatar($("thread-avatar"), conversation);
  $("thread-title").textContent = displayName(conversation);
  $("thread-info").textContent = `${
    others(conversation)
      .map((participant) => participant.address || participant.name)
      .join(", ") || (conversation.protocol || "message").toUpperCase()
  }${conversation.read_only ? " · Previous pairing · read only" : ""}`;
  const entry = thread(selected);
  const messages = [...entry.messages.values()];
  const rows = layoutThread(
    messages,
    threadOutbox(
      [...outbox.values()],
      selected,
      new Set(messages.map((message) => message.id)),
    ),
  );
  const container = $("messages");
  const rowsNode = $("rows");
  // Messages for a newly selected conversation usually land a render after the
  // switch, so the thread animates once as a whole instead of row by row.
  const switched = threadNodesFor !== selected;
  if (switched) {
    threadNodes.clear();
    rowsNode.replaceChildren();
    threadNodesFor = selected;
    threadSwitching = true;
    animateIn($("thread").querySelector(".thread-header"));
  }
  // A message that takes over from its own outbox row is already on screen, so
  // it slots into that row's place instead of animating in as something new.
  const replacing = new Set();
  for (const item of outbox.values())
    if (item.message_id && threadNodes.has(`o:${item.id}`))
      replacing.add(item.message_id);
  const atBottom =
    container.scrollHeight - container.scrollTop - container.clientHeight < 80;
  const previousHeight = container.scrollHeight;
  const seen = new Set();
  const order = [];
  for (const row of rows) {
    seen.add(row.key);
    const signature =
      row.kind === "divider"
        ? row.label
        : row.kind === "message"
          ? messageSignature(row, conversation)
          : JSON.stringify([
              row.item,
              row.first,
              row.last,
              currentSessionEpoch,
              providerState,
            ]);
    let cached = threadNodes.get(row.key);
    const fresh = !cached;
    if (!cached || cached.signature !== signature) {
      const node =
        row.kind === "divider"
          ? el("div", row.label, "divider")
          : row.kind === "message"
            ? buildMessageRow(row, conversation)
            : buildOutboxRow(row);
      if (cached) cached.node.replaceWith(node);
      cached = { node, signature };
      threadNodes.set(row.key, cached);
      if (
        fresh &&
        !threadSwitching &&
        !bulk &&
        !(row.kind === "message" && replacing.has(row.message.id))
      )
        animateIn(node);
    }
    order.push(cached.node);
  }
  for (const [key, cached] of threadNodes)
    if (!seen.has(key)) {
      cached.node.remove();
      threadNodes.delete(key);
    }
  let child = rowsNode.firstElementChild;
  for (const node of order) {
    if (child === node) {
      child = child.nextElementSibling;
      continue;
    }
    rowsNode.insertBefore(node, child);
  }
  while (child) {
    const next = child.nextElementSibling;
    child.remove();
    child = next;
  }
  if (threadSwitching && rows.length) {
    threadSwitching = false;
    animateIn(rowsNode);
  }
  const importState = renderImportStatus(conversation);
  const arriving =
    importState?.state === "active" || importState?.state === "waiting";
  if (!rows.length && (!entry.fetched || arriving))
    rowsNode.append(skeletonThread());
  else if (!rows.length)
    rowsNode.append(
      el("p", "No messages in stored history yet.", "thread-empty"),
    );
  $("older").hidden = !entry.before && !fullHistory && !importState;
  $("load-older").hidden = !!fullHistory || !entry.before;
  $("older-progress").hidden = !fullHistory;
  if (entry.scrollToTop) {
    container.scrollTop = 0;
    entry.scrollToTop = false;
    preserveScroll = false;
  } else if (preserveScroll) {
    container.scrollTop += container.scrollHeight - previousHeight;
    preserveScroll = false;
  } else if (atBottom || entry.scrollToBottom) {
    container.scrollTop = container.scrollHeight;
    if (entry.fetched) entry.scrollToBottom = false;
  }
  $("mark-read").disabled =
    conversation.read_only || !messages.some((message) => !message.read_only);
  $("import-conversation").disabled = conversation.read_only || importing > 0;
  $("load-full-history").disabled = !!fullHistory;
}

// A thread with nothing in it yet reads as an empty conversation, which is the
// wrong thing to say while its messages are still on their way. Placeholder
// bubbles say "loading" in the shape the messages will take.
const SKELETON_ROWS = [
  ["in", 62],
  ["out", 44],
  ["in", 78],
  ["out", 56],
  ["in", 38],
];
function skeletonThread() {
  const node = el("div", undefined, "skeleton");
  node.ariaHidden = "true";
  for (const [side, width] of SKELETON_ROWS) {
    const row = el("div", undefined, `skeleton-row ${side}`);
    const bubble = el("div", undefined, "skeleton-bubble");
    bubble.style.width = `${width}%`;
    row.append(bubble);
    node.append(row);
  }
  return node;
}

// A thread only holds what the bridge has imported so far, so an unfinished
// import is reported where the stored history runs out instead of being left to
// look like the whole conversation.
function renderImportStatus(conversation) {
  const id = `messages:${conversation.id}`;
  const state = importStatus(historyJobs.get(id), {
    ahead: Math.max(queuePosition([...historyJobs.values()], id), 0),
    connected: providerState === "connected",
  });
  const node = $("import-status");
  node.hidden = !state;
  if (state) {
    node.className = `import-status ${state.state}`;
    $("import-status-text").textContent = state.label;
  }
  const action = $("import-status-action");
  action.hidden = !state?.action;
  if (state?.action) {
    action.textContent = state.action;
    action.disabled = conversation.read_only || importing > 0;
  }
  return state;
}

// --- Contacts and recipient picking ----------------------------------------

async function loadContacts({ refresh = false } = {}) {
  if (contactsLoading) return contactsLoading;
  const requestGeneration = generation;
  contactsLoading = (async () => {
    try {
      const book = await request(`/v1/contacts${refresh ? "?refresh=1" : ""}`);
      if (requestGeneration !== generation) return;
      contactBook = {
        contacts: book?.contacts || [],
        stale: !!book?.stale,
        updated: book?.updated || "",
      };
      invalidate("newchat");
      if ($("people-dialog").open) renderPeople();
    } catch {
      // A phone that has never been read leaves the picker to typed numbers,
      // which still start a conversation.
    } finally {
      contactsLoading = undefined;
    }
  })();
  return contactsLoading;
}

// One recipient picker: chips for who is chosen and a list completing what is
// typed from the address book. The compose view and the group's Add people
// section are the same control over different fields.
function createPicker({ chips, input, list, onChange, excluded }) {
  let chosen = [];
  let active = -1;
  let locked = false;
  const suggestions = () => {
    const query = $(input).value;
    const typed = typedRecipient(query);
    const matches = matchContacts(contactBook.contacts, query, {
      exclude: [...chosen, ...(excluded?.() || [])],
    });
    if (typed && !matches.some((match) => match.address === typed.address))
      return [typed, ...matches];
    return matches;
  };
  const choose = (contact) => {
    if (
      locked ||
      chosen.some((entry) => contactKey(entry) === contactKey(contact))
    )
      return;
    chosen = [...chosen, contact];
    $(input).value = "";
    active = -1;
    picker.render();
    // Choosing from the list leaves the field ready for the next recipient
    // instead of leaving focus on a suggestion that is no longer there.
    $(input).focus();
    onChange?.();
  };
  const remove = (contact) => {
    if (locked) return;
    chosen = chosen.filter(
      (entry) => contactKey(entry) !== contactKey(contact),
    );
    picker.render();
    $(input).focus();
    onChange?.();
  };
  const picker = {
    get chosen() {
      return chosen;
    },
    addresses: () => chosen.map((contact) => contact.address),
    set(contacts) {
      chosen = [...contacts];
      active = -1;
      picker.render();
    },
    lock(value) {
      locked = value;
      $(input).disabled = value;
      picker.render();
    },
    clear() {
      chosen = [];
      active = -1;
      locked = false;
      $(input).value = "";
      $(input).disabled = false;
      picker.render();
    },
    // The typed-but-not-yet-chosen number counts as a recipient, so a number
    // typed in full never has to be confirmed before submitting.
    recipients() {
      const addresses = picker.addresses();
      const typed = typedRecipient($(input).value);
      if (typed && !addresses.includes(typed.address))
        addresses.push(typed.address);
      return addresses;
    },
    render() {
      const chipsNode = $(chips);
      chipsNode.replaceChildren();
      for (const contact of chosen) {
        const chip = el("span", undefined, "recipient-chip");
        chip.append(el("span", contactName(contact)));
        const close = button("", "chip-remove", () => remove(contact));
        close.append(icon("close"));
        close.setAttribute("aria-label", `Remove ${contactName(contact)}`);
        close.disabled = locked;
        chip.append(close);
        chipsNode.append(chip);
      }
      const listNode = $(list);
      const options = locked ? [] : suggestions();
      if (active >= options.length) active = options.length - 1;
      listNode.replaceChildren();
      for (const [index, contact] of options.entries()) {
        const row = button("", "contact-row", () => choose(contact));
        row.setAttribute("role", "option");
        row.setAttribute("aria-selected", String(index === active));
        if (index === active) row.classList.add("active");
        const avatar = el("span", initials(contactName(contact)), "avatar");
        avatar.style.background = avatarColor(contactName(contact));
        if (!avatar.textContent) avatar.append(icon("person"));
        row.append(avatar);
        const text = el("span", undefined, "contact-text");
        text.append(el("span", contactName(contact), "name"));
        const detail = contactDetail(contact);
        if (detail) text.append(el("span", detail, "detail"));
        row.append(text);
        listNode.append(row);
      }
      listNode.hidden = !options.length;
      $(input).setAttribute("aria-expanded", String(!!options.length));
    },
    onInput() {
      active = -1;
      picker.render();
      onChange?.();
    },
    onKeyDown(event) {
      const options = suggestions();
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        if (!options.length) return;
        event.preventDefault();
        active += event.key === "ArrowDown" ? 1 : -1;
        if (active >= options.length) active = -1;
        if (active < -1) active = options.length - 1;
        picker.render();
        return;
      }
      if (event.key === "Enter") {
        if (active >= 0 && options[active]) {
          event.preventDefault();
          choose(options[active]);
        } else if (options.length === 1 && typedRecipient($(input).value)) {
          event.preventDefault();
          choose(options[0]);
        }
        return;
      }
      // A separator ends a typed number the way a comma does in a mail client.
      if ([",", ";", " "].includes(event.key)) {
        const typed = typedRecipient($(input).value);
        if (typed) {
          event.preventDefault();
          choose(typed);
        }
        return;
      }
      if (event.key === "Escape" && options.length) {
        event.stopPropagation();
        active = -1;
        $(input).value = "";
        picker.render();
        onChange?.();
        return;
      }
      if (event.key === "Backspace" && !$(input).value && chosen.length)
        remove(chosen[chosen.length - 1]);
    },
  };
  $(input).oninput = () => picker.onInput();
  $(input).onkeydown = (event) => picker.onKeyDown(event);
  return picker;
}

const composePicker = createPicker({
  chips: "recipient-chips",
  input: "recipients",
  list: "contact-suggestions",
  onChange: () => invalidate("newchat"),
});
const addPicker = createPicker({
  chips: "add-chips",
  input: "add-recipients",
  list: "add-suggestions",
  // Someone already in the conversation is not someone to add to it.
  excluded: () => others(conversations.get(peopleConversation) || {}),
  onChange: () => renderAddPeople(),
});

function renderNewChat() {
  // A number typed but not yet turned into a chip is already a recipient, so
  // the group it would make is named from the same count the request uses.
  const recipients = composePicker.recipients();
  $("group-name-field").hidden = recipients.length < 2;
  $("group-name").disabled = composeLocked;
  $("compose-hint").textContent = composeHint(recipients, contactBook);
  $("create-conversation").disabled =
    creatingConversation || !recipients.length;
  composePicker.render();
}

function renderSelectedAttachments() {
  const files = pendingSend?.files || selectedFiles;
  const chips = $("selected-attachments");
  chips.replaceChildren();
  for (const file of files)
    chips.append(el("span", `${file.name} (${formatSize(file.size)})`, "chip"));
  if (files.length) {
    const total = files.reduce((sum, file) => sum + file.size, 0);
    chips.append(
      el(
        "span",
        `${files.length}/10 · ${formatSize(total)} of 20 MB`,
        "chip total",
      ),
    );
  }
}
function renderCompose() {
  const conversation = conversations.get(selected);
  if (!conversation) return;
  const hasPending = !!pendingSend;
  $("text").disabled = conversation.read_only || hasPending;
  $("attachments").disabled = conversation.read_only || hasPending;
  $("send").disabled = conversation.read_only || sending || hasPending;
  $("text").placeholder = conversation.read_only
    ? "Read only · previous pairing"
    : conversation.protocol === "rcs"
      ? "RCS message"
      : "Text message";
  $("pending-send").hidden = !hasPending;
  if (hasPending) {
    $("pending-send-text").textContent = sending
      ? pendingSend.progress || "Sending…"
      : pendingSend.error || "Message not sent.";
    $("retry-send").hidden = sending;
    $("discard-send").hidden = sending;
  }
  $("send-hint").textContent =
    providerState === "connected"
      ? ""
      : providerState === "authentication_required"
        ? "Pair your phone before sending."
        : providerState === "connection_failed"
          ? "Phone connection failed. Messages queue until it reconnects."
          : "Phone offline. Messages queue until it connects.";
  renderSelectedAttachments();
}

// --- Status, banner, settings ----------------------------------------------

function renderStatus() {
  const state = providerState.replaceAll("_", " ");
  $("status").textContent = token ? state : "Locked";
  $("provider-detail").textContent = status.detail || "";
  $("sync-status").textContent = status.last_sync
    ? `Recent history checked ${new Date(status.last_sync).toLocaleTimeString()}.${
        status.sync_state === "failed" ? " Latest check failed." : ""
      }`
    : token
      ? "Recent history has not been reconciled yet."
      : "";
  const previousCount =
    (status.previous_session_conversations || 0) +
    (status.previous_session_messages || 0);
  $("session-boundary").hidden = !previousCount;
  $("session-boundary-text").textContent = previousCount
    ? `${status.previous_session_conversations || 0} conversations and ${
        status.previous_session_messages || 0
      } messages from an earlier pairing are read-only.`
    : "";
  const needsAction = ["connection_failed", "authentication_required"].includes(
    providerState,
  );
  $("connection-actions").hidden = !needsAction;
  $("retry-connection").hidden = providerState === "authentication_required";
  $("connection-recovery").textContent =
    providerState === "authentication_required"
      ? status.reason === "session_expired"
        ? "Your phone session expired or was revoked. Pair again and confirm the emoji on your phone."
        : "Pair with your phone and confirm the emoji it shows."
      : "Retry the connection. If credentials changed, pair again.";
  const banner = $("banner");
  banner.hidden = !token || providerState === "connected";
  banner.className = `banner${needsAction ? " error" : ""}`;
  const action = $("banner-action");
  action.onclick = null;
  action.textContent = "";
  if (providerState === "authentication_required") {
    $("banner-text").textContent =
      status.reason === "session_expired"
        ? "Your phone session expired. Pair again to continue."
        : "Pair your phone to continue.";
    action.textContent =
      status.reason === "session_expired" ? "Reconnect phone" : "Pair phone";
    action.onclick = () => openDialog("pairing-dialog");
  } else if (providerState === "connection_failed") {
    $("banner-text").textContent = `Can't reach your phone.${
      status.detail ? ` ${status.detail}` : ""
    }`;
    action.textContent = "Retry";
    action.onclick = retryConnection;
  } else if (providerState === "connecting") {
    $("banner-text").textContent = "Connecting to your phone…";
  } else {
    $("banner-text").textContent = `Phone connection ${state}.${
      status.detail ? ` ${status.detail}` : " Messages queue until it connects."
    }`;
  }
}

function openDialog(id) {
  closeMenus();
  const dialog = $(id);
  if (!dialog.open) dialog.showModal();
  if (id === "pairing-dialog") {
    pairingPanelOpen = true;
    void loadPairingState();
  }
  if (id === "history-dialog") renderHistory();
}
for (const dialog of document.querySelectorAll("dialog")) {
  for (const close of dialog.querySelectorAll("[data-close]"))
    close.onclick = () => dialog.close();
  dialog.onclick = (event) => {
    if (event.target === dialog) dialog.close();
  };
}
$("image-dialog").onclick = () => $("image-dialog").close();
$("image-dialog").onclose = () => $("image-viewer").removeAttribute("src");
$("pairing-dialog").onclose = () => {
  pairingPanelOpen = false;
  clearTimeout(pairingPoll);
  pairingPoll = undefined;
};
$("emoji-dialog").onclose = () => {
  emojiTarget = undefined;
};
$("emoji-search").oninput = () => {
  if (emojiCatalog) renderEmojiGrid();
};
$("emoji-search").onkeydown = (event) => {
  if (event.key !== "Enter") return;
  event.preventDefault();
  $("emoji-grid").querySelector("button[data-emoji]")?.click();
};
$("emoji-grid").onclick = (event) => {
  const choice = event.target.closest("button[data-emoji]");
  if (!choice || !emojiTarget) return;
  const { conversationID, messageID } = emojiTarget;
  $("emoji-dialog").close();
  void startReaction(messageID, choice.dataset.emoji, false, conversationID);
};

function closeMenus() {
  for (const menu of document.querySelectorAll(".menu")) menu.hidden = true;
}
function bindMenu(buttonID, menuID) {
  $(buttonID).onclick = (event) => {
    event.stopPropagation();
    const open = $(menuID).hidden;
    closeMenus();
    $(menuID).hidden = !open;
  };
  $(menuID).onclick = (event) => {
    if (event.target.closest("button")) closeMenus();
  };
}
bindMenu("main-menu-button", "main-menu");
bindMenu("thread-menu-button", "thread-menu");
document.addEventListener("click", closeMenus);
document.addEventListener("click", (event) => {
  if (!desktop || event.defaultPrevented || event.button) return;
  const link = event.target.closest?.("a[href]");
  if (!link || link.hasAttribute("download")) return;
  const external = link.target === "_blank" || link.origin !== location.origin;
  if (!external || !/^https?:/i.test(link.href)) return;
  event.preventDefault();
  openExternal(link.href);
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") closeMenus();
});

// --- Pairing ----------------------------------------------------------------

function pairingActive() {
  return ["waiting_for_login", "connecting", "confirm_on_phone"].includes(
    pairingState.state,
  );
}
function renderPairing() {
  const state =
    pairingState.state ||
    (pairingState.required ? "pairing required" : "not started");
  $("bridge-url").value = pairingState.ticket
    ? apiBase || window.location.origin
    : "";
  $("pairing-ticket").value = pairingState.ticket || "";
  $("pairing-fields").hidden =
    !pairingState.ticket || pairingState.agent_enrolled;
  $("pairing-emoji").hidden =
    state !== "confirm_on_phone" || !pairingState.emoji;
  $("pairing-emoji").textContent = pairingState.emoji
    ? `Confirm ${pairingState.emoji} on your phone`
    : "";
  $("pairing-status").textContent = `${state.replaceAll("_", " ")}${
    pairingState.detail ? ` · ${pairingState.detail}` : ""
  }${
    pairingActive() && pairingState.expires
      ? ` · expires ${new Date(pairingState.expires).toLocaleTimeString()}`
      : ""
  }`;
  $("start-pairing").disabled = pairingActive();
  $("start-pairing").textContent = "Start browser sign-in";
  $("repair-pairing").hidden =
    !pairingState.can_repair && !pairingState.agent_enrolled;
  $("repair-intro").hidden =
    !pairingState.can_repair && !pairingState.agent_enrolled;
  $("repair-pairing").disabled =
    pairingActive() && state !== "waiting_for_login";
  const setup = $("pairing-browser-setup");
  const setupState = `${state}:${!!pairingState.can_repair}:${!!pairingState.agent_enrolled}`;
  if (setup.dataset.state !== setupState) {
    setup.open =
      (!pairingState.can_repair && !pairingState.agent_enrolled) ||
      (state === "waiting_for_login" && !pairingState.agent_enrolled) ||
      state === "failed";
    setup.dataset.state = setupState;
  }
  $("pairing-helper-download").href = api("/pairing-helper.zip");
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
        void loadAll().catch((error) => notice(error.message));
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

// --- History ----------------------------------------------------------------

function jobLabel(job) {
  if (job.conversation_id) {
    const conversation = conversations.get(job.conversation_id);
    return conversation ? displayName(conversation) : "One conversation";
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
  if (!$("history-dialog").open) return;
  $("import-all").disabled = importing > 0;
  const jobs = [...historyJobs.values()];
  const summary = summarizeHistory(jobs);
  $("history-summary").textContent = jobs.length
    ? `${summary.total} imports · ${summary.active} in progress · ${summary.complete} complete · ${summary.failed} failed · ${summary.paused} paused`
    : "";
  const container = $("history-jobs");
  container.replaceChildren();
  if (!jobs.length) {
    container.append(el("p", "No imports requested yet.", "hint"));
    return;
  }
  const shown = jobs
    .sort(
      (a, b) =>
        (a.state === "complete") - (b.state === "complete") ||
        (b.updated || "").localeCompare(a.updated || ""),
    )
    .slice(0, 100);
  for (const job of shown) {
    const previousPairing = job.session_epoch !== currentSessionEpoch;
    const node = el("div", undefined, `history-job ${job.state}`);
    node.append(
      el("strong", jobLabel(job)),
      el(
        "span",
        `${previousPairing ? "Previous pairing · " : ""}${(job.state || "queued").replaceAll("_", " ")} · ${job.pages || 0} pages · ${job.records || 0} records${
          job.detail ? ` · ${job.detail}` : ""
        }`,
        "state",
      ),
    );
    const actions = el("div", undefined, "button-row");
    if (job.state === "queued")
      actions.append(
        button("Pause", "text-button", () => void pauseHistory(job.id)),
      );
    if (["paused", "failed"].includes(job.state))
      actions.append(
        button("Resume", "text-button", () => void queueHistory(job, false)),
      );
    if (["complete", "failed"].includes(job.state))
      actions.append(
        button("Fresh scan", "text-button", () => void queueHistory(job, true)),
      );
    if (actions.childElementCount) node.append(actions);
    container.append(node);
  }
  if (jobs.length > shown.length)
    container.append(
      el("p", `${jobs.length - shown.length} more imports not shown.`, "hint"),
    );
}
async function queueHistory(job, restart) {
  const actionGeneration = generation;
  importing++;
  invalidate("history", "thread");
  try {
    await request("/v1/history", {
      method: "POST",
      body: JSON.stringify(historyBody(job, restart)),
    });
    if (actionGeneration !== generation) return;
    notice(restart ? "Fresh history scan queued." : "History import queued.");
  } catch (error) {
    if (actionGeneration !== generation) return;
    notice(error.message);
  } finally {
    if (actionGeneration === generation) {
      importing--;
      invalidate("history", "thread");
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
    notice("History import paused.");
  } catch (error) {
    if (actionGeneration !== generation) return;
    notice(error.message);
  }
}

// --- Selection and new conversations ---------------------------------------

function stashDraft() {
  if (!selected) return;
  drafts.set(selected, $("text").value);
  draftFiles.set(selected, selectedFiles);
}
async function select(id) {
  if (pendingSend && id !== selected) {
    notice(
      "Retry or discard the pending message before switching conversations.",
    );
    return;
  }
  stashDraft();
  stopFullHistory();
  selected = id;
  composing = false;
  selectedFiles = draftFiles.get(id) || [];
  $("text").value = drafts.get(id) || "";
  $("attachments").value = "";
  autogrow();
  const entry = thread(id);
  entry.scrollToBottom = true;
  invalidate("list", "thread", "compose");
  receiptAttempts.delete(id);
  try {
    await loadThread(id);
  } catch (error) {
    notice(error.message);
  }
  scheduleReadReceipt();
}
function deselect() {
  stashDraft();
  stopFullHistory();
  selected = "";
  composing = false;
  invalidate("list", "thread");
}
function maybeSelectCreatedConversation() {
  if (!createdConversation || createdConversation.selecting) return;
  const tracked = outbox.get(createdConversation.outboxID);
  if (
    tracked &&
    ["canceled", "rejected", "ambiguous"].includes(tracked.state)
  ) {
    // A request that did not produce a conversation comes back as a filled-in
    // compose view, so the recipients it was for can be reviewed and resubmitted.
    $("people-dialog").close();
    composeLocked = false;
    composePicker.lock(false);
    composePicker.set(recipientsOf(tracked.request?.recipients || []));
    $("group-name").value = tracked.request?.group_name || "";
    composing = true;
    $("conversation-status").textContent = `${tracked.state}: ${
      tracked.detail || "Conversation was not created."
    } Review the recipients before submitting a new request.`;
    createdConversation = undefined;
    invalidate("thread", "newchat");
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
  if (!conversations.has(conversationID)) {
    $("conversation-status").textContent =
      "Conversation accepted; waiting for its stored record…";
    return;
  }
  createdConversation.selecting = true;
  queueMicrotask(() => {
    createdConversation = undefined;
    $("conversation-status").textContent = "";
    resetCompose();
    void select(conversationID);
  });
}
const normalizeOutbox = (response) => response?.outbox || response;

// Rebuilds picker entries from bare addresses, naming them from the address
// book so a resubmitted request still reads as people rather than numbers.
function recipientsOf(addresses) {
  return addresses.map(
    (address) =>
      contactBook.contacts.find((contact) => contact.address === address) ||
      typedRecipient(address) || { id: "", name: "", address },
  );
}
function resetCompose() {
  pendingConversation = undefined;
  composeLocked = false;
  composePicker.clear();
  $("group-name").value = "";
  $("conversation-status").textContent = "";
  $("create-conversation").textContent = "Create";
  invalidate("newchat");
}
async function createConversation() {
  if (!pendingConversation) {
    const recipients = composePicker.recipients();
    if (!recipients.length) {
      $("conversation-status").textContent =
        "Choose a contact, or type an international number such as +14155550100.";
      return;
    }
    pendingConversation = newConversationRequest(
      recipients,
      recipients.length > 1 ? $("group-name").value : "",
    );
  }
  const pending = pendingConversation;
  creatingConversation = true;
  composeLocked = true;
  composePicker.lock(true);
  $("create-conversation").disabled = true;
  $("conversation-status").textContent = "Requesting conversation…";
  invalidate("newchat");
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
    $("conversation-status").textContent =
      "Conversation request accepted. Waiting for your phone…";
    maybeSelectCreatedConversation();
  } catch (error) {
    if (pending !== pendingConversation) return;
    $("conversation-status").textContent =
      `${error.message} Retry preserves the same idempotency key and recipients.`;
  } finally {
    creatingConversation = false;
    if (pending === pendingConversation) {
      $("create-conversation").textContent = "Retry";
    } else {
      composeLocked = false;
      composePicker.lock(false);
      $("create-conversation").textContent = "Create";
    }
    $("create-conversation").disabled = false;
    invalidate("newchat");
  }
}

// --- Reactions, uploads, sending -------------------------------------------

async function startReaction(
  messageID,
  emoji,
  remove = false,
  conversationID = selected,
) {
  if (!conversationID) return;
  if (!remove) rememberReaction(emoji);
  const key = reactionKey(conversationID, messageID, emoji, remove);
  if (!pendingReactions.has(key))
    pendingReactions.set(key, {
      conversationID,
      request: newReactionRequest(messageID, emoji, remove),
      sending: false,
    });
  await submitReaction(key);
}

// The catalog is a large generated module, so it is fetched the first time
// somebody looks past the quick row.
async function openEmojiPicker(messageID) {
  emojiTarget = { conversationID: selected, messageID };
  $("emoji-search").value = "";
  emojiGroup = undefined;
  $("emoji-grid").replaceChildren();
  $("emoji-tabs").replaceChildren();
  openDialog("emoji-dialog");
  if (!emojiCatalog) {
    $("emoji-status").textContent = "Loading emoji…";
    try {
      emojiCatalog = await import("/emoji.mjs");
    } catch {
      $("emoji-status").textContent =
        "Emoji list unavailable. Reload and try again.";
      return;
    }
    if (!$("emoji-dialog").open) return;
  }
  renderEmojiTabs();
  renderEmojiGrid();
  $("emoji-search").focus();
}
function emojiCategories() {
  const recents = recentReactions().map(
    (emoji) => emojiCatalog.lookupEmoji(emoji) || { emoji, label: emoji },
  );
  return recents.length
    ? [{ name: "Recent", tab: "🕘", emoji: recents }, ...emojiCatalog.GROUPS]
    : emojiCatalog.GROUPS;
}
function renderEmojiTabs() {
  $("emoji-tabs").replaceChildren(
    ...emojiCategories().map((category) => {
      const tab = button(
        category.tab || category.emoji[0]?.emoji || "?",
        "emoji-tab",
        () => {
          emojiGroup = category.name;
          $("emoji-search").value = "";
          renderEmojiGrid();
        },
      );
      tab.dataset.group = category.name;
      tab.title = category.name;
      tab.setAttribute("aria-label", category.name);
      return tab;
    }),
  );
}
function renderEmojiGrid() {
  const query = $("emoji-search").value.trim();
  const categories = emojiCategories();
  let entries;
  if (query) {
    entries = emojiCatalog.searchEmoji(query);
    $("emoji-status").textContent = entries.length
      ? ""
      : `No emoji match “${query}”.`;
  } else {
    const category =
      categories.find((entry) => entry.name === emojiGroup) || categories[0];
    emojiGroup = category.name;
    entries = category.emoji;
    $("emoji-status").textContent = "";
  }
  $("emoji-grid").replaceChildren(
    ...entries.map((entry) => {
      const choice = button(entry.emoji, "emoji-choice");
      choice.dataset.emoji = entry.emoji;
      choice.title = entry.label || entry.emoji;
      choice.setAttribute("aria-label", entry.label || entry.emoji);
      return choice;
    }),
  );
  for (const tab of $("emoji-tabs").children)
    tab.setAttribute(
      "aria-current",
      String(!query && tab.dataset.group === emojiGroup),
    );
  $("emoji-grid").scrollTop = 0;
}
async function submitReaction(key) {
  const pending = pendingReactions.get(key);
  if (!pending || pending.sending) return;
  pending.sending = true;
  invalidate("thread");
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
  } catch (error) {
    if (pendingReactions.get(key) !== pending) return;
    notice(`${error.message} Retry reuses the same request.`);
  } finally {
    pending.sending = false;
    invalidate("thread");
  }
}
async function uploadFiles(pending) {
  while (pending.uploadIDs.length < pending.files.length) {
    const file = pending.files[pending.uploadIDs.length];
    pending.progress = `Uploading ${pending.uploadIDs.length + 1} of ${pending.files.length}: ${file.name}`;
    invalidate("compose");
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
    if (!text.trim() && !selectedFiles.length) return;
    pendingSend = {
      conversationID: selected,
      files: [...selectedFiles],
      uploadIDs: [],
      text,
      requests: undefined,
      progress: "",
      error: "",
    };
  }
  const pending = pendingSend,
    sendGeneration = generation;
  sending = true;
  pending.error = "";
  invalidate("compose");
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
          ? `Sending ${item.body.attachment_ids.length ? "attachments" : "caption"}…`
          : "Sending…";
      invalidate("compose");
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
    autogrow();
    drafts.delete(selected);
    draftFiles.delete(selected);
    thread(selected).scrollToBottom = true;
    $("text").focus();
  } catch (error) {
    if (sendGeneration !== generation || pendingSend !== pending) return;
    pending.progress = "";
    pending.error = `${error.message} Retry keeps the same message and finished uploads.`;
  } finally {
    if (sendGeneration === generation) {
      sending = false;
      invalidate("compose", "thread");
    }
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

// --- Notifications ----------------------------------------------------------

const notified = new Set();
function notifyIncoming(message) {
  if (
    !$("notify").checked ||
    (!desktop &&
      (typeof Notification !== "function" ||
        Notification.permission !== "granted")) ||
    message.direction !== "incoming" ||
    message.deleted ||
    notified.has(message.id) ||
    (!document.hidden && message.conversation_id === selected)
  )
    return;
  notified.add(message.id);
  const conversation = conversations.get(message.conversation_id);
  const sender = conversation?.participants?.find(
    (participant) => participant.id === message.sender_id,
  );
  const title = conversation?.name || sender?.name || "New message";
  const body =
    message.text ||
    (message.attachments?.length ? "Attachment" : "New message");
  const text =
    sender && conversation?.participants?.length > 2
      ? `${sender.name || sender.address}: ${body}`
      : body;
  if (desktop) {
    void desktop("notify", { title, body: text }).catch(() => {});
    return;
  }
  const notification = new Notification(title, { body: text, tag: message.id });
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
    if (desktop) notice(wanted ? "Notifications on." : "Notifications off.");
    else if (wanted) {
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
  (!!desktop ||
    (typeof Notification === "function" &&
      Notification.permission === "granted"));
renderNotifyControls();

// --- Lock / unlock ----------------------------------------------------------

function clearPrivateUI() {
  pushSynced = false;
  for (const pending of previewCache.values())
    pending.then((url) => URL.revokeObjectURL(url)).catch(() => {});
  previewCache.clear();
  notified.clear();
  receiptAttempts.clear();
  clearTimeout(receiptTimer);
  pendingSend = pendingConversation = createdConversation = undefined;
  selectedFiles = [];
  conversations.clear();
  outbox.clear();
  historyJobs.clear();
  threads.clear();
  listNodes.clear();
  listRendered = false;
  threadNodes.clear();
  threadNodesFor = "";
  selected = "";
  composing = false;
  cursor = conversationCursor = outboxCursor = historyCursor = 0;
  typingUntil = 0;
  typingConversation = "";
  status = {};
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
  pendingReactions.clear();
  for (const dialog of document.querySelectorAll("dialog")) dialog.close();
  closeMenus();
  notice("");
  for (const id of [
    "provider-detail",
    "sync-status",
    "conversation-status",
    "thread-title",
    "thread-info",
    "pairing-status",
    "pairing-emoji",
    "history-summary",
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
  contactBook = { contacts: [], stale: false };
  peopleConversation = "";
  addPicker.clear();
  resetCompose();
  $("pairing-fields").hidden = true;
  $("connection-actions").hidden = true;
  $("banner").hidden = true;
  $("session-boundary").hidden = true;
  $("rows").replaceChildren();
  $("history-jobs").replaceChildren();
  $("selected-attachments").replaceChildren();
  $("typing").hidden = true;
  document.body.classList.remove("thread-open");
  renderStatus();
  renderList();
  renderThread();
}
$("login-form").onsubmit = async (event) => {
  event.preventDefault();
  token = $("token").value.trim();
  abort?.abort();
  abort = new AbortController();
  generation++;
  try {
    await loadAll();
    $("token").value = "";
    $("login").hidden = true;
    $("app").hidden = false;
    notice("");
    if (desktop) void desktop("save_token", { token }).catch(() => {});
    void stream(generation);
  } catch (error) {
    token = "";
    alert(error.message);
  }
};
$("logout").onclick = () => {
  if (desktop) void desktop("clear_token").catch(() => {});
  abort?.abort();
  generation++;
  token = "";
  clearPrivateUI();
  $("app").hidden = true;
  $("login").hidden = false;
};

// --- Wiring -----------------------------------------------------------------

$("search").oninput = () => invalidate("list");
$("open-pairing").onclick = () => openDialog("pairing-dialog");
$("settings-pairing").onclick = () => {
  $("settings-dialog").close();
  openDialog("pairing-dialog");
};
$("open-settings").onclick = () => openDialog("settings-dialog");
$("open-history").onclick = () => openDialog("history-dialog");
async function startPairing(reuseSignIn = false) {
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
  $("repair-pairing").disabled = true;
  try {
    const state = await request(
      reuseSignIn ? "/v1/pairing/repair" : "/v1/pairing/start",
      {
        method: "POST",
        body: "{}",
      },
    );
    if (actionGeneration !== generation) return;
    pairingState = state;
    const clearedLocalRetry =
      !!pendingSend || !!pendingConversation || pendingReactions.size > 0;
    pendingSend = undefined;
    pendingConversation = undefined;
    pendingReactions.clear();
    $("recipients").disabled = false;
    $("create-conversation").textContent = "Create";
    invalidate("thread", "compose");
    renderPairing();
    if (clearedLocalRetry)
      notice(
        "Pairing changed; review the recipient and submit each operation again.",
      );
    schedulePairingPoll();
  } catch (error) {
    if (actionGeneration !== generation) return;
    renderPairing();
    $("pairing-status").textContent = error.message;
  }
}
$("start-pairing").onclick = () => startPairing();
$("repair-pairing").onclick = () => startPairing(!pairingState.agent_enrolled);
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
for (const copy of document.querySelectorAll("[data-copy]"))
  copy.onclick = async () => {
    const input = $(copy.dataset.copy);
    try {
      await navigator.clipboard.writeText(input.value);
    } catch {
      input.select();
      document.execCommand("copy");
      input.setSelectionRange(0, 0);
    }
    $("pairing-status").textContent = "Copied.";
  };
$("new-conversation").onclick = () => {
  if (pendingSend) {
    notice("Retry or discard the pending message before starting a chat.");
    return;
  }
  stashDraft();
  composing = true;
  invalidate("thread", "newchat");
  void loadContacts();
  $("recipients").focus();
};
$("cancel-conversation").onclick = () => {
  resetCompose();
  composing = false;
  invalidate("thread");
};
$("new-conversation-form").onsubmit = (event) => {
  event.preventDefault();
  void createConversation();
};
for (const back of document.querySelectorAll("[data-back]"))
  back.onclick = () => {
    if (composing) $("cancel-conversation").onclick();
    else deselect();
  };
async function syncRecent() {
  try {
    await request("/v1/sync", { method: "POST" });
    notice("Recent-history check requested.");
  } catch (error) {
    notice(error.message);
  }
}
$("sync").onclick = syncRecent;
$("settings-sync").onclick = syncRecent;
async function retryConnection() {
  $("retry-connection").disabled = true;
  try {
    await request("/v1/connection/restart", { method: "POST" });
    notice("Connection restart requested.");
    void pollStatus();
  } catch (error) {
    notice(error.message);
  } finally {
    $("retry-connection").disabled = false;
  }
}
$("retry-connection").onclick = retryConnection;
$("import-all").onclick = async () => {
  const actionGeneration = generation;
  importing++;
  invalidate("history", "thread");
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
  invalidate("history", "thread");
};
// --- People in a conversation ----------------------------------------------

function renderAddPeople() {
  const conversation = conversations.get(peopleConversation);
  const chosen = addPicker.recipients();
  $("add-people-submit").disabled = !chosen.length;
  // Everyone already here plus everyone chosen is what the phone is asked for,
  // so the button says which of the two outcomes that is.
  const total = others(conversation || {}).length + chosen.length;
  $("add-people-submit").textContent =
    total > 1 ? "Start group with everyone" : "Start conversation";
  addPicker.render();
}
function renderPeople() {
  const conversation = conversations.get(peopleConversation);
  if (!conversation) {
    $("people-dialog").close();
    return;
  }
  $("people-title").textContent = isGroup(conversation)
    ? displayName(conversation)
    : "People";
  const list = $("people-list");
  list.replaceChildren();
  for (const person of participantList(conversation)) {
    const row = el("div", undefined, "person-row");
    const avatar = el("span", initials(person.name), "avatar");
    avatar.style.background = avatarColor(person.name);
    if (!avatar.textContent) avatar.append(icon("person"));
    row.append(avatar);
    const text = el("span", undefined, "contact-text");
    text.append(el("span", person.name, "name"));
    if (person.detail) text.append(el("span", person.detail, "detail"));
    row.append(text);
    list.append(row);
  }
  // A read-only conversation belongs to a previous pairing, and nothing can be
  // addressed from it.
  $("add-people").hidden = conversation.read_only;
  renderAddPeople();
}
function openPeople() {
  if (!selected) return;
  peopleConversation = selected;
  addPicker.clear();
  $("add-people-status").textContent = "";
  renderPeople();
  $("people-dialog").showModal();
  void loadContacts();
}
async function addPeople() {
  const conversation = conversations.get(peopleConversation);
  const recipients = addPicker.recipients();
  if (!conversation || !recipients.length) return;
  const pending = addParticipantsRequest(
    recipients,
    others(conversation).length + recipients.length > 1
      ? displayName(conversation)
      : "",
  );
  $("add-people-submit").disabled = true;
  addPicker.lock(true);
  $("add-people-status").textContent = "Requesting conversation…";
  try {
    const result = normalizeOutbox(
      await request(
        `/v1/conversations/${encodeURIComponent(peopleConversation)}/participants`,
        {
          method: "POST",
          headers: { "Idempotency-Key": pending.key },
          body: JSON.stringify(pending.body),
        },
      ),
    );
    createdConversation = {
      outboxID: result.id,
      conversationID: result.conversation_id || "",
    };
    $("add-people-status").textContent =
      "Request accepted. Waiting for your phone…";
    maybeSelectCreatedConversation();
    // The dialog carries the only copy of that status, so closing it hands the
    // same news to the toast.
    if (createdConversation) {
      $("people-dialog").close();
      notice("Request accepted. Your phone opens the conversation.");
    }
  } catch (error) {
    $("add-people-status").textContent = error.message;
  } finally {
    addPicker.lock(false);
    renderAddPeople();
  }
}
$("open-people").onclick = openPeople;
$("add-people-submit").onclick = () => void addPeople();

$("import-conversation").onclick = () =>
  void queueHistory({ conversation_id: selected }, false);
$("import-status-action").onclick = () =>
  void queueHistory({ conversation_id: selected }, false);
$("compose").onsubmit = (event) => {
  event.preventDefault();
  void sendMessage();
};
$("retry-send").onclick = () => void sendMessage();
$("discard-send").onclick = () => {
  if (sending) return;
  const uploaded = pendingSend?.uploadIDs.length || 0;
  pendingSend = undefined;
  notice(
    uploaded
      ? "Pending message discarded. Already uploaded bytes are no longer attached to a draft."
      : "Pending message discarded.",
  );
  invalidate("compose");
};
$("text").oninput = () => {
  if (selected) drafts.set(selected, $("text").value);
  autogrow();
  scheduleTyping();
};
$("text").onpaste = (event) => {
  const pasted = pastedImages(event.clipboardData);
  if (!pasted.length) return;
  event.preventDefault();
  const files = pasted.map((file, index) =>
    file.name
      ? file
      : new File([file], pastedImageName(file, index), {
          type: file.type,
          lastModified: file.lastModified,
        }),
  );
  try {
    validateAttachments([...selectedFiles, ...files]);
    selectedFiles = [...selectedFiles, ...files];
    draftFiles.set(selected, selectedFiles);
    scheduleTyping();
  } catch (error) {
    notice(error.message);
  }
  invalidate("compose");
};
$("text").onkeydown = (event) => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    void sendMessage();
  }
};
$("attachments").onchange = () => {
  const files = [...$("attachments").files];
  try {
    validateAttachments(files);
    selectedFiles = files;
    draftFiles.set(selected, selectedFiles);
    scheduleTyping();
  } catch (error) {
    $("attachments").value = "";
    selectedFiles = [];
    draftFiles.delete(selected);
    notice(error.message);
  }
  invalidate("compose");
};
async function loadOlder() {
  const id = selected;
  const entry = threads.get(id);
  if (!entry || !entry.before || entry.loading || fullHistory) return;
  const olderButton = $("load-older");
  olderButton.disabled = true;
  try {
    preserveScroll = true;
    await loadThread(id, entry.before);
  } catch (error) {
    preserveScroll = false;
    notice(error.message);
  } finally {
    olderButton.disabled = false;
  }
}
$("load-older").onclick = loadOlder;
if (typeof IntersectionObserver === "function")
  new IntersectionObserver(
    (entries) => {
      if (entries.some((entry) => entry.isIntersecting)) void loadOlder();
    },
    { root: $("messages") },
  ).observe($("older"));

// Walks every remaining page of stored history in one go and lands on the
// oldest message. Pages render only once at the end, because relaying the
// thread after each page is what makes long conversations crawl.
const FULL_HISTORY_PAGE = 500;
function stopFullHistory() {
  if (fullHistory) fullHistory.cancelled = true;
}
async function loadFullHistory() {
  const id = selected;
  const entry = id && threads.get(id);
  if (!entry || fullHistory) return;
  if (!entry.before) {
    $("messages").scrollTop = 0;
    return;
  }
  const run = (fullHistory = { id, cancelled: false });
  const stale = () =>
    run !== fullHistory || selected !== id || threads.get(id) !== entry;
  try {
    showFullHistoryProgress(entry.messages.size);
    await walkOlder(
      async (before) => {
        await loadThread(id, before, { limit: FULL_HISTORY_PAGE, quiet: true });
        return stale() ? before : entry.before;
      },
      {
        before: entry.before,
        cancelled: () => run.cancelled || stale(),
        onPage: () => showFullHistoryProgress(entry.messages.size),
      },
    );
    if (stale()) return;
    // Stopping partway keeps the viewport where it was; reaching the start is
    // the whole point of the button, so land there.
    if (entry.before) {
      notice(`Stopped after loading ${entry.messages.size} messages.`);
      preserveScroll = true;
    } else {
      notice(
        `Loaded ${entry.messages.size} messages, back to the start of the conversation.`,
      );
      entry.scrollToTop = true;
    }
  } catch (error) {
    notice(error.message);
  } finally {
    if (run === fullHistory) fullHistory = undefined;
    // One relayout for every page at once: no entrance animation per row.
    bulkRows = true;
    invalidate("thread", "compose");
  }
}
// Written straight to the DOM: the batched renderer does not run during a
// walk, because the point is to relayout the thread only once at the end.
function showFullHistoryProgress(loaded) {
  $("older").hidden = false;
  $("load-older").hidden = true;
  $("older-progress").hidden = false;
  $("load-full-history").disabled = true;
  $("older-progress-text").textContent = loaded
    ? `Loading older messages… ${loaded} so far`
    : "Loading older messages…";
}
$("load-full-history").onclick = () => void loadFullHistory();
$("stop-full-history").onclick = stopFullHistory;

// --- Read receipts ----------------------------------------------------------

// Only a message from the live session can carry a read receipt: records held
// over from a previous session are no longer current and the phone rejects
// them.
function newestReadable(id) {
  return [...(threads.get(id)?.messages.values() || [])]
    .filter((message) => !message.read_only)
    .sort((a, b) => (b.time || "").localeCompare(a.time || ""))[0];
}
async function sendReadReceipt(id, messageID) {
  await request(`/v1/conversations/${encodeURIComponent(id)}/read`, {
    method: "POST",
    body: JSON.stringify({ message_id: messageID }),
  });
}

// A conversation on screen has been seen, so it clears its own badge instead of
// waiting for an explicit mark-read. The attempted message is remembered so a
// receipt the phone never accepts is not retried on every event; selecting the
// conversation again asks once more.
const receiptAttempts = new Map();
let receiptTimer;
function scheduleReadReceipt() {
  clearTimeout(receiptTimer);
  receiptTimer = setTimeout(() => void markSelectedRead(), 250);
}
async function markSelectedRead() {
  const id = selected;
  const conversation = conversations.get(id);
  if (
    !token ||
    !id ||
    document.hidden ||
    !$("auto-mark-read").checked ||
    providerState !== "connected" ||
    !conversation?.unread ||
    conversation.read_only
  )
    return;
  const latest = newestReadable(id);
  if (!latest || receiptAttempts.get(id) === latest.id) return;
  receiptAttempts.set(id, latest.id);
  try {
    await sendReadReceipt(id, latest.id);
  } catch {
    // Silent: an unsolicited receipt is not worth a notice, and the next
    // message or the next visit tries again.
  }
}
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) scheduleReadReceipt();
});
$("mark-read").onclick = async () => {
  const latest = newestReadable(selected);
  if (!latest) return;
  receiptAttempts.set(selected, latest.id);
  try {
    await sendReadReceipt(selected, latest.id);
    notice("Read receipt requested.");
  } catch (error) {
    notice(error.message);
  }
};
let typingShown = false;
setInterval(() => {
  const wanted =
    !!selected && typingUntil > Date.now() && typingConversation === selected;
  if (wanted !== typingShown) {
    typingShown = wanted;
    $("typing").hidden = !wanted;
    if (wanted) {
      const container = $("messages");
      if (
        container.scrollHeight - container.scrollTop - container.clientHeight <
        120
      )
        container.scrollTop = container.scrollHeight;
    }
  }
}, 500);
setInterval(() => void pollStatus(), 10000);

// The service worker backs the installed app: an offline shell and, once a
// subscription exists, notifications delivered while no window is open. The
// desktop app already ships the shell and notifies through the OS.
if (!desktop && "serviceWorker" in navigator) {
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
// --- Desktop bootstrap ------------------------------------------------------

// A browser loaded this client from the bridge it talks to. The desktop app
// bundles the client and starts with no bridge at all, so it has to ask for one
// before the token screen means anything.
function showConnect(prefill = "") {
  $("connect-url").value = prefill;
  $("connect-error").textContent = "";
  $("app").hidden = true;
  $("login").hidden = true;
  $("connect").hidden = false;
  $("connect-url").focus();
}

async function unlockWithSavedToken() {
  const saved = await desktop("get_token").catch(() => null);
  if (!saved || token) return;
  $("token").value = saved;
  $("login-form").requestSubmit();
}

if (desktop) {
  $("connect-form").onsubmit = async (event) => {
    event.preventDefault();
    $("connect-error").textContent = "";
    const url = normalizeBase($("connect-url").value);
    try {
      await desktop("save_bridge_url", { url });
    } catch (message) {
      $("connect-error").textContent = String(message);
      return;
    }
    apiBase = url;
    $("connect").hidden = true;
    $("login").hidden = false;
    await unlockWithSavedToken();
  };
  // Hidden until the bridge is known, so the token screen never asks for a
  // token against nothing.
  $("login").hidden = true;
  desktop("get_bridge_url")
    .then(async (saved) => {
      if (!saved) return showConnect();
      apiBase = normalizeBase(saved);
      $("login").hidden = false;
      await unlockWithSavedToken();
    })
    .catch(() => showConnect());
  // The tray's "Change bridge URL…" item.
  void window.__TAURI__?.event
    ?.listen("show-connect", () => showConnect(apiBase))
    .catch(() => {});
}

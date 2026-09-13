// Presentation helpers with no DOM access so the thread layout, list labels,
// and time formatting can be unit tested.

// Google repeats a participant record per SIM or per self-chat leg, and the
// extra copies of the owner's own number are not flagged as self.
export function others(conversation) {
  const participants = conversation.participants || [];
  const self = new Set(
    participants
      .filter((participant) => participant.is_me && participant.address)
      .map((participant) => participant.address),
  );
  const seen = new Set();
  return participants.filter((participant) => {
    const key = participant.address || participant.name || participant.id;
    if (participant.is_me || self.has(participant.address) || seen.has(key))
      return false;
    seen.add(key);
    return true;
  });
}

export function displayName(conversation) {
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

export function isGroup(conversation) {
  return others(conversation).length > 1;
}

export function initials(name) {
  const word = (name || "").trim().split(/\s+/)[0] || "";
  const first = [...word][0] || "";
  return /\p{L}/u.test(first) ? first.toUpperCase() : "";
}

const avatarPalette = [
  "#1a73e8",
  "#d93025",
  "#188038",
  "#e37400",
  "#9334e6",
  "#e52592",
  "#007b83",
  "#b06000",
  "#5f6368",
  "#0b8043",
];

export function avatarColor(seed) {
  let hash = 0;
  for (const char of String(seed || ""))
    hash = (hash * 31 + char.codePointAt(0)) >>> 0;
  return avatarPalette[hash % avatarPalette.length];
}

const sameDay = (a, b) =>
  a.getFullYear() === b.getFullYear() &&
  a.getMonth() === b.getMonth() &&
  a.getDate() === b.getDate();

const dayDistance = (date, now) => {
  const start = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
  return Math.round((start(now) - start(date)) / 86400000);
};

export function clockTime(date) {
  return date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}

// Conversation list stamp: time today, weekday this week, otherwise a date.
export function listTime(value, now = new Date()) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  if (sameDay(date, now)) return clockTime(date);
  const distance = dayDistance(date, now);
  if (distance > 0 && distance < 7)
    return date.toLocaleDateString([], { weekday: "short" });
  if (date.getFullYear() === now.getFullYear())
    return date.toLocaleDateString([], { month: "short", day: "numeric" });
  return date.toLocaleDateString([], {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

export function dayLabel(date, now = new Date()) {
  if (sameDay(date, now)) return "Today";
  const distance = dayDistance(date, now);
  if (distance === 1) return "Yesterday";
  if (distance > 0 && distance < 7)
    return date.toLocaleDateString([], { weekday: "long" });
  if (date.getFullYear() === now.getFullYear())
    return date.toLocaleDateString([], {
      weekday: "short",
      month: "short",
      day: "numeric",
    });
  return date.toLocaleDateString([], {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

export function dividerLabel(date, now = new Date()) {
  return `${dayLabel(date, now)} • ${clockTime(date)}`;
}

export function statusLabel(status) {
  const value = (status || "").toLowerCase();
  if (!value.startsWith("outgoing")) return "";
  if (/fail|not_delivered|error|cancel/.test(value)) return "Not sent";
  if (/displayed|read/.test(value)) return "Read";
  if (/deliver/.test(value)) return "Delivered";
  if (/send|pending|queue|draft/.test(value)) return "Sending…";
  return "Sent";
}

export function outboxLabel(item) {
  switch (item.state) {
    case "queued":
      return "Waiting for phone…";
    case "sending":
      return "Sending…";
    case "confirmed":
      return "Sent";
    default:
      return `Not sent${item.detail ? ` · ${item.detail}` : ""}`;
  }
}

export function outboxText(item) {
  const body = item.request || {};
  if (body.kind === "reaction")
    return `${body.remove ? "Removed" : "Reacted"} ${body.emoji}`;
  if (body.kind === "conversation")
    return `New conversation with ${(body.recipients || []).join(", ")}`;
  return body.text || "";
}

export function outboxAttachmentCount(item) {
  return item.request?.attachment_ids?.length || 0;
}

const messageTime = (m) => new Date(m.time || 0).getTime() || 0;

export function sortMessages(messages) {
  return [...messages].sort(
    (a, b) =>
      messageTime(a) - messageTime(b) ||
      (a.time || "").localeCompare(b.time || "") ||
      a.id.localeCompare(b.id),
  );
}

export function sortConversations(conversations) {
  return [...conversations].sort(
    (a, b) =>
      (b.updated || "").localeCompare(a.updated || "") ||
      b.id.localeCompare(a.id),
  );
}

export function filterConversations(conversations, query) {
  const needle = (query || "").trim().toLowerCase();
  if (!needle) return conversations;
  return conversations.filter((conversation) => {
    if (displayName(conversation).toLowerCase().includes(needle)) return true;
    return others(conversation).some((participant) =>
      (participant.address || "").toLowerCase().includes(needle),
    );
  });
}

// A conversation-kind outbox row only matters to the new chat flow, and a
// confirmed row is redundant once its message is stored.
export function threadOutbox(outbox, conversationID, messageIDs) {
  return outbox.filter((item) => {
    const body = item.request || {};
    if (body.kind === "conversation") return false;
    if ((body.conversation_id || item.conversation_id) !== conversationID)
      return false;
    return item.state !== "confirmed" || !messageIDs.has(item.message_id);
  });
}

const GROUP_GAP = 10 * 60 * 1000;

// Rows for the thread: time dividers, messages, and pending outbox items, with
// grouping flags for bubble shapes and sender labels.
export function layoutThread(messages, outbox, options = {}) {
  const now = options.now || new Date();
  const items = [
    ...sortMessages(messages).map((message) => ({
      kind: "message",
      key: `m:${message.id}`,
      time: messageTime(message),
      sender:
        message.direction === "outgoing"
          ? "me"
          : message.direction === "system"
            ? "system"
            : `p:${message.sender_id || ""}`,
      direction: message.direction,
      message,
    })),
    ...outbox.map((item) => ({
      kind: "outbox",
      key: `o:${item.id}`,
      time: new Date(item.created || 0).getTime() || 0,
      sender: "me",
      direction: "outgoing",
      item,
    })),
  ].sort((a, b) => a.time - b.time || a.key.localeCompare(b.key));
  const rows = [];
  let previous;
  for (const item of items) {
    const date = new Date(item.time);
    if (
      !previous ||
      item.time - previous.time > GROUP_GAP ||
      !sameDay(date, new Date(previous.time))
    )
      rows.push({
        kind: "divider",
        key: `d:${item.key}`,
        label: dividerLabel(date, now),
      });
    rows.push(item);
    previous = item;
  }
  for (let i = 0; i < rows.length; i++) {
    const row = rows[i];
    if (row.kind === "divider") continue;
    const before = rows[i - 1];
    const after = rows[i + 1];
    row.first =
      !before || before.kind === "divider" || before.sender !== row.sender;
    row.last =
      !after || after.kind === "divider" || after.sender !== row.sender;
  }
  return rows;
}

const URL_PATTERN = /\b(?:https?:\/\/|www\.)[^\s<>"']+[^\s<>"'.,;:!?)\]]/gi;

export function linkify(text) {
  const segments = [];
  let index = 0;
  for (const match of (text || "").matchAll(URL_PATTERN)) {
    if (match.index > index)
      segments.push({ text: text.slice(index, match.index) });
    const url = match[0];
    segments.push({
      text: url,
      href: url.startsWith("www.") ? `https://${url}` : url,
    });
    index = match.index + url.length;
  }
  if (index < (text || "").length) segments.push({ text: text.slice(index) });
  return segments;
}

export function formatSize(size) {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${Math.ceil(size / 1024)} KB`;
  return `${(size / (1024 * 1024)).toFixed(1)} MB`;
}

export function summarizeHistory(jobs) {
  const summary = { total: 0, active: 0, complete: 0, failed: 0, paused: 0 };
  for (const job of jobs) {
    summary.total++;
    if (job.state === "complete") summary.complete++;
    else if (job.state === "failed") summary.failed++;
    else if (job.state === "paused") summary.paused++;
    else summary.active++;
  }
  return summary;
}

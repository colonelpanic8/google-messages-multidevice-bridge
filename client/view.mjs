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

// Conversation rows name the author of their newest message the way the phone
// does: "You" for your own, a first name in a group, nothing in a one-to-one
// thread where the only other author is the row's own title.
export function previewSender(conversation) {
  const direction = conversation.preview_direction || "";
  if (direction === "outgoing") return "You";
  if (direction !== "incoming" || !isGroup(conversation)) return "";
  const sender = others(conversation).find(
    (participant) => participant.id === (conversation.preview_sender_id || ""),
  );
  if (!sender) return "";
  const name = sender.name || sender.address || "";
  return name.trim().split(/\s+/)[0] || "";
}

export function previewLine(conversation) {
  const text = conversation.preview || "";
  const sender = text ? previewSender(conversation) : "";
  return sender ? `${sender}: ${text}` : text;
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
  "#0b57d0",
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

// Anything you send walks one progression — Sending… → Sent → Delivered → Read
// — shared by the outbox row and the stored message that takes its place, so
// the handoff never reads as a step backwards and only a real failure is an
// error.
const NO_STATUS = { label: "", error: false };
const SENDING = { label: "Sending…", error: false };
const SENT = { label: "Sent", error: false };
const CANCELED = { label: "Canceled", error: false };

export function messageStatus(status) {
  const value = (status || "").toLowerCase();
  if (!value.startsWith("outgoing")) return NO_STATUS;
  if (/cancel/.test(value)) return CANCELED;
  if (/fail|restricted|error/.test(value))
    return { label: "Not sent", error: true };
  if (/displayed|read/.test(value)) return { label: "Read", error: false };
  // Not delivered *yet* is Google still working, not a message that failed.
  if (/not_delivered/.test(value)) return SENT;
  if (/deliver/.test(value)) return { label: "Delivered", error: false };
  if (/send|pending|queue|draft|validating|retry|scheduled/.test(value))
    return SENDING;
  return SENT;
}

// Delivery is the message's story to tell, so a row still in the outbox says
// no more than that the send is under way — including once Google has accepted
// it, which confirms nothing the recipient would notice.
export function outboxStatus(item, connected = true) {
  switch (item.state) {
    case "queued":
      return connected
        ? SENDING
        : { label: "Waiting for phone…", error: false };
    case "sending":
    case "accepted":
    case "confirmed":
      return SENDING;
    case "canceled":
      return CANCELED;
    default:
      return {
        label: `Not sent${item.detail ? ` · ${item.detail}` : ""}`,
        error: true,
      };
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

// Google repeats the owner across SIM legs, so every self participant id counts
// as "you" when attributing a reaction.
function selfParticipantIDs(conversation) {
  const participants = conversation?.participants || [];
  const addresses = new Set(
    participants
      .filter((participant) => participant.is_me && participant.address)
      .map((participant) => participant.address),
  );
  return new Set(
    participants
      .filter(
        (participant) =>
          participant.is_me ||
          (participant.address && addresses.has(participant.address)),
      )
      .map((participant) => participant.id),
  );
}

export function reactedByMe(reaction, conversation) {
  const self = selfParticipantIDs(conversation);
  return (reaction?.participants || []).some((id) => self.has(id));
}

// Names behind a reaction chip, with the owner first and labelled "You".
export function reactionNames(reaction, conversation) {
  const self = selfParticipantIDs(conversation);
  const byID = new Map(
    (conversation?.participants || []).map((participant) => [
      participant.id,
      participant,
    ]),
  );
  const names = [];
  let mine = false;
  for (const id of reaction?.participants || []) {
    if (self.has(id)) {
      mine = true;
      continue;
    }
    const participant = byID.get(id);
    const name = participant?.name || participant?.address || "Someone";
    if (!names.includes(name)) names.push(name);
  }
  return mine ? ["You", ...names] : names;
}

export function formatNameList(names) {
  if (names.length < 3) return names.join(" and ");
  return `${names.slice(0, -1).join(", ")}, and ${names.at(-1)}`;
}

// Tooltip for a reaction chip: who reacted, and with what.
export function reactionTitle(reaction, conversation) {
  const names = reactionNames(reaction, conversation);
  const emoji = reaction?.emoji || "";
  if (!names.length) return `Reacted ${emoji}`.trim();
  return `${formatNameList(names)} reacted ${emoji}`.trim();
}

// The worker takes eligible queued jobs oldest-updated first, so a job's place
// in that order is how many other imports run before this one.
export function queuePosition(jobs, id) {
  return [...jobs]
    .filter((job) => job.state === "queued")
    .sort((a, b) => (a.updated || "").localeCompare(b.updated || ""))
    .findIndex((job) => job.id === id);
}

// What to say above the oldest stored message. A thread whose import has not
// finished is not a complete history, and silently looking complete is what
// makes a half-imported thread read as a broken one. Null once the import is
// done and the thread can be trusted to be whole.
export function importStatus(job, { ahead = 0, connected = true } = {}) {
  if (!job)
    return {
      state: "idle",
      label: "Older messages have not been imported from this conversation.",
      action: "Import from phone",
    };
  if (job.state === "complete") return null;
  if (job.state === "failed")
    return {
      state: "failed",
      label: job.detail || "Import stopped.",
      action: "Try again",
    };
  if (job.state === "paused")
    return { state: "paused", label: "Import paused.", action: "Resume" };
  const progress = job.records ? ` · ${job.records} imported` : "";
  if (job.detail) return { state: "waiting", label: job.detail + progress };
  if (!connected)
    return {
      state: "waiting",
      label: `Older messages import when the phone reconnects${progress}`,
    };
  if (ahead > 0)
    return {
      state: "waiting",
      label: `Waiting to import older messages · ${ahead} ${
        ahead === 1 ? "conversation" : "conversations"
      } ahead${progress}`,
    };
  return {
    state: "active",
    label: `Importing older messages from your phone…${progress}`,
  };
}

// --- Contacts and participants ---------------------------------------------

// The phone reports a number the way it dials or displays it, so a query typed
// with punctuation still has to reach the contact behind it.
const digitsOf = (value) => (value || "").replace(/\D/g, "");

// A contact is identified by the number it would address, because the address
// book repeats one person per number and per SIM.
export function contactKey(contact) {
  return (
    contact?.address ||
    `${contact?.id || ""}:${contact?.formatted || contact?.name || ""}`
  );
}

export function contactName(contact) {
  return contact?.name || contact?.formatted || contact?.address || "Unknown";
}

// The second line of a picker row, empty when the first line is already the
// number.
export function contactDetail(contact) {
  const number = contact?.formatted || contact?.address || "";
  return contact?.name ? number : "";
}

// A number typed in full is a recipient in its own right, so someone who is not
// in the address book is still reachable.
export function typedRecipient(value) {
  const text = (value || "").trim();
  if (!/^\+[1-9]\d{6,14}$/.test(text)) return null;
  return { id: "", name: "", address: text, formatted: text };
}

// Contacts to offer for what has been typed so far. An empty query offers the
// address book as the bridge ordered it, which puts frequent contacts first.
export function matchContacts(
  contacts,
  query,
  { exclude = [], limit = 8 } = {},
) {
  const taken = new Set(exclude.map(contactKey));
  const available = (contacts || []).filter(
    (contact) => contact.address && !taken.has(contactKey(contact)),
  );
  const needle = (query || "").trim().toLowerCase();
  if (!needle) return available.slice(0, limit);
  const needleDigits = digitsOf(query);
  const ranked = [];
  for (const contact of available) {
    const name = (contact.name || "").toLowerCase();
    const number = digitsOf(contact.address || contact.formatted);
    let rank = -1;
    if (name.startsWith(needle)) rank = 0;
    else if (name.split(/\s+/).some((word) => word.startsWith(needle)))
      rank = 1;
    else if (needleDigits && number.startsWith(needleDigits)) rank = 2;
    else if (name.includes(needle)) rank = 3;
    else if (needleDigits && number.includes(needleDigits)) rank = 4;
    if (rank >= 0) ranked.push({ rank, contact });
  }
  return ranked
    .sort((a, b) => a.rank - b.rank)
    .slice(0, limit)
    .map((entry) => entry.contact);
}

// Everyone in a conversation, with the owner last and named as themselves.
export function participantList(conversation) {
  const people = others(conversation).map((participant) => ({
    id: participant.id,
    name: participant.name || participant.address || participant.id,
    detail: participant.name ? participant.address || "" : "",
    isMe: false,
  }));
  const me = (conversation?.participants || []).find(
    (participant) => participant.is_me,
  );
  if (me)
    people.push({
      id: me.id,
      name: "You",
      detail: me.address || "",
      isMe: true,
    });
  return people;
}

// What the compose view says under the recipient field. Naming a group is only
// offered once there is a group to name.
export function composeHint(recipients, contactBook) {
  if (recipients.length > 1)
    return "Your phone creates the group. A name is only kept if it creates an RCS group.";
  if (contactBook?.stale)
    return "Showing contacts from the last time the phone was reachable.";
  if (!contactBook?.contacts?.length)
    return "Type international numbers such as +14155550100.";
  return "Pick contacts, or type an international number such as +14155550100.";
}

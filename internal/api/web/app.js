import { createParser, newRequest, mergeMessages } from "/stream.mjs";
const $ = (id) => document.getElementById(id);
let token = "",
  abort,
  selected = "",
  conversations = [],
  outbox = [],
  messages = [],
  before = "";
let cursor = 0,
  targetCursor = 0,
  refreshTask,
  pending,
  typingUntil = 0,
  typingConversation = "";
let generation = 0,
  sending = false;
const drafts = new Map();
const messageUpdates = new Map();
const notice = (message) => {
  $("notice").textContent = message;
};
function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}
async function request(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    signal: abort.signal,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(options.body ? { "Content-Type": "application/json" } : {}),
      ...options.headers,
    },
  });
  if (!response.ok) {
    const error = new Error(
      response.status === 401
        ? "Token rejected. Lock and unlock with your bridge token."
        : (await response.text()).trim(),
    );
    error.status = response.status;
    throw error;
  }
  return response.status === 204 ? null : response.json();
}
function name(c) {
  return (
    c.name ||
    c.participants
      ?.filter((p) => !p.is_me)
      .map((p) => p.name || p.address || p.id)
      .join(", ") ||
    c.id
  );
}
function renderConversations() {
  const search = $("search").value.toLowerCase();
  $("conversations").replaceChildren();
  for (const c of conversations.filter((c) =>
    name(c).toLowerCase().includes(search),
  )) {
    const button = el(
      "button",
      undefined,
      `conversation ${c.id === selected ? "active" : ""} ${c.unread ? "unread" : ""}`,
    );
    button.append(
      el("strong", name(c)),
      el("small", c.preview || "No recent preview"),
    );
    button.onclick = () => select(c.id);
    $("conversations").append(button);
  }
  if (!conversations.length)
    $("conversations").append(el("p", "No conversations stored yet.", "hint"));
}
function renderThread() {
  const c = conversations.find((c) => c.id === selected);
  $("empty").hidden = !!c;
  $("thread").hidden = !c;
  if (!c) return;
  $("thread-title").textContent = name(c);
  $("thread-info").textContent = `${c.protocol.toUpperCase()} · ${
    c.participants
      ?.filter((p) => !p.is_me)
      .map((p) => p.address || p.name)
      .join(", ") || "Phone conversation"
  }`;
  const container = $("messages");
  const bottom =
    container.scrollHeight - container.scrollTop - container.clientHeight < 70;
  container.replaceChildren();
  for (const m of [...messages].sort(
    (a, b) => a.time.localeCompare(b.time) || a.id.localeCompare(b.id),
  )) {
    const node = el("article", undefined, `message ${m.direction}`);
    const sender = c.participants?.find((p) => p.id === m.sender_id);
    if (sender && !sender.is_me)
      node.append(el("div", sender.name || sender.address, "sender"));
    if (m.subject) node.append(el("strong", m.subject));
    node.append(
      el(
        "p",
        m.deleted
          ? "Message deleted on phone"
          : m.text ||
              (m.attachments?.length ? "" : "Message content unavailable"),
      ),
    );
    for (const a of m.deleted ? [] : m.attachments || []) {
      const button = el(
        "button",
        `${a.name || "Attachment"} · ${Math.ceil(a.size / 1024)} KB${a.available ? " · Download" : " · Unavailable"}`,
        "attachment",
      );
      button.disabled = !a.available;
      button.onclick = async () => {
        button.disabled = true;
        try {
          const response = await fetch(
            `/v1/attachments/${encodeURIComponent(a.id)}`,
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
          link.download = a.name || "attachment";
          link.click();
          setTimeout(() => URL.revokeObjectURL(url), 60000);
        } catch (error) {
          notice(error.message);
        } finally {
          button.disabled = false;
        }
      };
      node.append(button);
    }
    for (const r of m.reactions || [])
      node.append(
        el("span", `${r.emoji} ${r.participants?.length || 0}`, "reaction"),
      );
    node.append(
      el(
        "div",
        `${new Date(m.time).toLocaleString()} · ${m.status.replaceAll("_", " ")}`,
        "meta",
      ),
    );
    container.append(node);
  }
  if (!messages.length)
    container.append(el("p", "No messages in stored history yet.", "hint"));
  if (bottom) container.scrollTop = container.scrollHeight;
  $("older").hidden = !before;
  $("text").disabled = c.read_only || !!pending;
  $("send").disabled = c.read_only || sending;
  $("send").textContent = pending ? "Retry same request" : "Send message";
  $("mark-read").disabled = !messages.length;
  $("outbox").replaceChildren();
  for (const o of outbox
    .filter(
      (o) =>
        o.request.conversation_id === selected &&
        (o.state !== "confirmed" ||
          !messages.some((m) => m.id === o.message_id)),
    )
    .sort((a, b) => a.created.localeCompare(b.created))) {
    const node = el("div", undefined, `outbox-item ${o.state}`);
    node.append(
      el("strong", o.state === "confirmed" ? "Observed on phone" : o.state),
      el("p", o.request.text),
      el(
        "div",
        o.detail ||
          (o.state === "queued"
            ? "Waiting for phone connection. You can cancel before sending starts."
            : ""),
        "hint",
      ),
    );
    if (o.state === "queued") {
      const cancel = el("button", "Cancel queued message");
      cancel.onclick = async () => {
        try {
          await request(`/v1/outbox/${encodeURIComponent(o.id)}/cancel`, {
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
async function select(id) {
  if (pending && id !== selected) {
    notice(
      "Resolve the pending request using Retry same request before switching conversations.",
    );
    return;
  }
  if (selected) drafts.set(selected, $("text").value);
  selected = id;
  messageUpdates.clear();
  messages = [];
  before = "";
  $("text").value = drafts.get(id) || "";
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
    const target = targetCursor,
      id = selected;
    const [status, cs, os, ms] = await Promise.all([
      request("/v1/status"),
      request("/v1/conversations"),
      request("/v1/outbox"),
      id
        ? request(
            `/v1/conversations/${encodeURIComponent(id)}/messages?limit=100`,
          )
        : null,
    ]);
    if (currentGeneration !== generation) return;
    conversations = cs.conversations;
    outbox = os.outbox;
    $("status").textContent = status.state.replaceAll("_", " ");
    $("provider-detail").textContent = status.detail || "";
    $("sync-status").textContent = status.last_sync
      ? `Recent history checked ${new Date(status.last_sync).toLocaleTimeString()}. ${status.sync_state === "failed" ? "Latest check failed." : ""}`
      : "Recent history has not been reconciled yet.";
    $("send-hint").textContent =
      status.state === "connected"
        ? "Your phone handles delivery."
        : status.state === "authentication_required"
          ? "Pair the bridge before queued messages can send."
          : status.state === "connection_failed"
            ? "Restart the bridge connection before queued messages can send."
            : "Offline: sending queues until your phone connects.";
    if (id === selected && ms) {
      const expanded = messages.length > 100;
      messages = mergeMessages(
        messages,
        ms.messages,
        ms.cursor,
        messageUpdates,
      );
      if (!expanded) before = ms.next_before;
    }
    if (!cursor) cursor = Math.min(cs.cursor, os.cursor);
    cursor = Math.max(cursor, target);
    renderConversations();
    renderThread();
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
      const parse = createParser((event) => {
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
  pending = undefined;
  cursor = targetCursor = 0;
  selected = "";
  conversations = outbox = messages = [];
  drafts.clear();
  messageUpdates.clear();
  sending = false;
  $("text").value = "";
  $("app").hidden = true;
  $("login").hidden = false;
  $("logout").hidden = true;
  $("status").textContent = "Locked";
  $("messages").replaceChildren();
  $("outbox").replaceChildren();
  renderConversations();
  renderThread();
};
$("search").oninput = renderConversations;
$("sync").onclick = async () => {
  try {
    await request("/v1/sync", { method: "POST" });
    notice("Recent-history check requested.");
  } catch (error) {
    notice(error.message);
  }
};
$("compose").onsubmit = async (event) => {
  event.preventDefault();
  if (sending) return;
  sending = true;
  const sendGeneration = generation;
  if (!pending) pending = newRequest(selected, $("text").value);
  const send = pending;
  $("send").disabled = true;
  $("text").disabled = true;
  try {
    await request("/v1/messages", {
      method: "POST",
      headers: { "Idempotency-Key": send.key },
      body: JSON.stringify(send.body),
    });
    if (sendGeneration !== generation) return;
    pending = undefined;
    $("text").value = "";
    drafts.delete(selected);
    notice("");
    await refresh();
  } catch (error) {
    if (sendGeneration !== generation) return;
    if ([400, 404, 415].includes(error.status)) {
      pending = undefined;
      notice(`${error.message}. Edit your message and try again.`);
    } else
      notice(
        `${error.message}. Retry same request preserves its idempotency key.`,
      );
  } finally {
    if (sendGeneration === generation) {
      sending = false;
      renderThread();
    }
  }
};
$("older").onclick = async () => {
  const id = selected;
  try {
    const page = await request(
      `/v1/conversations/${encodeURIComponent(id)}/messages?before=${encodeURIComponent(before)}&limit=100`,
    );
    if (selected !== id) return;
    messages = mergeMessages(
      messages,
      page.messages,
      page.cursor,
      messageUpdates,
    );
    before = page.next_before;
    renderThread();
  } catch (error) {
    notice(error.message);
  }
};
$("mark-read").onclick = async () => {
  const latest = [...messages].sort((a, b) => b.time.localeCompare(a.time))[0];
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

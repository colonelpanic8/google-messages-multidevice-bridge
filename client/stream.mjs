// SSE frames can span arbitrary UTF-8 network chunks; unfinished frames wait.
export function createParser(onEvent) {
  let pending = "";
  return (chunk) => {
    pending += chunk;
    pending = pending.replace(/\r\n/g, "\n");
    let end;
    while ((end = pending.indexOf("\n\n")) >= 0) {
      const frame = pending.slice(0, end);
      pending = pending.slice(end + 2);
      const data = [];
      for (const line of frame.split("\n")) {
        if (line.startsWith("data:"))
          data.push(line.slice(5).replace(/^ /, ""));
      }
      if (data.length) onEvent(JSON.parse(data.join("\n")));
    }
    if (pending.length > 2 ** 20) throw new Error("Stream frame too large");
  };
}
export function idempotencyKey() {
  const key = Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
  return key;
}

export function newRequest(conversationID, text, attachmentIDs = []) {
  return {
    key: idempotencyKey(),
    body: {
      conversation_id: conversationID,
      text,
      attachment_ids: [...attachmentIDs],
    },
  };
}

// Media and text never share one send; a caption follows as its own message.
export function splitRequests(conversationID, text, attachmentIDs = []) {
  const requests = [];
  if (attachmentIDs.length)
    requests.push(newRequest(conversationID, "", attachmentIDs));
  if (text.trim()) requests.push(newRequest(conversationID, text));
  return requests;
}

export function newConversationRequest(recipients, name = "") {
  return {
    key: idempotencyKey(),
    body: { recipients: [...recipients], name: name.trim() },
  };
}

// Google Messages cannot change who is in a conversation, so the bridge
// addresses one to everyone instead and the phone decides whether that is the
// thread already open or a new group.
export function addParticipantsRequest(recipients, name = "") {
  return {
    key: idempotencyKey(),
    body: { recipients: [...recipients], name: name.trim() },
  };
}

export function newReactionRequest(messageID, emoji, remove = false) {
  return {
    key: idempotencyKey(),
    body: { message_id: messageID, emoji, remove },
  };
}

export function validateAttachments(files) {
  if (files.length > 10) throw new Error("Choose no more than 10 attachments.");
  const size = files.reduce((total, file) => total + file.size, 0);
  if (size > 20 * 1024 * 1024)
    throw new Error("Attachments must total 20 MiB or less.");
  return size;
}

// Preserve fetched pages while applying live updates newer than a snapshot.
export function mergeMessages(existing, page, cursor, updates) {
  const records = new Map(existing.map((m) => [m.id, m]));
  const fetched = new Set(page.map((m) => m.id));
  for (const m of page) records.set(m.id, m);
  for (const event of updates.values()) {
    if (!fetched.has(event.entity_id) || event.id > cursor)
      records.set(event.entity_id, event.data);
    else updates.delete(event.entity_id);
  }
  return [...records.values()];
}

// Pages backwards until there is nothing older, `cancelled` says to stop, or a
// page hands back the cursor it was given, which would otherwise spin forever.
export async function walkOlder(
  fetchPage,
  { before, cancelled = () => false, onPage } = {},
) {
  let cursor = before || "";
  while (cursor && !cancelled()) {
    const next = (await fetchPage(cursor)) || "";
    if (next === cursor) break;
    cursor = next;
    onPage?.(cursor);
  }
  return cursor;
}

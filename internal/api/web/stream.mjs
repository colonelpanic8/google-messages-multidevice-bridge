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

export function newConversationRequest(recipients) {
  return {
    key: idempotencyKey(),
    body: { recipients: [...recipients] },
  };
}

export function newReactionRequest(messageID, emoji, remove = false) {
  return {
    key: idempotencyKey(),
    body: { message_id: messageID, emoji, remove },
  };
}

export function parseRecipients(value) {
  const recipients = value
    .split(/[\s,;]+/)
    .map((recipient) => recipient.trim())
    .filter(Boolean);
  if (
    !recipients.length ||
    recipients.some((recipient) => !/^\+[1-9]\d{1,14}$/.test(recipient))
  )
    throw new Error("Use international phone numbers such as +14155550100.");
  return [...new Set(recipients)];
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

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
export function newRequest(conversationID, text) {
  const key = Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
  return { key, body: { conversation_id: conversationID, text } };
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

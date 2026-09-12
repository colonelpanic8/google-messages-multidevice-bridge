import test from "node:test";
import assert from "node:assert/strict";
import { createParser } from "./stream.mjs";

test("SSE parser handles split frames, comments, and multiple events", () => {
  const events = [],
    parse = createParser((e) => events.push(e));
  const wire =
    ': keepalive\r\n\r\nid: 1\r\ndata: {"id":1,"text":"hello 🌏"}\r\n\r\nevent: typing\r\ndata: {"type":"typing"}\r\n\r\n';
  for (const char of wire) parse(char);
  assert.deepEqual(events, [{ id: 1, text: "hello 🌏" }, { type: "typing" }]);
});
test("unfinished frames are never applied", () => {
  const events = [],
    parse = createParser((e) => events.push(e));
  parse('data: {"id":2}\n');
  assert.equal(events.length, 0);
  parse("\n");
  assert.deepEqual(events, [{ id: 2 }]);
});

test("live receipt updates survive stale snapshots and refreshes of newer pages", async () => {
  const { mergeMessages } = await import("./stream.mjs");
  const old = { id: "old", status: "sent" };
  const recent = { id: "recent", status: "sent" };
  const receipt = { ...old, status: "read" };
  const updates = new Map([
    ["old", { id: 9, entity_id: "old", data: receipt }],
  ]);
  assert.deepEqual(mergeMessages([old], [recent], 10, updates), [
    receipt,
    recent,
  ]);
  assert.deepEqual(mergeMessages([], [old], 8, updates), [receipt]);
  const newer = { ...old, status: "deleted" };
  assert.deepEqual(mergeMessages([old], [newer], 10, updates), [newer]);
  assert.deepEqual(mergeMessages([newer], [recent], 11, updates), [
    newer,
    recent,
  ]);
});

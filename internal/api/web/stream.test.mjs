import test from "node:test";
import assert from "node:assert/strict";
import {
  splitRequests,
  createParser,
  newConversationRequest,
  newReactionRequest,
  newRequest,
  parseRecipients,
  validateAttachments,
  walkOlder,
} from "./stream.mjs";

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

test("outgoing request helpers snapshot bodies for idempotent retries", () => {
  const attachmentIDs = ["upload-1"];
  const message = newRequest("conversation-1", "hello", attachmentIDs);
  attachmentIDs.push("upload-2");
  assert.deepEqual(message.body, {
    conversation_id: "conversation-1",
    text: "hello",
    attachment_ids: ["upload-1"],
  });
  assert.match(message.key, /^[0-9a-f]{32}$/);

  assert.deepEqual(newConversationRequest(["+14155550100"]).body, {
    recipients: ["+14155550100"],
  });
  assert.deepEqual(newReactionRequest("message-1", "👍").body, {
    message_id: "message-1",
    emoji: "👍",
    remove: false,
  });
});

test("recipient and attachment limits are checked before network activity", () => {
  assert.deepEqual(
    parseRecipients("+14155550100, +442071838750 +14155550100"),
    ["+14155550100", "+442071838750"],
  );
  assert.throws(() => parseRecipients("415-555-0100"), /international/);
  assert.equal(validateAttachments([{ size: 1024 }, { size: 2048 }]), 3072);
  assert.throws(
    () => validateAttachments(Array.from({ length: 11 }, () => ({ size: 1 }))),
    /10 attachments/,
  );
  assert.throws(
    () => validateAttachments([{ size: 20 * 1024 * 1024 + 1 }]),
    /20 MiB/,
  );
});

test("captions are queued after attachments as separate requests", () => {
  const both = splitRequests("c1", " caption ", ["upload-1"]);
  assert.deepEqual(
    both.map((r) => r.body),
    [
      { conversation_id: "c1", text: "", attachment_ids: ["upload-1"] },
      { conversation_id: "c1", text: " caption ", attachment_ids: [] },
    ],
  );
  assert.notEqual(both[0].key, both[1].key);
  assert.equal(splitRequests("c1", "   ", ["upload-1"]).length, 1);
  assert.equal(splitRequests("c1", "hi").length, 1);
  assert.equal(splitRequests("c1", "").length, 0);
});

test("paging to the start of a thread stops at the oldest page", async () => {
  const pages = { c: "b", b: "a", a: "" };
  const seen = [];
  assert.equal(
    await walkOlder(
      async (before) => {
        seen.push(before);
        return pages[before];
      },
      { before: "c" },
    ),
    "",
  );
  assert.deepEqual(seen, ["c", "b", "a"]);
});
test("paging stops when cancelled or when a page repeats its cursor", async () => {
  const seen = [];
  await walkOlder(
    async (before) => {
      seen.push(before);
      return "b";
    },
    { before: "c", cancelled: () => seen.length >= 3 },
  );
  assert.deepEqual(seen, ["c", "b"]);

  const stuck = [];
  assert.equal(
    await walkOlder(
      async (before) => {
        stuck.push(before);
        return before;
      },
      { before: "c" },
    ),
    "c",
  );
  assert.deepEqual(stuck, ["c"]);
});

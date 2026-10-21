import test from "node:test";
import assert from "node:assert/strict";
import {
  splitRequests,
  createParser,
  applyMessageEvent,
  mergeMessagePage,
  readStreamChunk,
  newConversationRequest,
  newReactionRequest,
  newRequest,
  clipboardDataEmpty,
  clipboardImages,
  pastedImageName,
  pastedImages,
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

function messageCache() {
  return {
    messages: new Map(),
    versions: new Map(),
    cursor: 0,
    fetched: false,
  };
}
function loadPage(entry, messages, cursor) {
  mergeMessagePage(entry, { messages, cursor });
  entry.fetched = true;
}
function update(entry, data, id) {
  return applyMessageEvent(entry, { id, entity_id: data.id, data });
}

test("updates remain visible even when an in-flight page fails", () => {
  const entry = messageCache();
  loadPage(entry, [{ id: "m", status: "sent" }], 10);
  entry.loading = true;
  const receipt = { id: "m", status: "read" };
  assert.equal(update(entry, receipt, 12), true);
  assert.deepEqual(entry.messages.get("m"), receipt);
  entry.loading = false;
  assert.deepEqual(entry.messages.get("m"), receipt);
  loadPage(entry, [{ id: "m", status: "sent" }], 11);
  assert.deepEqual(entry.messages.get("m"), receipt);
});

test("a newer page does not suppress a delayed update for another page", () => {
  const entry = messageCache();
  loadPage(entry, [{ id: "old", status: "sent" }], 10);
  loadPage(entry, [{ id: "recent", status: "sent" }], 20);
  const receipt = { id: "old", status: "read" };
  assert.equal(update(entry, receipt, 15), true);
  assert.deepEqual(entry.messages.get("old"), receipt);
  assert.equal(update(entry, { id: "recent", status: "pending" }, 19), false);
  assert.equal(update(entry, { id: "unseen" }, 9), false);
});

test("pages and duplicate events cannot roll back newer edits or receipts", () => {
  const entry = messageCache();
  update(entry, { id: "m", text: "edited" }, 12);
  loadPage(entry, [{ id: "m", text: "original" }], 11);
  assert.equal(entry.messages.get("m").text, "edited");
  update(entry, { id: "m", text: "edited again" }, 15);
  loadPage(entry, [{ id: "other" }], 20);
  assert.equal(update(entry, { id: "m", text: "edited" }, 12), false);
  loadPage(entry, [{ id: "m", text: "deleted" }], 21);
  assert.equal(entry.messages.get("m").text, "deleted");
  assert.equal(update(entry, { id: "m", text: "old" }, 18), false);
});

test("a missing heartbeat aborts a stalled stream read", async () => {
  const controller = new AbortController();
  const reader = {
    read: () =>
      new Promise((_, reject) => {
        controller.signal.addEventListener(
          "abort",
          () => reject(new Error("aborted")),
          { once: true },
        );
      }),
  };
  await assert.rejects(readStreamChunk(reader, controller, 10), /aborted/);
  assert.equal(controller.signal.aborted, true);
});

test("stream heartbeats clear their deadline", async () => {
  const controller = new AbortController();
  const chunk = { done: false, value: new Uint8Array([10]) };
  assert.equal(
    await readStreamChunk({ read: async () => chunk }, controller, 10),
    chunk,
  );
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(controller.signal.aborted, false);
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
    name: "",
  });
  assert.deepEqual(
    newConversationRequest(["+14155550100", "+442071838750"], " Book club ")
      .body,
    {
      recipients: ["+14155550100", "+442071838750"],
      name: "Book club",
    },
  );
  assert.deepEqual(newReactionRequest("message-1", "👍").body, {
    message_id: "message-1",
    emoji: "👍",
    remove: false,
  });
});

test("attachment limits are checked before network activity", () => {
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

test("clipboard image files become attachments without consuming text paste", () => {
  const image = { name: "screenshot.png", size: 12, type: "image/png" };
  const text = { name: "notes.txt", size: 5, type: "text/plain" };
  assert.deepEqual(
    pastedImages({
      items: [
        { kind: "string", type: "text/plain" },
        { kind: "file", type: image.type, getAsFile: () => image },
        { kind: "file", type: text.type, getAsFile: () => text },
      ],
    }),
    [image],
  );
  assert.deepEqual(pastedImages({ files: [image, text] }), [image]);
  assert.deepEqual(pastedImages({ items: [], files: [text] }), []);
});

test("an empty paste DataTransfer sends us to the clipboard itself", () => {
  assert.equal(clipboardDataEmpty(undefined), true);
  assert.equal(clipboardDataEmpty({ types: [], items: [], files: [] }), true);
  assert.equal(clipboardDataEmpty({ types: ["text/plain"] }), false);
  assert.equal(
    clipboardDataEmpty({ items: [{ kind: "file", type: "image/png" }] }),
    false,
  );
});

test("clipboard reads yield image blobs and survive refused flavors", async () => {
  const png = { size: 3, type: "image/png" };
  const clipboard = {
    read: async () => [
      { types: ["text/plain"], getType: async () => ({}) },
      {
        types: ["image/png"],
        getType: async (type) => (assert.equal(type, "image/png"), png),
      },
      {
        types: ["image/tiff"],
        getType: async () => {
          throw new Error("unavailable");
        },
      },
    ],
  };
  assert.deepEqual(await clipboardImages(clipboard), [png]);
  assert.deepEqual(await clipboardImages(undefined), []);
  assert.deepEqual(
    await clipboardImages({
      read: async () => {
        throw new Error("denied");
      },
    }),
    [],
  );
});

test("unnamed pasted images receive uploadable filenames", () => {
  assert.equal(pastedImageName({ name: "photo.jpeg" }), "photo.jpeg");
  assert.equal(
    pastedImageName({ name: "", type: "image/png" }),
    "pasted-image.png",
  );
  assert.equal(
    pastedImageName({ name: "", type: "image/jpeg" }, 1),
    "pasted-image-2.jpg",
  );
  assert.equal(
    pastedImageName({ name: "", type: "image/vnd.example" }),
    "pasted-image.img",
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

test("WhatsApp attachments get separate durable requests", () => {
  const requests = splitRequests("chat@lid", "caption", ["a", "b"], "whatsapp");
  assert.equal(requests.length, 3);
  assert.deepEqual(
    requests.map((r) => r.body.attachment_ids),
    [["a"], ["b"], []],
  );
  assert.equal(new Set(requests.map((r) => r.key)).size, 3);
});

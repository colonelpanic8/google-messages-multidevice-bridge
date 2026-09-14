import test from "node:test";
import assert from "node:assert/strict";
import {
  contactDetail,
  contactKey,
  contactName,
  displayName,
  filterConversations,
  importStatus,
  initials,
  layoutThread,
  linkify,
  listTime,
  matchContacts,
  participantList,
  previewLine,
  queuePosition,
  typedRecipient,
  reactedByMe,
  reactionNames,
  reactionTitle,
  messageStatus,
  outboxStatus,
  threadOutbox,
} from "./view.mjs";

const at = (minutes) =>
  new Date(Date.UTC(2026, 8, 13, 12, minutes)).toISOString();
const now = new Date(Date.UTC(2026, 8, 13, 13, 0));

test("thread layout groups consecutive senders and inserts time dividers", () => {
  const rows = layoutThread(
    [
      { id: "1", time: at(0), direction: "incoming", sender_id: "a" },
      { id: "2", time: at(1), direction: "incoming", sender_id: "a" },
      { id: "3", time: at(2), direction: "outgoing", sender_id: "me" },
      { id: "4", time: at(30), direction: "incoming", sender_id: "a" },
    ],
    [{ id: "o1", created: at(31), state: "queued", request: { text: "hi" } }],
    { now },
  );
  assert.deepEqual(
    rows.map((row) => row.kind),
    [
      "divider",
      "message",
      "message",
      "message",
      "divider",
      "message",
      "outbox",
    ],
  );
  assert.deepEqual(
    rows
      .filter((row) => row.kind !== "divider")
      .map((row) => [row.first, row.last]),
    [
      [true, false],
      [false, true],
      [true, true],
      [true, true],
      [true, true],
    ],
  );
});

test("outbox rows hide conversation requests and confirmed stored sends", () => {
  const outbox = [
    { id: "a", state: "queued", request: { conversation_id: "c", text: "x" } },
    {
      id: "b",
      state: "confirmed",
      message_id: "m1",
      request: { conversation_id: "c" },
    },
    {
      id: "c",
      state: "confirmed",
      message_id: "m9",
      request: { conversation_id: "c" },
    },
    {
      id: "d",
      state: "queued",
      request: { kind: "conversation", recipients: ["+1"] },
      conversation_id: "c",
    },
    { id: "e", state: "queued", request: { conversation_id: "other" } },
  ];
  assert.deepEqual(
    threadOutbox(outbox, "c", new Set(["m1"])).map((item) => item.id),
    ["a", "c"],
  );
});

test("status labels collapse provider states", () => {
  assert.deepEqual(messageStatus("outgoing_displayed"), {
    label: "Read",
    error: false,
  });
  assert.deepEqual(messageStatus("outgoing_delivered"), {
    label: "Delivered",
    error: false,
  });
  assert.deepEqual(messageStatus("outgoing_complete"), {
    label: "Sent",
    error: false,
  });
  assert.deepEqual(messageStatus("outgoing_sending"), {
    label: "Sending…",
    error: false,
  });
  assert.deepEqual(messageStatus("outgoing_not_delivered_yet"), {
    label: "Sent",
    error: false,
  });
  assert.deepEqual(messageStatus("outgoing_failed_generic"), {
    label: "Not sent",
    error: true,
  });
  assert.equal(messageStatus("incoming_complete").label, "");
});

test("an outbox row reports progress until its message takes over", () => {
  const sending = { label: "Sending…", error: false };
  assert.deepEqual(outboxStatus({ state: "queued" }, true), sending);
  assert.deepEqual(outboxStatus({ state: "queued" }, false), {
    label: "Waiting for phone…",
    error: false,
  });
  assert.deepEqual(outboxStatus({ state: "sending" }, true), sending);
  assert.deepEqual(
    outboxStatus({ state: "accepted", detail: "Google accepted" }, true),
    sending,
  );
  assert.deepEqual(outboxStatus({ state: "confirmed" }, true), sending);
  assert.deepEqual(
    outboxStatus({ state: "ambiguous", detail: "unknown" }, true),
    { label: "Not sent · unknown", error: true },
  );
});

test("names, initials, and search", () => {
  const conversation = {
    id: "9",
    participants: [
      { id: "1", name: "Me", is_me: true },
      { id: "2", name: "Ada Lovelace", address: "+15550100" },
      { id: "3", name: "Ada Lovelace", address: "+15550100" },
    ],
  };
  assert.equal(displayName(conversation), "Ada Lovelace");
  const selfCopies = {
    id: "1",
    participants: [
      { id: "2", name: "Me", address: "+1300", is_me: true },
      { id: "17", name: "Kat", address: "+1949" },
      { id: "18", name: "Me", address: "+1300" },
    ],
  };
  assert.equal(displayName(selfCopies), "Kat");
  assert.equal(filterConversations([selfCopies], "kat").length, 1);
  assert.equal(initials("Ada Lovelace"), "A");
  assert.equal(initials("+1 555"), "");
  assert.equal(filterConversations([conversation], "0100").length, 1);
  assert.equal(filterConversations([conversation], "zzz").length, 0);
});

test("preview lines attribute the newest message", () => {
  const group = {
    id: "g",
    preview: "on my way",
    participants: [
      { id: "1", name: "Me", is_me: true },
      { id: "2", name: "Ada Lovelace", address: "+15550100" },
      { id: "3", name: "Grace Hopper", address: "+15550101" },
    ],
  };
  assert.equal(
    previewLine({ ...group, preview_direction: "outgoing" }),
    "You: on my way",
  );
  assert.equal(
    previewLine({
      ...group,
      preview_direction: "incoming",
      preview_sender_id: "3",
    }),
    "Grace: on my way",
  );
  assert.equal(
    previewLine({
      ...group,
      preview_direction: "incoming",
      preview_sender_id: "?",
    }),
    "on my way",
  );
  const direct = {
    id: "d",
    preview: "hi",
    preview_direction: "incoming",
    preview_sender_id: "2",
    participants: [
      { id: "1", name: "Me", is_me: true },
      { id: "2", name: "Ada Lovelace", address: "+15550100" },
    ],
  };
  assert.equal(previewLine(direct), "hi");
  assert.equal(
    previewLine({ ...direct, preview_direction: "outgoing" }),
    "You: hi",
  );
  assert.equal(previewLine({ id: "e", preview_direction: "outgoing" }), "");
});

test("list time uses clock time today", () => {
  assert.match(listTime(at(5), now), /12:05|13:05|\d/);
  assert.equal(listTime("", now), "");
});

test("linkify finds URLs and keeps surrounding text", () => {
  assert.deepEqual(linkify("see https://a.io/x, ok"), [
    { text: "see " },
    { text: "https://a.io/x", href: "https://a.io/x" },
    { text: ", ok" },
  ]);
  assert.deepEqual(linkify("plain"), [{ text: "plain" }]);
});

test("reaction attribution names participants and folds the owner's SIM legs", () => {
  const conversation = {
    id: "c",
    participants: [
      { id: "me", name: "Me", address: "+1300", is_me: true },
      { id: "me-sim2", name: "Me", address: "+1300" },
      { id: "a", name: "Ada Lovelace", address: "+15550100" },
      { id: "b", address: "+15550199" },
    ],
  };
  assert.deepEqual(
    reactionNames(
      { emoji: "👍", participants: ["a", "me-sim2"] },
      conversation,
    ),
    ["You", "Ada Lovelace"],
  );
  assert.deepEqual(
    reactionNames(
      { emoji: "👍", participants: ["a", "b", "gone"] },
      conversation,
    ),
    ["Ada Lovelace", "+15550199", "Someone"],
  );
  assert.equal(
    reactionTitle({ emoji: "👍", participants: ["me"] }, conversation),
    "You reacted 👍",
  );
  assert.equal(
    reactionTitle(
      { emoji: "❤️", participants: ["a", "b", "me"] },
      conversation,
    ),
    "You, Ada Lovelace, and +15550199 reacted ❤️",
  );
  assert.equal(
    reactionTitle({ emoji: "😮", participants: [] }, conversation),
    "Reacted 😮",
  );
  assert.equal(reactedByMe({ participants: ["me-sim2"] }, conversation), true);
  assert.equal(reactedByMe({ participants: ["a"] }, conversation), false);
});

test("import status distinguishes a running import from one still in line", () => {
  const jobs = [
    { id: "messages:a", state: "queued", updated: at(0) },
    { id: "messages:b", state: "queued", updated: at(1) },
    { id: "messages:c", state: "complete", updated: at(2) },
  ];
  assert.equal(queuePosition(jobs, "messages:a"), 0);
  assert.equal(queuePosition(jobs, "messages:b"), 1);
  assert.equal(queuePosition(jobs, "messages:c"), -1);

  const running = importStatus(jobs[0], { ahead: 0 });
  assert.equal(running.state, "active");
  assert.match(running.label, /Importing older messages/);

  const waiting = importStatus(jobs[1], { ahead: 1 });
  assert.equal(waiting.state, "waiting");
  assert.match(waiting.label, /1 conversation ahead/);
});

test("import status stays silent only once the conversation is fully imported", () => {
  assert.equal(importStatus({ state: "complete" }), null);
  assert.equal(importStatus(undefined).state, "idle");
  assert.equal(importStatus({ state: "paused" }).state, "paused");

  const failed = importStatus({
    state: "failed",
    detail: "cursor unsupported",
  });
  assert.equal(failed.state, "failed");
  assert.equal(failed.label, "cursor unsupported");
  assert.equal(importStatus({ state: "failed" }).label, "Import stopped.");

  const offline = importStatus(
    { state: "queued", records: 12 },
    {
      connected: false,
    },
  );
  assert.equal(offline.state, "waiting");
  assert.match(offline.label, /reconnects · 12 imported/);

  const retrying = importStatus({
    state: "queued",
    detail: "Phone history unavailable; will retry",
  });
  assert.match(retrying.label, /will retry/);
});

const addressBook = [
  { id: "p1", name: "Ada Lovelace", address: "+14155550100", frequent: true },
  { id: "p2", name: "Alan Turing", address: "+442071838750" },
  { id: "p3", name: "", address: "+15125550111", formatted: "(512) 555-0111" },
  { id: "p4", name: "No Number", address: "" },
];

test("contacts match by name, by word, and by the digits of a number", () => {
  assert.deepEqual(
    matchContacts(addressBook, "ada").map((c) => c.id),
    ["p1"],
  );
  assert.deepEqual(
    matchContacts(addressBook, "lovelace").map((c) => c.id),
    ["p1"],
  );
  assert.deepEqual(
    matchContacts(addressBook, "(512) 555").map((c) => c.id),
    ["p3"],
  );
  // A name prefix outranks a match buried in the middle of another name.
  assert.deepEqual(
    matchContacts(addressBook, "a").map((c) => c.id),
    ["p1", "p2"],
  );
});

test("the picker offers only contacts that can be addressed", () => {
  assert.ok(!matchContacts(addressBook, "").some((c) => c.id === "p4"));
  assert.deepEqual(
    matchContacts(addressBook, "", { exclude: [addressBook[0]] }).map(
      (c) => c.id,
    ),
    ["p2", "p3"],
  );
});

test("a number typed in full is a recipient of its own", () => {
  assert.equal(typedRecipient(" +14155550100 ")?.address, "+14155550100");
  assert.equal(typedRecipient("4155550100"), null);
  assert.equal(typedRecipient("+1"), null);
});

test("a contact reads as its name over its number, and is keyed by number", () => {
  assert.equal(contactName(addressBook[0]), "Ada Lovelace");
  assert.equal(contactDetail(addressBook[0]), "+14155550100");
  // A contact with no name already shows its number, so the second line stays
  // empty rather than repeating it.
  assert.equal(contactName(addressBook[2]), "(512) 555-0111");
  assert.equal(contactDetail(addressBook[2]), "");
  assert.equal(contactKey(addressBook[0]), "+14155550100");
});

test("a group lists everyone with the owner last", () => {
  const conversation = {
    participants: [
      { id: "me", name: "Me", address: "+15550000000", is_me: true },
      { id: "a", name: "Ada", address: "+14155550100", is_me: false },
      { id: "b", name: "", address: "+442071838750", is_me: false },
    ],
  };
  assert.deepEqual(
    participantList(conversation).map((p) => [p.name, p.detail, p.isMe]),
    [
      ["Ada", "+14155550100", false],
      ["+442071838750", "", false],
      ["You", "+15550000000", true],
    ],
  );
});

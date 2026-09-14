import test from "node:test";
import assert from "node:assert/strict";
import {
  displayName,
  filterConversations,
  initials,
  layoutThread,
  linkify,
  listTime,
  reactedByMe,
  reactionNames,
  reactionTitle,
  statusLabel,
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
  assert.equal(statusLabel("outgoing_displayed"), "Read");
  assert.equal(statusLabel("outgoing_delivered"), "Delivered");
  assert.equal(statusLabel("outgoing_complete"), "Sent");
  assert.equal(statusLabel("outgoing_failed_generic"), "Not sent");
  assert.equal(statusLabel("incoming_complete"), "");
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

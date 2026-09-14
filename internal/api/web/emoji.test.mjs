import test from "node:test";
import assert from "node:assert/strict";
import {
  EMOJI,
  GROUPS,
  emojiLabel,
  lookupEmoji,
  searchEmoji,
} from "./emoji.mjs";

const emojiOf = (query, limit) =>
  searchEmoji(query, limit).map((entry) => entry.emoji);

test("catalog is grouped and every entry is reachable by its glyph", () => {
  assert.ok(GROUPS.length > 5);
  assert.ok(EMOJI.length > 1000);
  for (const entry of EMOJI) assert.equal(lookupEmoji(entry.emoji), entry);
});

test("search ranks whole-name and keyword hits above substrings", () => {
  assert.equal(emojiOf("thumbs up")[0], "👍");
  assert.equal(emojiOf("red heart")[0], "❤️");
  assert.equal(emojiOf("+1")[0], "👍");
  assert.ok(emojiOf("party", 20).includes("🎉"));
});

test("every query token must match, and misses return nothing", () => {
  assert.deepEqual(emojiOf("grinning zzzz"), []);
  assert.deepEqual(emojiOf("zzzzzz"), []);
  assert.ok(emojiOf("cat face", 20).includes("🐱"));
  assert.ok(!emojiOf("cat face", 200).includes("🎉"));
});

test("a pasted glyph finds itself, with or without its variation selector", () => {
  assert.deepEqual(emojiOf("🎉"), ["🎉"]);
  assert.equal(emojiOf("❤")[0], "❤️");
  assert.equal(emojiLabel("👍"), "thumbs up");
  assert.equal(emojiLabel("not-an-emoji"), "");
});

test("search honours the result limit", () => {
  assert.equal(searchEmoji("face", 5).length, 5);
});

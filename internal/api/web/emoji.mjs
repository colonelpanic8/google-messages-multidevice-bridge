// Emoji catalog and search over the generated Unicode/CLDR data. Loaded on
// demand so the initial client payload stays small.

import { EMOJI_GROUPS } from "./emoji-data.mjs";

// CLDR order is the palette order, and the index doubles as the tie-breaker
// that keeps search results stable.
export const EMOJI = [];
export const GROUPS = EMOJI_GROUPS.map(([name, blob]) => {
  const entries = blob.split("\n").map((record) => {
    const [emoji, label, terms = ""] = record.split("\t");
    const entry = { emoji, label, terms, order: EMOJI.length };
    EMOJI.push(entry);
    return entry;
  });
  return { name, emoji: entries };
});

const byEmoji = new Map(EMOJI.map((entry) => [entry.emoji, entry]));

// Variation selectors and skin tone modifiers are stripped so a reaction that
// arrived in any form still resolves to a catalog entry.
const BARE = /[\u{FE0F}\u{FE0E}\u{1F3FB}-\u{1F3FF}]/gu;

export function lookupEmoji(emoji) {
  if (!emoji) return undefined;
  return (
    byEmoji.get(emoji) ||
    byEmoji.get(emoji.replace(BARE, "")) ||
    byEmoji.get(`${emoji}\u{FE0F}`)
  );
}

export function emojiLabel(emoji) {
  return lookupEmoji(emoji)?.label || "";
}

const tokenize = (query) =>
  (query || "")
    .toLowerCase()
    .split(/[^\p{L}\p{N}+]+/u)
    .filter(Boolean);

// Lower is better: a whole-name hit beats a word start, which beats a keyword,
// which beats an arbitrary substring.
function tokenScore(entry, token) {
  const label = entry.label;
  if (label === token) return 0;
  if (label.startsWith(token)) return 1;
  if (label.includes(` ${token}`)) return 2;
  const terms = entry.terms;
  if (terms === token || terms.startsWith(`${token} `)) return 3;
  if (terms.includes(` ${token} `) || terms.endsWith(` ${token}`)) return 3;
  if (terms.startsWith(token) || terms.includes(` ${token}`)) return 4;
  if (label.includes(token)) return 5;
  return terms.includes(token) ? 6 : -1;
}

// Every token must match something, so "red heart" does not match every heart.
export function searchEmoji(query, limit = 120) {
  const tokens = tokenize(query);
  // A pasted emoji should offer itself even though its own glyph is not a
  // search term.
  const trimmed = (query || "").trim();
  const pasted = [
    lookupEmoji(trimmed),
    ...[...trimmed].map((char) => lookupEmoji(char)),
  ].filter(Boolean);
  if (!tokens.length) return [...new Set(pasted)].slice(0, limit);
  const hits = [];
  for (const entry of EMOJI) {
    let score = 0;
    for (const token of tokens) {
      const value = tokenScore(entry, token);
      if (value < 0) {
        score = -1;
        break;
      }
      score += value;
    }
    if (score >= 0) hits.push({ entry, score });
  }
  hits.sort((a, b) => a.score - b.score || a.entry.order - b.entry.order);
  const results = [...pasted, ...hits.map((hit) => hit.entry)];
  return [...new Set(results)].slice(0, limit);
}

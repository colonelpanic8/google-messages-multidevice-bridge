// Run against the synthetic browser fixture; see README.md.
export default async function checkCompose(page) {
  const assert = (condition, message) => {
    if (!condition) throw new Error(message);
  };
  await page.setViewportSize({ width: 1280, height: 900 });
  const input = page.getByRole("textbox", { name: "Message", exact: true });
  const height = () => input.evaluate((node) => node.clientHeight);
  const scrollable = () =>
    input.evaluate((node) => getComputedStyle(node).overflowY === "auto");
  const fill = async (value) => {
    await input.fill(value);
    await input.evaluate(
      () => new Promise((resolve) => requestAnimationFrame(resolve)),
    );
    return height();
  };

  await page
    .getByRole("textbox", { name: "Bridge API token" })
    .fill("synthetic-browser-test-token-only");
  await page.getByRole("button", { name: "Unlock", exact: true }).click();
  await page.getByRole("button", { name: /L Long history/ }).click();
  await page.getByRole("button", { name: "Conversation options" }).click();
  await page
    .getByRole("menuitem", { name: "Go to start of conversation" })
    .click();
  await page.waitForFunction(
    () => document.getElementById("rows").children.length === 3000,
  );

  assert((await fill("")) === 48, "Empty composer should be one line");
  assert(
    (await fill("one\n")) === 70,
    "Trailing newline should grow the composer",
  );
  assert((await fill("one\ntwo\nthree")) === 92, "Explicit lines should fit");
  assert(
    (await fill("line\n".repeat(30))) === 160,
    "Long drafts should be capped",
  );
  assert(
    await input.evaluate((node) => node.scrollHeight > node.clientHeight),
    "Capped drafts should scroll",
  );
  assert(await scrollable(), "Capped drafts should show a scrollbar");
  assert(
    (await fill("short")) === 48,
    "Deleting text should shrink the composer",
  );
  assert(!(await scrollable()), "Uncapped drafts should not show a scrollbar");

  for (const value of ["word ".repeat(30), "x".repeat(150), "👩🏽‍💻 ".repeat(60)]) {
    const size = await fill(value);
    assert(size > 48, "Wrapped text should grow the composer");
    assert(
      await input.evaluate((node) => node.scrollHeight <= node.clientHeight),
      "Wrapped text should fit without scrolling below the height cap",
    );
  }
  const draft = "A draft that wraps across lines. ".repeat(4);
  const wide = await fill(draft);
  const viewport = page.viewportSize();
  await page.setViewportSize({ width: 480, height: 800 });
  assert(
    (await height()) > wide,
    "Narrowing the window should rewrap the draft",
  );
  await page.setViewportSize(viewport);
  assert(
    (await height()) === wide,
    "Widening the window should shrink the draft",
  );

  await page.getByRole("button", { name: /P Project notes/ }).click();
  assert(
    (await height()) === 48,
    "An empty conversation should have a small composer",
  );
  await page.getByRole("button", { name: /L Long history/ }).click();
  assert(
    (await input.inputValue()) === draft,
    "Switching should restore the draft",
  );
  assert((await height()) === wide, "Restored drafts should retain their size");

  const timing = await input.evaluate(async (node) => {
    const samples = [];
    for (let i = 0; i < 60; i++) {
      await new Promise(requestAnimationFrame);
      const start = performance.now();
      node.value = "Synthetic typing performance measurement ".repeat(3) + i;
      node.dispatchEvent(new Event("input", { bubbles: true }));
      samples.push(performance.now() - start);
    }
    samples.sort((a, b) => a - b);
    return { median: samples[30], p95: samples[57], max: samples[59] };
  });
  await page.getByRole("button", { name: "More options" }).click();
  await page.getByRole("menuitem", { name: /Lock/ }).click();
  assert(
    (await page.locator("#text").inputValue()) === "",
    "Locking should clear the draft",
  );
  assert(
    !(await page.locator("#compose-size").textContent()).trim(),
    "Locking should clear the sizing copy",
  );
  return { passed: true, messages: 1500, inputHandlerMilliseconds: timing };
}

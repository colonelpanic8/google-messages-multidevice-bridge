// Synthetic fixture only: instrumentation is injected by Playwright, never shipped.
export default async function checkPerformance(page, verify = true) {
  const assert = (condition, message) => {
    if (verify && !condition) throw new Error(message);
  };
  await page.evaluate(async () => {
    for (const registration of await navigator.serviceWorker.getRegistrations())
      await registration.unregister();
  });
  await page.route("**/sw.js", (route) => route.abort());
  await page.route("**/app.js", async (route) => {
    const response = await route.fetch();
    const headers = response.headers();
    delete headers["content-encoding"];
    delete headers["content-length"];
    await route.fulfill({
      response,
      headers,
      body:
        (await response.text()) +
        `
const measurements = { renders: 0, lists: 0 };
const originalThreadRender = renderThread;
renderThread = () => { measurements.renders++; const start = performance.now(); originalThreadRender(); measurements.lastRenderMilliseconds = performance.now() - start; };
const originalListRender = renderList;
renderList = () => { measurements.lists++; const start = performance.now(); originalListRender(); measurements.lastListMilliseconds = performance.now() - start; };
window.performanceFixture = {
  measurements, renderThread, applyEvent, select, loadAll,
  previews: () => previewCache.size,
  message: () => threads.get(selected).messages.get("long-1500"),
};`,
    });
  });
  await page.reload();
  await page
    .getByRole("textbox", { name: "Bridge API token" })
    .fill("synthetic-browser-test-token-only");
  await page.getByRole("button", { name: "Unlock", exact: true }).click();
  await page.getByRole("button", { name: /L Long history/ }).click();
  const start = Date.now();
  await page.getByRole("button", { name: "Conversation options" }).click();
  await page
    .getByRole("menuitem", { name: "Go to start of conversation" })
    .click();
  await page.waitForFunction(
    () => document.getElementById("rows").children.length === 3000,
  );
  const historyMilliseconds = Date.now() - start;
  assert(
    (await page.locator("#rows .react-choices").count()) === 0,
    "Closed reaction menus should not allocate buttons",
  );
  const reaction = page.locator("#rows .react summary").first();
  await reaction.click();
  await page.getByRole("button", { name: "React 👍", exact: true }).waitFor();
  await reaction.click();
  const result = await page.evaluate(async () => {
    const f = window.performanceFixture;
    const settle = () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      );
    await settle();
    const samples = [];
    for (let i = 0; i < 8; i++) {
      const start = performance.now();
      f.renderThread();
      document.getElementById("messages").scrollHeight;
      samples.push(performance.now() - start);
      await settle();
    }
    samples.sort((a, b) => a - b);
    const before = f.measurements.renders;
    f.applyEvent({
      id: 1000001,
      type: "outbox",
      entity_id: "other-send",
      data: {
        id: "other-send",
        state: "queued",
        request: { conversation_id: "synthetic-sms", text: "Synthetic" },
      },
    });
    f.applyEvent({
      id: 1000002,
      type: "history",
      entity_id: "messages:synthetic-long",
      data: {
        id: "messages:synthetic-long",
        kind: "messages",
        conversation_id: "synthetic-long",
        state: "queued",
        records: 1500,
      },
    });
    await settle();
    const unrelatedRenders = f.measurements.renders - before;
    f.applyEvent({
      id: 1000003,
      type: "message",
      entity_id: "long-1500",
      data: { ...f.message(), text: "Updated synthetic message" },
    });
    await settle();
    const messageUpdateMilliseconds = f.measurements.lastRenderMilliseconds;
    const messageUpdated = document
      .getElementById("rows")
      .textContent.includes("Updated synthetic message");
    return {
      renderMilliseconds: { median: samples[4], max: samples[7] },
      unrelatedRenders,
      messageUpdated,
      messageUpdateMilliseconds,
      domElements: document.getElementById("rows").querySelectorAll("*").length,
    };
  });
  assert(
    result.unrelatedRenders === 0,
    "History progress and another chat's outbox must not rebuild messages",
  );
  assert(result.messageUpdated, "Live message updates must still appear");
  let requests = 0;
  const count = (request) => {
    if (request.url().includes("/messages?")) requests++;
  };
  page.on("request", count);
  await page.evaluate(() => window.performanceFixture.select("synthetic-sms"));
  await page.evaluate(async () => {
    window.performanceFixture.applyEvent({
      id: 1000004,
      type: "history",
      entity_id: "messages:synthetic-sms",
      data: {
        id: "messages:synthetic-sms",
        kind: "messages",
        conversation_id: "synthetic-sms",
        state: "queued",
      },
    });
    await new Promise((resolve) =>
      requestAnimationFrame(() => requestAnimationFrame(resolve)),
    );
  });
  assert(
    (await page.locator("#rows").textContent()).includes("Synthetic"),
    "History progress must preserve pending messages in an empty thread",
  );
  await page.evaluate(() => window.performanceFixture.select("synthetic-long"));
  page.off("request", count);
  assert(
    requests === 1,
    "Returning to a live cached conversation must not refetch messages",
  );
  if (verify) {
    await page.evaluate(() =>
      window.performanceFixture.select("synthetic-conversation"),
    );
    await page.waitForFunction(
      () => document.querySelector("#rows img.preview")?.naturalWidth > 0,
    );
    const previewURL = await page
      .locator("#rows img.preview")
      .getAttribute("src");
    await page.evaluate(() =>
      window.performanceFixture.select("synthetic-group"),
    );
    await page.waitForFunction(
      () => window.performanceFixture.previews() === 0,
    );
    assert(
      await page.evaluate(async (url) => {
        try {
          await fetch(url);
          return false;
        } catch {
          return true;
        }
      }, previewURL),
      "Leaving a conversation must release its image URLs",
    );
  }
  const list = await page.evaluate(async () => {
    const f = window.performanceFixture;
    const settle = () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      );
    for (let i = 0; i < 2000; i++) {
      const id = `perf-${i}`;
      f.applyEvent({
        id: 3000000 + i,
        type: "conversation",
        entity_id: id,
        data: {
          id,
          name: `Synthetic person ${i}`,
          updated: "2020-01-01T00:00:00Z",
        },
      });
    }
    await settle();
    const initialMilliseconds = f.measurements.lastListMilliseconds;
    const search = document.getElementById("search");
    const samples = [];
    for (const query of ["Synthetic person 19", "Synthetic person 1", ""]) {
      search.value = query;
      search.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
      samples.push({
        query,
        milliseconds: f.measurements.lastListMilliseconds,
        matches: document.getElementById("conversations").children.length,
      });
    }
    return { initialMilliseconds, searches: samples };
  });
  assert(
    list.searches[0].matches === 111 &&
      list.searches[1].matches === 1111 &&
      list.searches[2].matches === 2004,
    "Filtering and restoring the conversation list must preserve matches",
  );
  if (verify) {
    await page.evaluate(() =>
      window.performanceFixture.select("synthetic-long"),
    );
    await page.evaluate(() => window.performanceFixture.loadAll());
    await page.waitForFunction(
      () => window.performanceFixture.message()?.text === "Message 1500",
    );
    await page.waitForFunction(
      () => document.getElementById("conversations").children.length === 4,
    );
  }
  return { historyMilliseconds, ...result, switchRequests: requests, list };
}

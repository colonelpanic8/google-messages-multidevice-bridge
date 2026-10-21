// Runs only against the synthetic browser fixture; no upstream messages are sent.
export default async function checkSync(page) {
  const assert = (condition, message) => {
    if (!condition) throw new Error(message);
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
const networkFetch = window.fetch;
const wire = new TextEncoder();
const fixture = window.syncFixture = {
  events: [], connections: [], loadAll, loadThread, select,
  message: (id, messageID) => threads.get(id)?.messages.get(messageID),
  conversation: (id) => conversations.get(id),
  send(event) {
    this.events.push(event);
    const connection = this.connections.at(-1);
    if (!connection.signal.aborted && !connection.closed)
      connection.controller.enqueue(wire.encode('data: ' + JSON.stringify(event) + '\\n\\n'));
  },
  disconnect() {
    const connection = this.connections.at(-1);
    connection.closed = true;
    connection.controller.close();
  },
};
window.fetch = (input, options = {}) => {
  const url = new URL(input, location.href);
  if (url.pathname !== '/v1/stream') return networkFetch(input, options);
  if (fixture.invalidCursor) {
    fixture.invalidCursor = false;
    return Promise.resolve(new Response('invalid cursor', { status: 400 }));
  }
  const connection = { after: Number(url.searchParams.get('after')), signal: options.signal };
  fixture.connections.push(connection);
  const body = new ReadableStream({
    start(controller) {
      connection.controller = controller;
      options.signal.addEventListener('abort', () => {
        if (!connection.closed) {
          connection.closed = true;
          controller.error(new DOMException('Aborted', 'AbortError'));
        }
      }, { once: true });
      for (const event of fixture.events)
        if (event.id > connection.after)
          controller.enqueue(wire.encode('data: ' + JSON.stringify(event) + '\\n\\n'));
      controller.enqueue(wire.encode('data: {"type":"live"}\\n\\n'));
    },
  });
  return Promise.resolve(new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }));
};`,
    });
  });
  await page.reload();
  await page
    .getByRole("textbox", { name: "Bridge API token" })
    .fill("synthetic-browser-test-token-only");
  await page.getByRole("button", { name: "Unlock", exact: true }).click();
  await page.waitForFunction(
    () => window.syncFixture?.connections.length === 1,
  );
  await page.evaluate(() => window.syncFixture.select("synthetic-long"));

  let releasePage;
  const pageHeld = new Promise((resolve) => {
    releasePage = resolve;
  });
  let pageStarted;
  const started = new Promise((resolve) => {
    pageStarted = resolve;
  });
  await page.route("**/messages?*before=*", async (route) => {
    pageStarted();
    await pageHeld;
    await route.fulfill({ status: 503, body: "Synthetic history failure" });
  });
  await page.evaluate(() => {
    const f = window.syncFixture;
    f.pending = f.loadThread("synthetic-long", "long-1401").catch(() => {});
  });
  await started;
  await page.evaluate(() => {
    const f = window.syncFixture;
    f.nextID = f.connections.at(-1).after + 100;
    f.send({
      id: f.nextID++,
      type: "message",
      entity_id: "long-1500",
      data: {
        ...f.message("synthetic-long", "long-1500"),
        text: "Live update during failed history",
      },
    });
  });
  await page
    .getByText("Live update during failed history", { exact: true })
    .waitFor();
  releasePage();
  await page.evaluate(() => window.syncFixture.pending);
  assert(
    (await page
      .getByText("Live update during failed history", { exact: true })
      .count()) === 1,
    "Failed history hid live message",
  );
  await page.unroute("**/messages?*before=*");

  // A live update while a full snapshot reload is in flight must be replayed.
  let releaseSnapshot;
  const snapshotHeld = new Promise((resolve) => {
    releaseSnapshot = resolve;
  });
  let snapshotStarted;
  const snapshotReady = new Promise((resolve) => {
    snapshotStarted = resolve;
  });
  await page.route("**/v1/conversations", async (route) => {
    const response = await route.fetch();
    snapshotStarted();
    await snapshotHeld;
    await route.fulfill({ response });
  });
  await page.evaluate(() => {
    const f = window.syncFixture;
    f.pending = f.loadAll();
  });
  await snapshotReady;
  await page.evaluate(() => {
    const f = window.syncFixture;
    f.send({
      id: f.nextID++,
      type: "conversation",
      entity_id: "synthetic-group",
      data: {
        ...f.conversation("synthetic-group"),
        preview: "Preview received during reload",
        preview_direction: "incoming",
        preview_sender_id: "friend",
      },
    });
  });
  releaseSnapshot();
  await page.evaluate(() => window.syncFixture.pending);
  await page
    .getByText("Alex: Preview received during reload", { exact: true })
    .waitFor();
  await page.unroute("**/v1/conversations");
  await page.evaluate(() => {
    const f = window.syncFixture;
    const conversation = f.conversation("synthetic-group");
    f.send({
      id: f.nextID++,
      type: "conversation",
      entity_id: conversation.id,
      data: {
        ...conversation,
        participants: conversation.participants.map((p) =>
          p.id === "friend" ? { ...p, name: "Renamed Friend" } : p,
        ),
      },
    });
  });
  await page
    .getByText("Renamed: Preview received during reload", { exact: true })
    .waitFor();

  const beforeReconnect = await page.evaluate(() => {
    const f = window.syncFixture;
    f.disconnect();
    return f.connections.length;
  });
  await page.waitForFunction(
    (count) => window.syncFixture.connections.length > count,
    beforeReconnect,
  );
  assert(
    await page.evaluate(
      () =>
        window.syncFixture.connections.at(-1).after ===
        window.syncFixture.nextID - 1,
    ),
    "Reconnect did not resume at last event",
  );

  const beforeReset = await page.evaluate(() => {
    const f = window.syncFixture;
    f.invalidCursor = true;
    f.events = [];
    f.disconnect();
    return f.connections.length;
  });
  await page.waitForFunction(
    (count) => window.syncFixture.connections.length > count,
    beforeReset,
  );
  assert(
    await page.evaluate(
      () =>
        window.syncFixture.connections.at(-1).after <
        window.syncFixture.nextID - 1,
    ),
    "Database rollback did not reset the event cursor",
  );
  await page
    .getByText("Live update during failed history", { exact: true })
    .waitFor({ state: "hidden" });
  return {
    liveUpdatesDuringFailedFetch: true,
    snapshotReplay: true,
    previewSenderRefresh: true,
    reconnectResume: true,
    databaseRollbackRecovery: true,
  };
}

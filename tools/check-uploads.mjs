// Run from the repository root against the synthetic browser fixture.
export default async function checkUploads(page) {
  const assert = (condition, message) => {
    if (!condition) throw new Error(message);
  };
  const token = page.getByRole("textbox", { name: "Bridge API token" });
  await token.fill("synthetic-browser-test-token-only");
  await token.press("Enter");
  await page.getByRole("button", { name: /P Project notes/ }).press("Enter");
  const picker = page.locator("#attachments");
  await picker.setInputFiles([
    "client/icons/icon-192.png",
    "client/icons/icon-512.png",
  ]);
  await picker.setInputFiles("client/icons/favicon-32.png");
  await picker.setInputFiles(Array(8).fill("client/icons/icon-192.png"));
  await page.getByText("Choose no more than 10 attachments.").waitFor();
  await page
    .getByRole("button", { name: "Remove favicon-32.png" })
    .press("Enter");

  const attempts = [];
  const receipts = [];
  await page.route("**/v1/uploads?*", async (route) => {
    const name = route.request().url().split("?name=")[1];
    attempts.push(name);
    if (name === "icon-512.png" && attempts.length === 2) {
      await route.fulfill({ status: 503, body: "Upload interrupted" });
      return;
    }
    const response = await route.fetch();
    receipts.push((await response.json()).id);
    await route.fulfill({ response });
  });
  const queued = page.waitForResponse(
    (response) =>
      response.url().endsWith("/v1/messages") &&
      response.request().method() === "POST",
  );
  await page.getByRole("button", { name: "Send", exact: true }).press("Enter");
  const response = await queued;
  const item = await response.json();
  assert(
    response.status() === 202 && item.state === "queued",
    "Photos should reach the outbox",
  );
  assert(
    attempts.join() === "icon-192.png,icon-512.png,icon-512.png",
    "Retry should resume at the interrupted photo",
  );
  assert(
    receipts.length === 2 &&
      receipts[0] !== receipts[1] &&
      item.request.attachment_ids.join() === receipts.join(),
    "The outbox should contain both photos in selection order",
  );
  await page.waitForFunction(
    () => !document.getElementById("selected-attachments").childElementCount,
    undefined,
    { polling: 100 },
  );
  return {
    passed: true,
    attempts,
    queuedPhotos: item.request.attachment_ids.length,
  };
}

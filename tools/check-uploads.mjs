// Run from the repository root against the synthetic browser fixture.
export default async function checkUploads(page) {
  const assert = (condition, message) => {
    if (!condition) throw new Error(message);
  };
  const token = page.getByRole("textbox", { name: "Bridge API token" });
  await token.fill("synthetic-browser-test-token-only");
  await token.press("Enter");
  await page.getByRole("button", { name: /Weekend plans/ }).press("Enter");
  const preview = page.getByRole("img", { name: "park.png" });
  await page.waitForFunction(
    () => document.querySelector("img.preview")?.naturalWidth === 3840,
  );
  for (const width of [1280, 375]) {
    await page.setViewportSize({ width, height: 800 });
    const fits = await preview.evaluate((img) => {
      const bubble = img.closest(".bubble").getBoundingClientRect();
      const row = img.closest(".row").getBoundingClientRect();
      const photo = img.getBoundingClientRect();
      return (
        photo.width > 0 &&
        photo.width <= 360 &&
        Math.abs(photo.height - (photo.width * 9) / 16) < 1 &&
        Math.abs(bubble.width - photo.width) < 1 &&
        Math.abs(photo.right - row.right) < 1 &&
        photo.left >= row.left
      );
    });
    assert(fits, `Outgoing image should fit its bubble at ${width}px`);
  }
  const attachmentID = await preview.getAttribute("data-attachment-id");
  const original = await page.request.get(
    `${await page.evaluate(() => location.origin)}/v1/attachments/${attachmentID}`,
    { headers: { Authorization: "Bearer synthetic-browser-test-token-only" } },
  );
  assert(original.ok(), "Fixture image should be available");
  const photo = await original.body();
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.getByRole("button", { name: /P Project notes/ }).press("Enter");
  const picker = page.locator("#attachments");
  await picker.setInputFiles([
    "client/icons/icon-192.png",
    "client/icons/favicon-32.png",
  ]);
  await picker.setInputFiles("client/icons/icon-512.png");
  await page
    .getByRole("button", { name: "Remove icon-512.png" })
    .press("Enter");
  const draft = page.getByRole("textbox", { name: "Message", exact: true });
  await draft.evaluate((input) => {
    const drop = new DataTransfer();
    drop.items.add(new File(["file"], "dropped.png", { type: "image/png" }));
    input.dispatchEvent(
      new DragEvent("drop", {
        bubbles: true,
        cancelable: true,
        dataTransfer: drop,
      }),
    );
  });
  await page.getByRole("button", { name: "Remove dropped.png" }).press("Enter");
  await draft.evaluate(
    (input, bytes) => {
      const clipboard = new DataTransfer();
      clipboard.items.add(
        new File([new Uint8Array(bytes)], "park.png", { type: "image/png" }),
      );
      input.dispatchEvent(
        new ClipboardEvent("paste", {
          cancelable: true,
          clipboardData: clipboard,
        }),
      );
    },
    [...photo],
  );
  await page.getByRole("button", { name: "Remove park.png" }).waitFor();
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
    if (name === "park.png") {
      assert(
        route.request().postDataBuffer().equals(photo),
        "The upload should preserve every byte of the 4K photo",
      );
    }
    if (name === "park.png" && attempts.length === 2) {
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
    attempts.join() === "icon-192.png,park.png,park.png",
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

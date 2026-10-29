import { chromium } from "@playwright/test";
import assert from "node:assert/strict";
import { mkdir, readFile } from "node:fs/promises";
import { existsSync } from "node:fs";

const origin = process.env.MAKEZ_BROWSER_URL;
if (!origin) throw new Error("MAKEZ_BROWSER_URL is required");
const browser = await chromium.launch({
  executablePath:
    process.env.CHROMIUM_PATH ||
    (existsSync("/usr/bin/chromium") ? "/usr/bin/chromium" : undefined),
  headless: true,
});
const context = await browser.newContext({ acceptDownloads: true });
const page = await context.newPage();
const credentials = [];
page.on("response", async (response) => {
  if (response.url().endsWith("/v1/auth/refresh") && response.status() === 200)
    credentials.push((await response.json()).access_token);
});
try {
  await page.goto(origin);
  await page.evaluate(() => {
    localStorage.setItem("makez.token", "legacy-sensitive-key");
    localStorage.setItem("makez.connection", JSON.stringify({ token: "old-key" }));
  });
  await page.reload();
  await page.getByRole("button", { name: "Sign in with Google", exact: true }).click();
  await page.getByRole("button", { name: "Sign out", exact: true }).waitFor();
  await page.getByLabel("Workspace", { exact: true }).waitFor();
  await page
    .getByLabel("Workspace", { exact: true })
    .selectOption(process.env.MAKEZ_BROWSER_WORKSPACE);
  assert.equal(new URL(page.url()).origin, origin);
  assert.equal(new URL(page.url()).search, "");
  assert.equal(await page.evaluate(() => localStorage.getItem("makez.token")), null);
  assert.equal(await page.evaluate(() => localStorage.getItem("makez.connection")), null);
  const cookies = await context.cookies();
  const refresh = cookies.find((cookie) => cookie.name === "makez-refresh");
  assert.ok(refresh?.httpOnly);
  assert.ok(!(await page.evaluate(() => document.cookie.includes("makez-refresh"))));
  await page.getByText("Completed artifact task", { exact: true }).click();
  await page.getByText("result.txt", { exact: true }).waitFor();
  const downloadPromise = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download result.txt" }).click();
  const download = await downloadPromise;
  assert.equal(await readFile(await download.path(), "utf8"), "verified browser artifact\n");
  await page.screenshot({ path: "/tmp/makez-evidence/browser-artifact.png", fullPage: true });
  await page
    .getByLabel("Workspace", { exact: true })
    .selectOption(process.env.MAKEZ_BROWSER_VIEWER_WORKSPACE);
  await page.getByRole("button", { name: "Send", exact: true }).waitFor();
  assert.ok(await page.getByRole("button", { name: "Send", exact: true }).isDisabled());
  assert.equal(await page.getByText("Completed artifact task", { exact: true }).count(), 0);
  await page
    .getByLabel("Workspace", { exact: true })
    .selectOption(process.env.MAKEZ_BROWSER_WORKSPACE);
  await page
    .getByPlaceholder("Describe the goal and the PR or artifact you want.")
    .fill("Browser queued task");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByText("Canceled", { exact: true }).first().waitFor();
  // Reload two tabs together: refresh serialization must prevent family replay.
  const second = await context.newPage();
  await Promise.all([page.reload(), second.goto(origin)]);
  await Promise.all([
    page.getByRole("button", { name: "Sign out", exact: true }).waitFor(),
    second.getByRole("button", { name: "Sign out", exact: true }).waitFor(),
  ]);
  assert.ok(credentials.length >= 2);
  const revoke = await fetch(
    `${process.env.MAKEZ_BROWSER_API}/v1/workspaces/${process.env.MAKEZ_BROWSER_WORKSPACE}/members/${process.env.MAKEZ_BROWSER_USER}`,
    {
      method: "PATCH",
      headers: {
        Authorization: `Bearer ${process.env.MAKEZ_BROWSER_ADMIN_ACCESS}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ role: "Viewer" }),
    },
  );
  assert.equal(revoke.status, 204);
  await page
    .getByLabel("Workspace", { exact: true })
    .selectOption(process.env.MAKEZ_BROWSER_WORKSPACE);
  await page.waitForFunction(() =>
    document
      .querySelector('select[aria-label="Workspace"]')
      ?.selectedOptions[0]?.textContent.includes("Viewer"),
  );
  assert.ok(await page.getByRole("button", { name: "Send", exact: true }).isDisabled());
  for (const value of await page.evaluate(() => Object.values(localStorage)))
    assert.ok(!credentials.some((token) => value.includes(token)));
  await page.getByRole("button", { name: "Sign out all", exact: true }).click();
  await Promise.all([
    page.getByRole("button", { name: "Sign in with Google", exact: true }).waitFor(),
    second.getByRole("button", { name: "Sign in with Google", exact: true }).waitFor(),
  ]);
  await mkdir("/tmp/makez-evidence", { recursive: true });
  await page.screenshot({ path: "/tmp/makez-evidence/browser-logged-out.png", fullPage: true });
  console.log(
    "Browser journey passed: mock OIDC, workspace isolation, Viewer restrictions, create/cancel, SSE, artifact download, reload, multi-tab refresh, role revocation, logout-all, storage/URL assertions.",
  );
} finally {
  await context.close();
  await browser.close();
}

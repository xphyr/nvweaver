// Drives the Web UI in a real browser, against test/webui-demo. Takes the demo's address, and a directory for screenshots.
// Run through test/webui-e2e.sh.
import { chromium } from "playwright";

const base = process.argv[2];
const shots = process.argv[3];
const tokenBase = process.argv[4];
const tokenValue = process.argv[5];
const passwordBase = process.argv[6];
let pass = 0, fail = 0;
const ok = (name) => { pass++; console.log(`  ok    ${name}`); };
const bad = (name, why) => { fail++; console.log(`  FAIL  ${name}`); if (why) console.log(`        ${why}`); };
const check = (name, cond, why) => (cond ? ok(name) : bad(name, why));

const browser = await chromium.launch();
async function newPage(opts = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, ...opts });
  const page = await context.newPage();
  page.problems = [];
  page.on("console", (m) => { if (m.type() === "error") page.problems.push("console: " + m.text()); });
  page.on("pageerror", (e) => page.problems.push("exception: " + e.message));
  return page;
}
// Moving between screens only changes the hash, and the status line still says "Updated" from the last one, so wait for the screen itself.
const visit = async (page, route, at = base) => {
  await page.goto(at + "/ui/" + route);
  await page.waitForSelector("main h1", { timeout: 10000 }).catch((e) => {
    console.log(`  FAIL  ${at}/ui/${route} never drew anything: ${page.problems.join("; ")}`);
    process.exit(1);
  });
};
// A value that's been drawn by mistake, such as what's left of a condition.
const strayWords = (page) => page.evaluate(() => {
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  const found = [];
  while (walker.nextNode()) {
    const text = walker.currentNode.textContent.trim();
    if (["false", "true", "undefined", "null", "NaN", "[object Object]"].includes(text) && !walker.currentNode.parentElement.closest("td")) found.push(text);
  }
  return found;
});

console.log("\nEvery screen");
const page = await newPage();
const screens = { "": "Overview", "#/pools": "Pools", "#/volumes": "Volumes", "#/snapshots": "Snapshots", "#/subsystems": "Subsystems", "#/hosts": "Hosts", "#/ports": "Ports" };
for (const [route, title] of Object.entries(screens)) {
  await visit(page, route);
  check(`${title} shows its title`, (await page.textContent("h1")) === title, await page.textContent("h1"));
  check(`${title} is marked in the navigation`, (await page.textContent('nav a[aria-current="page"]')) === title);
  check(`${title} has nothing drawn by mistake`, (await strayWords(page)).length === 0, (await strayWords(page)).join(", "));
  await page.screenshot({ path: `${shots}/${title.toLowerCase()}.png`, fullPage: true });
}
check("nothing went wrong in the browser", page.problems.length === 0, page.problems.join("; "));

console.log("\nThe project's own logo");
await visit(page, "");
const logoWidth = await page.evaluate(() => document.querySelector(".brand img").naturalWidth);
check("the mark loads, from the project's images", logoWidth === 180, `its width is ${logoWidth}`);
check("next to its name", (await page.textContent(".brand")).trim() === "nvweaver");
check("the page has an icon", (await page.evaluate(async () => (await fetch(document.querySelector('link[rel="icon"]').href)).status)) === 200);

const dark = await newPage({ colorScheme: "dark" });
await visit(dark, "");
await dark.waitForSelector(".cards");
await dark.screenshot({ path: `${shots}/overview-dark.png`, fullPage: true });

console.log("\nReal data");
await visit(page, "");
await page.waitForSelector(".cards");
const overview = await page.textContent("main");
check("the overview counts the pools and volumes", /2\s*Pools/.test(overview) && /16\s*Volumes/.test(overview), overview.slice(0, 120));
check("and says what it's running", overview.includes("nvmet") && overview.includes("demo"));
await visit(page, "#/volumes");
await page.waitForSelector("main tbody tr");
check("volumes are listed", (await page.locator("main tbody tr").count()) === 16);
check("a clone says where it came from", (await page.textContent("main")).includes("clone of vol-0001-nightly"));
check("an export links to its subsystem", (await page.locator('a[href^="#/subsystems/nqn"]').count()) === 5);
await page.locator('a[href^="#/subsystems/nqn"]').first().click();
await page.waitForSelector("dl.facts");
const detail = await page.textContent("main");
check("the subsystem shows who can connect", detail.includes("Who can connect") && detail.includes("node-a"));
check("and its namespace, serving", (await page.locator(".pill.ok").count()) >= 1 && detail.includes("Live"));
check("it has nothing drawn by mistake", (await strayWords(page)).length === 0, (await strayWords(page)).join(", "));
await page.screenshot({ path: `${shots}/subsystem.png`, fullPage: true });
await visit(page, "#/subsystems");
await page.waitForSelector("main tbody tr");
check("a subsystem open to any host stands out", (await page.locator(".pill.warn").count()) === 1);

console.log("\nMore than one pool");
await visit(page, "#/pools");
await page.waitForSelector("main tbody tr");
check("both are listed", (await page.locator("main tbody tr").count()) === 2);
check("with how many volumes each has", /archive.*sata/s.test(await page.textContent("main")));
await page.screenshot({ path: `${shots}/pools.png`, fullPage: true });
await page.click('main a[href="#/volumes/archive"]');
await page.waitForSelector("main tbody tr");
check("a pool's volume count opens only its volumes", (await page.textContent("h1")) === "Volumes in archive" && (await page.locator("main tbody tr").count()) === 3, await page.textContent("h1"));
check("the picker shows which pool that is", (await page.inputValue(".picker select")) === "archive");
await page.selectOption(".picker select", "fast");
await page.locator("h1", { hasText: "Volumes in fast" }).waitFor();
await page.waitForSelector("main tbody tr");
check("choosing another shows its volumes", (await page.locator("main tbody tr").count()) === 13);
await page.selectOption(".picker select", "");
await page.locator("h1", { hasText: /^Volumes$/ }).waitFor();
await page.waitForSelector("main tbody tr");
check("and All pools shows them all", (await page.locator("main tbody tr").count()) === 16);
await page.screenshot({ path: `${shots}/volumes.png`, fullPage: true });
await visit(page, "#/volume/backup-01");
await page.waitForSelector("dl.facts");
const backup = await page.textContent("main");
check("a volume says which pool it's in", backup.includes("archive"));
check("and has its own snapshots there", backup.includes("backup-01-monthly") && !backup.includes("vol-0001-nightly"));
check("and what it's used of its size", /16 MiB of 20 GiB/.test(backup), backup.slice(0, 120));
await visit(page, "#/snapshots");
await page.waitForSelector("main tbody tr");
check("snapshots come from both pools", (await page.locator("main tbody tr").count()) === 3);

console.log("\nOne volume");
await visit(page, "#/volumes");
await page.waitForSelector("main tbody tr");
await page.click('main tbody a[href="#/volume/vol-0002"]');
await page.waitForSelector("dl.facts");
const vol2 = await page.textContent("main");
check("a volume's name opens its page", (await page.textContent("h1")) === "vol-0002", await page.textContent("h1"));
check("it's still under Volumes in the navigation", (await page.textContent('nav a[aria-current="page"]')) === "Volumes");
check("it shows what's used of its size", /64 MiB of 3 GiB \(2%\)/.test(vol2), vol2.slice(0, 200));
check("and explains what that includes", vol2.includes("shared with its snapshots and clones"));
const barWidth = await page.evaluate(() => document.querySelector("dl.facts .bar > div").style.width);
check("with a bar that matches", /^2\.0\d*%$/.test(barWidth), barWidth);
check("and its snapshots", vol2.includes("vol-0002-before-upgrade"));
check("but no other volume's", !vol2.includes("vol-0001-nightly"));
check("it has nothing drawn by mistake", (await strayWords(page)).length === 0, (await strayWords(page)).join(", "));
await page.screenshot({ path: `${shots}/volume.png`, fullPage: true });
await visit(page, "#/volume/vol-0007");
await page.waitForSelector("dl.facts");
const vol7 = await page.textContent("main");
check("a volume with no snapshots says so", vol7.includes("No snapshots of this volume"));
check("an empty thin volume shows as nearly empty", /of 8 GiB \((0|under 1)%\)/.test(vol7), vol7.slice(0, 160));
const noStorage = await newPage();
await noStorage.route("**/usage", (r) => r.fulfill({ status: 500, json: { error: "lvm: no such volume group" } }));
await visit(noStorage, "#/volume/vol-0002");
await noStorage.waitForSelector("dl.facts");
const vol2b = await noStorage.textContent("main");
check("if the storage can't say, the page still works", vol2b.includes("can't say") && vol2b.includes("vol-0002-before-upgrade"));

console.log("\nFiltering");
await visit(page, "#/volumes");
await page.waitForSelector('input[type="search"]');
await page.fill('input[type="search"]', "vol-0007");
check("it narrows the list", (await page.locator("main tbody tr").count()) === 1);
await page.fill('input[type="search"]', "no such thing");
check("and says when nothing matches", (await page.textContent("main")).includes("Nothing matches"));

console.log("\nData is only ever text");
await visit(page, "#/pools");
await page.waitForSelector("main tbody tr");
check("markup in a label is shown as text", (await page.textContent("main")).includes("<img src=x onerror="));
check("and doesn't become an element", (await page.locator("main img").count()) === 0);
check("and doesn't run", (await page.evaluate(() => window.pwned)) === undefined);

console.log("\nThe security policy is really applied");
const blocked = await page.evaluate(async () => {
  try { await fetch("https://example.com/"); return false; } catch { return true; }
});
check("the page can't send anything to another site", blocked);
const inlineRan = await page.evaluate(() => {
  const s = document.createElement("script");
  s.textContent = "window.inlineRan = true";
  document.head.append(s);
  return window.inlineRan === true;
});
check("an injected script doesn't run", !inlineRan);

console.log("\nA real web page attacking the API");
const attacker = await newPage();
await attacker.route("https://evil.example/**", (r) => r.fulfill({ status: 200, contentType: "text/html", body: "<title>evil</title>" }));
await attacker.goto("https://evil.example/");
const attack = await attacker.evaluate(async (api) => {
  try {
    await fetch(api + "/v1/hosts", { method: "POST", mode: "no-cors", headers: { "Content-Type": "text/plain" }, body: JSON.stringify({ nqn: "nqn.2026-11.net.example:from-evil" }) });
    return "sent";
  } catch (e) { return "blocked: " + e.message; }
}, base);
console.log(`        (the browser reports: ${attack})`);
await visit(page, "#/hosts");
await page.waitForSelector("main tbody tr");
check("a request from another site didn't take effect", !(await page.textContent("main")).includes("from-evil"));

console.log("\nPages");
// Made-up lists, so that this can have thousands, as the API would send them: 500 at a time.
const paged = await newPage();
const allVolumes = Array.from({ length: 1234 }, (_, i) => ({ name: "v" + String(i + 1).padStart(4, "0"), pool: "fast", size_bytes: 2 ** 30, uuid: "u", nguid: "n" + i, state: "online", mapped_to: null, source: null, created_at: "2026-11-06T00:00:00Z", internal_name: "i", device_path: "/d" }));
let volumeRequests = 0;
await paged.route("**/v1/volumes?*", (r) => handleVolumes(r));
await paged.route("**/v1/volumes", (r) => handleVolumes(r));
const handleVolumes = async (r) => {
  if (r.request().method() !== "GET") return r.continue();
  volumeRequests++;
  const url = new URL(r.request().url());
  const after = url.searchParams.get("after") || "";
  const start = after ? allVolumes.findIndex((v) => v.name === after) + 1 : 0;
  const items = allVolumes.slice(start, start + 500);
  const body = { items };
  if (start + 500 < allVolumes.length) body.next_after = items[items.length - 1].name;
  await r.fulfill({ json: body });
};
const rows = (p) => p.locator("main tbody tr").count();
const firstCell = (p) => p.locator("main tbody tr td").first().textContent();
await visit(paged, "#/volumes");
await paged.waitForSelector("main tbody tr");
check("a long list shows 25 rows to begin with", (await rows(paged)) === 25, String(await rows(paged)));
check("and says where it is, and that there's more", (await paged.textContent("#page-range")).replace(/\s/g, "") === "1\u201325of500+", await paged.textContent("#page-range"));
check("Previous can't go anywhere yet", await paged.isDisabled("#page-prev"));
await paged.screenshot({ path: `${shots}/paged.png`, fullPage: true });
await paged.click("#page-next");
check("Next shows the next rows", (await firstCell(paged)).startsWith("v0026"), await firstCell(paged));
await paged.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
await paged.waitForTimeout(400);
check("a refresh keeps its place", (await firstCell(paged)).startsWith("v0026"), await firstCell(paged));
await paged.click("#page-prev");
check("Previous goes back", (await firstCell(paged)).startsWith("v0001"));
await paged.selectOption("#page-size", "250");
check("the number of rows can be raised", (await rows(paged)) === 250);
await paged.click("#page-next");
check("and paging follows it", (await firstCell(paged)).startsWith("v0251"));
await paged.selectOption("#page-size", "100");
check("changing it keeps the same part of the list in view", (await firstCell(paged)).startsWith("v0201"), await firstCell(paged));
await paged.reload();
await paged.waitForSelector("main tbody tr");
check("and it's remembered", (await paged.inputValue("#page-size")) === "100" && (await rows(paged)) === 100);
const before = volumeRequests;
for (let i = 0; i < 4; i++) await paged.click("#page-next");
check("it only asks the API for what's needed, so far", (await firstCell(paged)).startsWith("v0401") && volumeRequests === before);
await paged.click("#page-next");
await paged.locator("main tbody td", { hasText: "v0501" }).first().waitFor();
check("past what's loaded, it fetches the next batch", (await firstCell(paged)).startsWith("v0501") && volumeRequests > before, await firstCell(paged));
for (let i = 0; i < 5; i++) await paged.click("#page-next");
await paged.locator("main tbody td", { hasText: "v1001" }).first().waitFor();
await paged.click("#page-next");
await paged.click("#page-next");
await paged.locator("main tbody td", { hasText: "v1201" }).first().waitFor();
check("and reaches the real end", (await rows(paged)) === 34 && (await paged.textContent("#page-range")).includes("of 1234"), await paged.textContent("#page-range"));
check("where Next stops", await paged.isDisabled("#page-next"));
await paged.fill('input[type="search"]', "v12");
check("filtering goes back to the first page", (await firstCell(paged)).startsWith("v0012") || (await firstCell(paged)).startsWith("v12"), await firstCell(paged));
await paged.fill('input[type="search"]', "");
await paged.selectOption("#page-size", "25");
const narrow = await newPage({ viewport: { width: 390, height: 800 } });
await narrow.route("**/v1/volumes?*", (r) => handleVolumes(r));
await narrow.route("**/v1/volumes", (r) => handleVolumes(r));
await visit(narrow, "#/volumes");
await narrow.waitForSelector("#page-next");
check("the pager fits a phone", !(await narrow.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)));
await visit(page, "#/ports");
await page.waitForSelector("main tbody tr");
check("a short list doesn't show a pager", (await page.locator(".pager").count()) === 0);

console.log("\nCreating and deleting");
const ed = await newPage();
const openOn = async (route, selector = "main h1") => { await visit(ed, route); await ed.waitForSelector(selector); };
const dialogText = () => ed.textContent("dialog");
const submit = () => ed.click("#dialog-submit");
const closed = () => ed.waitForSelector("dialog", { state: "detached" });
const alertInDialog = async () => { await ed.waitForSelector('dialog [role="alert"]'); return ed.textContent('dialog [role="alert"]'); };
const api = (path) => ed.evaluate(async (p) => { const r = await fetch(p); return { status: r.status, body: await r.json().catch(() => null) }; }, path);

await openOn("#/volumes", "#create-volume");
await ed.click("#create-volume");
await ed.waitForSelector("dialog[open]");
await ed.screenshot({ path: `${shots}/create-volume.png` });
check("creating a volume asks which pool, as there are two", (await ed.locator("#field-pool").count()) === 1);
await ed.fill("#field-name", "ui-made");
await ed.selectOption("#field-pool", "archive");
await ed.fill("#field-size", "2");
await ed.click("#dialog-cancel");
await closed();
check("cancelling creates nothing", (await api("/v1/volumes/ui-made")).status === 404);

await ed.click("#create-volume");
await ed.waitForSelector("dialog[open]");
await ed.fill("#field-name", "bad name!");
await ed.fill("#field-size", "2");
await submit();
check("a refused name shows the API's reason", (await alertInDialog()).length > 3);
check("and stays open, with what was typed", (await ed.inputValue("#field-name")) === "bad name!");
await ed.fill("#field-name", "ui-made");
await ed.selectOption("#field-pool", "archive");
await submit();
await closed();
await ed.waitForSelector("dl.facts");
check("then it's created, and opens its page", (await ed.textContent("h1")) === "ui-made");
const made = (await api("/v1/volumes/ui-made")).body;
check("in the pool chosen, at the size chosen", made.pool === "archive" && made.size_bytes === 2 * 2 ** 30, JSON.stringify(made).slice(0, 120));
check("and it says so", (await ed.textContent("#toast")).includes("Created ui-made"));

await ed.click("#grow-volume");
await ed.waitForSelector("dialog[open]");
check("growing starts from its current size", (await ed.inputValue("#field-size")) === "2");
await ed.fill("#field-size", "1");
await submit();
check("shrinking is refused, with a reason", (await alertInDialog()).length > 3);
await ed.fill("#field-size", "3");
await submit();
await closed();
check("growing works", (await api("/v1/volumes/ui-made")).body.size_bytes === 3 * 2 ** 30);

await ed.waitForSelector("#take-snapshot");
await ed.click("#take-snapshot");
await ed.waitForSelector("dialog[open]");
await ed.fill("#field-name", "ui-snap");
await submit();
await closed();
await ed.locator("main td", { hasText: "ui-snap" }).first().waitFor();
check("a snapshot is taken, and listed on its volume", (await api("/v1/snapshots/ui-snap")).body.volume === "ui-made");
await ed.locator("tr", { hasText: "ui-snap" }).locator("button", { hasText: "Clone" }).click();
await ed.waitForSelector("dialog[open]");
await ed.fill("#field-name", "ui-clone");
await submit();
await closed();
await ed.locator("h1", { hasText: "ui-clone" }).waitFor();
const clone = (await api("/v1/volumes/ui-clone")).body;
check("cloning a snapshot makes a new volume from it", clone.source && clone.source.snapshot === "ui-snap");
check("with its own identity", clone.nguid !== made.nguid && clone.uuid !== made.uuid);
await ed.waitForSelector("#clone-volume");
await ed.click("#clone-volume");
await ed.waitForSelector("dialog[open]");
await ed.fill("#field-name", "ui-clone2");
await submit();
await closed();
await ed.locator("h1", { hasText: "ui-clone2" }).waitFor();
check("cloning a volume works too", (await api("/v1/volumes/ui-clone2")).body.source.volume === "ui-clone");

await ed.waitForSelector("#delete-volume");
await ed.click("#delete-volume");
await ed.waitForSelector("dialog[open]");
await ed.screenshot({ path: `${shots}/delete-volume.png` });
check("deleting says what it destroys", (await dialogText()).includes("destroys its data"));
check("and can't be confirmed until its name is typed", await ed.isDisabled("#dialog-submit"));
await ed.fill("#field-confirm", "ui-clone");
check("a similar name isn't enough", await ed.isDisabled("#dialog-submit"));
await ed.keyboard.press("Escape");
await closed();
check("Escape cancels", (await api("/v1/volumes/ui-clone2")).status === 200);
await ed.click("#delete-volume");
await ed.waitForSelector("dialog[open]");
await ed.fill("#field-confirm", "ui-clone2");
await submit();
await closed();
await ed.waitForSelector("#create-volume");
check("with its name, it's deleted", (await api("/v1/volumes/ui-clone2")).status === 404);
check("and goes back to the list", (await ed.textContent("h1")).startsWith("Volumes"));

await openOn("#/volume/vol-0001", "#delete-volume");
check("an exported volume can't be deleted", await ed.isDisabled("#delete-volume"));
check("and says why", (await ed.textContent("#delete-hint")).includes("exported"));
await ed.waitForTimeout(200);

await openOn("#/snapshots", "main tbody tr");
await ed.locator("tr", { hasText: "ui-snap" }).locator("button", { hasText: "Delete" }).click();
await ed.waitForSelector("dialog[open]");
await submit();
await closed();
check("a snapshot can be deleted", (await api("/v1/snapshots/ui-snap")).status === 404);
await visit(ed, "#/volume/ui-clone");
await ed.waitForSelector("#delete-volume");
await ed.click("#delete-volume");
await ed.fill("#field-confirm", "ui-clone");
await submit();
await closed();
await visit(ed, "#/volume/ui-made");
await ed.waitForSelector("#delete-volume");
await ed.click("#delete-volume");
await ed.fill("#field-confirm", "ui-made");
await submit();
await closed();
check("and everything made here is gone again", (await api("/v1/volumes?pool=archive")).body.items.length === 3);

// A refresh underneath mustn't lose what's being typed.
await openOn("#/volumes", "#create-volume");
await ed.click("#create-volume");
await ed.waitForSelector("dialog[open]");
await ed.fill("#field-name", "still typing");
await ed.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
await ed.waitForTimeout(500);
check("a refresh doesn't disturb an open dialog", (await ed.inputValue("#field-name")) === "still typing" && (await ed.locator("dialog[open]").count()) === 1);
await ed.click("#dialog-cancel");
await closed();

// The API says 503 when it's busy, having changed nothing, and it should simply try again.
let busyOnce = true;
await ed.route("**/v1/snapshots", async (r) => {
  if (r.request().method() === "POST" && busyOnce) {
    busyOnce = false;
    await r.fulfill({ status: 503, headers: { "Retry-After": "1" }, json: { error: "too much is in progress" } });
    return;
  }
  await r.continue();
});
await openOn("#/volume/vol-0010", "#take-snapshot");
await ed.click("#take-snapshot");
await ed.waitForSelector("dialog[open]");
await ed.fill("#field-name", "ui-busy");
await submit();
await closed();
check("a busy API is retried, with no error shown", (await api("/v1/snapshots/ui-busy")).status === 200);
await ed.evaluate(async () => { await fetch("/v1/snapshots/ui-busy", { method: "DELETE" }); });
check("nothing went wrong in the browser's script", ed.problems.filter((p) => p.startsWith("exception")).length === 0, ed.problems.join("; "));

console.log("\nWarnings");
const warned = await newPage();
await warned.route("**/v1/system", async (r) => {
  const resp = await r.fetch();
  const body = await resp.json();
  body.warnings = ["LVM on this machine can read exported devices: /dev/sdb."];
  await r.fulfill({ response: resp, json: body });
});
for (const route of ["", "#/volumes", "#/ports"]) {
  await visit(warned, route);
  check(`shown on ${route || "the overview"}`, (await warned.textContent("#warnings")).includes("/dev/sdb"));
}
await warned.screenshot({ path: `${shots}/warning.png` });
await warned.route("**/v1/pools", async (r) => {
  await r.fulfill({ json: [{ name: "gone", type: "lvm-thin", volume_group: "vg", thin_pool: "t", state: "missing", warnings: ["VG name vg is used by 2 VGs"], warning_percent: 80, volumes: 3, snapshots: 0 }] });
});
await visit(warned, "#/pools");
const missing = await warned.textContent("main");
check("a pool that's gone says so, and why", missing.includes("missing") && missing.includes("VG name vg is used") && (await warned.locator(".pill.bad").count()) === 1);
check("with no capacity, it doesn't invent one", missing.includes("unknown"));

console.log("\nSigning in, on a service with a token");
if (tokenBase) {
  const signed = await newPage();
  await visit(signed, "", tokenBase);
  check("it asks for the token", (await signed.textContent("h1")) === "Sign in");
  check("and shows nothing else", (await signed.locator("nav a").count()) === 0 && !(await signed.textContent("main")).includes("Volumes"));
  check("that isn't a failure in the browser's script", signed.problems.filter((p) => p.startsWith("exception")).length === 0, signed.problems.join("; "));
  await signed.screenshot({ path: `${shots}/signin.png` });
  await signed.fill("#token", "wrong");
  await signed.click('button[type="submit"]');
  await signed.waitForSelector(".panel-notice.bad");
  check("a wrong token is refused, saying so", (await signed.textContent("main")).includes("wasn't accepted"));
  await signed.fill("#token", tokenValue);
  await signed.click('button[type="submit"]');
  await signed.waitForSelector(".cards");
  check("the right one works", (await signed.textContent("h1")) === "Overview");
  await visit(signed, "#/volumes", tokenBase);
  await signed.waitForSelector("#create-volume");
  await signed.click("#create-volume");
  await signed.waitForSelector("dialog[open]");
  await signed.fill("#field-name", "by-token");
  await signed.fill("#field-size", "1");
  await signed.click("#dialog-submit");
  await signed.locator("h1", { hasText: "by-token" }).waitFor();
  const made = await signed.evaluate(async (t) => (await fetch("/v1/volumes/by-token", { headers: { Authorization: "Bearer " + t } })).status, tokenValue);
  check("a token can change things", made === 200, String(made));
  await signed.click("#delete-volume");
  await signed.fill("#field-confirm", "by-token");
  await signed.click("#dialog-submit");
  await signed.waitForSelector("#create-volume");
  const removed = await signed.evaluate(async (t) => (await fetch("/v1/volumes/by-token", { headers: { Authorization: "Bearer " + t } })).status, tokenValue);
  check("and delete them", removed === 404, String(removed));
  await visit(signed, "", tokenBase);
  await signed.waitForSelector(".cards");
  check("it isn't in the address", !signed.url().includes(tokenValue));
  await signed.reload();
  await signed.waitForSelector("main h1");
  check("it survives a reload", (await signed.textContent("h1")) === "Overview");
  const stored = await signed.evaluate(() => JSON.stringify(window.localStorage));
  check("and isn't kept beyond this tab", !stored.includes(tokenValue));
  const fresh = await newPage();
  await visit(fresh, "", tokenBase);
  check("a new session has to sign in", (await fresh.textContent("h1")) === "Sign in");
} else {
  bad("no service with a token to test against");
}

console.log("\nSigning in with a password");
const pw = await newPage();
await pw.goto(passwordBase + "/ui/");
await pw.waitForSelector("#username");
check("it asks for a username and password", (await pw.textContent("h1")) === "Sign in" && (await pw.locator("#password").count()) === 1);
check("and shows nothing else", (await pw.locator("nav a").count()) === 0 && !(await pw.textContent("main")).includes("Volumes"));
await pw.screenshot({ path: `${shots}/login.png` });
const direct = await pw.evaluate(async () => (await fetch("/v1/volumes")).status);
check("the API refuses it until then", direct === 401, String(direct));
const tryLogin = async (user, password) => {
  await pw.fill("#username", user);
  await pw.fill("#password", password);
  await pw.click('button[type="submit"]');
};
await tryLogin("admin", "wrong");
await pw.waitForSelector('[role="alert"]');
const wrongPassword = await pw.textContent('[role="alert"]');
check("a wrong password is refused", wrongPassword.includes("wrong username or password"), wrongPassword);
check("and is cleared", (await pw.inputValue("#password")) === "");
await tryLogin("nobody", "correct horse");
await pw.locator('[role="alert"]').waitFor();
check("an unknown user gets the same answer", (await pw.textContent('[role="alert"]')) === wrongPassword);
await tryLogin("admin", "correct horse");
await pw.waitForSelector(".cards");
check("the right one signs in", (await pw.textContent("h1")) === "Overview");
await pw.waitForSelector("#signout");
check("it says who's signed in", (await pw.textContent(".who")) === "admin");
await pw.screenshot({ path: `${shots}/signed-in.png` });
const cookies = await pw.context().cookies();
const cookie = cookies.find((c) => c.name === "nvweaver_session");
check("the session is in a cookie that scripts can't read", cookie && cookie.httpOnly && cookie.sameSite === "Strict", JSON.stringify(cookie));
check("and isn't visible to the page", !(await pw.evaluate(() => document.cookie)).includes("nvweaver_session"));
const stored = await pw.evaluate(() => JSON.stringify(sessionStorage) + JSON.stringify(localStorage));
check("no password or session is kept in the browser's storage", !stored.includes("correct horse") && !stored.includes(cookie.value));
await pw.reload();
await pw.waitForSelector(".cards");
check("it survives a reload", (await pw.textContent("h1")) === "Overview");
await pw.waitForSelector("#signout");

// A hostile page, in the same browser, with the session in its cookies.
await pw.context().route("https://evil.example/**", (r) => r.fulfill({ status: 200, contentType: "text/html", body: "<title>evil</title>" }));
const evil = await pw.context().newPage();
await evil.goto("https://evil.example/");
const stolen = await evil.evaluate(async (api) => {
  const out = {};
  await fetch(api + "/v1/hosts", { method: "POST", mode: "no-cors", credentials: "include", headers: { "Content-Type": "text/plain" }, body: JSON.stringify({ nqn: "nqn.2026-11.net.example:evil-session" }) }).catch(() => {});
  try {
    const r = await fetch(api + "/v1/system", { credentials: "include" });
    out.read = r.status + (await r.text());
  } catch (e) { out.read = "blocked"; }
  return out;
}, passwordBase);
check("a hostile page can't read through the session", stolen.read === "blocked" || !stolen.read.includes("backend"), stolen.read);
await evil.close();
await visit(pw, "#/hosts", passwordBase);
await pw.waitForSelector("main tbody tr");
check("or change anything through it", !(await pw.textContent("main")).includes("evil-session"));

await visit(pw, "#/volumes", passwordBase);
await pw.waitForSelector("#create-volume");
await pw.click("#create-volume");
await pw.fill("#field-name", "by-session");
await pw.fill("#field-size", "1");
await pw.click("#dialog-submit");
await pw.locator("h1", { hasText: "by-session" }).waitFor();
const bySession = await pw.evaluate(async () => (await fetch("/v1/volumes/by-session")).status);
check("a session can change things", bySession === 200, String(bySession));
await pw.click("#delete-volume");
await pw.fill("#field-confirm", "by-session");
await pw.click("#dialog-submit");
await pw.waitForSelector("#create-volume");
const gone = await pw.evaluate(async () => (await fetch("/v1/volumes/by-session")).status);
check("and delete them", gone === 404, String(gone));

await pw.waitForSelector("#signout");
await pw.click("#signout");
await pw.waitForSelector("#username");
check("signing out goes back to the form", (await pw.textContent("h1")) === "Sign in");
await pw.context().addCookies([cookie]);
const after = await pw.evaluate(async () => (await fetch("/v1/volumes")).status);
check("and the old session no longer works, even if it's kept", after === 401, String(after));
check("nothing went wrong in the browser's script", pw.problems.filter((p) => p.startsWith("exception")).length === 0, pw.problems.join("; "));

console.log("\nCan't reach the service, and narrow screens");
const phone = await newPage({ viewport: { width: 390, height: 800 } });
await visit(phone, "#/volumes");
const overflow = await phone.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
check("a phone-sized screen doesn't scroll sideways", !overflow);
const navHeight = await phone.evaluate(() => document.querySelector(".top").getBoundingClientRect().height);
check("and its navigation doesn't take over", navHeight < 120, `the header is ${navHeight}px tall`);
await phone.screenshot({ path: `${shots}/phone.png` });
const offline = await newPage();
await visit(offline, "#/volumes");
await offline.route("**/v1/**", (r) => r.abort());
await offline.waitForSelector("main tbody tr");
await offline.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
let saysSo = false;
for (let i = 0; i < 50 && !saysSo; i++) {
  saysSo = (await offline.textContent("#status")).startsWith("Can't reach");
  if (!saysSo) await new Promise((done) => setTimeout(done, 100));
}
check("losing the service says so", saysSo, await offline.textContent("#status"));
check("and keeps what's on the screen", (await offline.locator("main tbody tr").count()) === 16);

await browser.close();
console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);

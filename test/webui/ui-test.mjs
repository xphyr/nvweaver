// Drives the Web UI in a real browser, against test/webui-demo. Takes the demo's address, and a directory for screenshots.
// Run through test/webui-e2e.sh.
import { chromium } from "playwright";

const base = process.argv[2];
const shots = process.argv[3];
const tokenBase = process.argv[4];
const tokenValue = process.argv[5];
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

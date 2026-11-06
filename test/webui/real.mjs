// One step of driving the web interface against a real target, for test/webui-real-e2e.sh, which does what only a real host can between steps.
// A step takes its name and what it needs, and prints checks, and KEY=VALUE lines for the script. The session is kept between steps in a file.
import { chromium } from "playwright";
import { existsSync } from "node:fs";

const [step, base, ...args] = process.argv.slice(2);
const stateFile = "/work/state.json";
const VG = "nvwlab";
const NQN = "nqn.2026-11.net.example:real";
let fail = 0;
const check = (name, cond, why) => {
  console.log(cond ? `  ok    ${name}` : `  FAIL  ${name}`);
  if (!cond) { fail++; if (why !== undefined) console.log(`        ${why}`); }
};

const browser = await chromium.launch();
// The certificate is a test's own, as trusting a CA in a browser isn't what's being tested.
const context = await browser.newContext({ ignoreHTTPSErrors: true, storageState: existsSync(stateFile) ? stateFile : undefined });
const page = await context.newPage();
const problems = [];
page.on("pageerror", (e) => problems.push(e.message));
const open = async (route, selector) => { await page.goto(`${base}/ui/${route}`); await page.waitForSelector(selector); };
const dialog = async (opener) => { await page.click(opener); await page.waitForSelector("dialog[open]"); };
const submit = async () => { await page.click("#dialog-submit"); await page.waitForSelector("dialog", { state: "detached", timeout: 60000 }); };
const row = (text) => page.locator("main tbody tr", { hasText: text });
const api = (path) => page.evaluate(async (p) => { const r = await fetch(p); return { status: r.status, body: await r.json().catch(() => null) }; }, path);

const steps = {
  async signin() {
    await page.goto(`${base}/ui/`);
    await page.waitForSelector("#username");
    check("over real HTTPS, it asks for a password", base.startsWith("https://"));
    await page.fill("#username", args[0]);
    await page.fill("#password", "wrong");
    await page.click('button[type="submit"]');
    await page.waitForSelector('[role="alert"]');
    check("a wrong one is refused", (await page.textContent('[role="alert"]')).includes("wrong username or password"));
    await page.fill("#password", args[1]);
    await page.click('button[type="submit"]');
    await page.waitForSelector(".cards");
    check("the right one signs in, to the real target", (await page.textContent("main")).includes(args[2]));
    const cookie = (await context.cookies()).find((c) => c.name === "nvweaver_session");
    check("its cookie is only sent over HTTPS, and scripts can't read it", cookie && cookie.secure && cookie.httpOnly && cookie.sameSite === "Strict", JSON.stringify(cookie));
  },
  async setup() {
    await open("#/pools", "#adopt-pool");
    await dialog("#adopt-pool");
    await page.fill("#field-name", "real");
    await page.selectOption("#field-type", "lvm-thin");
    await page.fill("#field-vg", VG);
    await page.fill("#field-thin", "pool0");
    await submit();
    await row("real").waitFor();
    const pool = (await api("/v1/pools/real")).body;
    check("a real LVM thin pool is adopted", pool.state === "online" && pool.capacity.total_bytes > 0, JSON.stringify(pool).slice(0, 150));
    await open("#/ports", "#add-port");
    await dialog("#add-port");
    await page.fill("#field-address", args[0]);
    await submit();
    await row(args[0]).waitFor();
    check("a port is added, listening for real", (await api("/v1/ports")).body.length === 1);
    await open("#/hosts", "#add-host");
    await dialog("#add-host");
    await page.fill("#field-nqn", args[1]);
    await submit();
    await row(args[1]).waitFor();
    check("the real host is added", (await api("/v1/hosts")).body.length === 1);
  },
  async export() {
    await open("#/volumes", "#create-volume");
    await dialog("#create-volume");
    await page.fill("#field-name", "real-vol");
    await page.fill("#field-size", "512");
    await page.selectOption('[aria-label="Size unit"]', "MiB");
    await submit();
    await page.waitForSelector("#export-volume");
    check("a thin volume is created", (await api("/v1/volumes/real-vol")).body.size_bytes === 512 * 2 ** 20);
    await dialog("#export-volume");
    await page.fill("#field-nqn", NQN);
    await page.check(`#field-hosts input[value="${args[0]}"]`);
    await submit();
    await page.waitForSelector("#unexport-volume");
    const command = await page.textContent("code.command");
    check("it's exported, with a command to connect", command.includes(NQN), command);
    const sub = (await api("/v1/subsystems/" + encodeURIComponent(NQN))).body;
    check("and is really being served", sub.namespaces[0].live_enabled === true, JSON.stringify(sub.namespaces));
    console.log(`CONNECT=${command}`);
    console.log(`NQN=${NQN}`);
  },
  async usage() {
    await open("#/volume/real-vol", "dl.facts");
    const text = await page.textContent("dl.facts");
    const used = (await api("/v1/volumes/real-vol/usage")).body;
    check("the page shows what's really allocated", /MiB of 512 MiB/.test(text), text.slice(0, 80));
    // LVM gives this as a percentage to two places, which can round it down.
    const written = 16 * 2 ** 20;
    check("about what the host wrote", used.used_bytes >= written - used.size_bytes / 10000 && used.used_bytes < 2 * written, JSON.stringify(used));
  },
  async clone() {
    await open("#/volume/real-vol", "#take-snapshot");
    await dialog("#take-snapshot");
    await page.fill("#field-name", "real-snap");
    await submit();
    await row("real-snap").waitFor();
    check("a snapshot is taken, while it's exported", (await api("/v1/snapshots/real-snap")).status === 200);
    await row("real-snap").locator("button", { hasText: "Clone" }).click();
    await page.waitForSelector("dialog[open]");
    await page.fill("#field-name", "real-clone");
    await submit();
    await page.waitForSelector("#export-volume");
    await dialog("#export-volume");
    await page.check('#field-mode input[value="existing"]');
    await page.selectOption("#field-existing", NQN);
    await submit();
    await page.waitForSelector("#unexport-volume");
    const ns = (await api("/v1/volumes/real-clone")).body.mapped_to;
    check("its clone joins the subsystem", ns && ns.subsystem === NQN && ns.nsid === 2, JSON.stringify(ns));
  },
  async deny() {
    await open("#/subsystems/" + encodeURIComponent(NQN), "#allow-host");
    await row(args[0]).locator("button", { hasText: "Remove" }).click();
    await page.waitForSelector("dialog[open]");
    await submit();
    check("the host is removed", (await api("/v1/subsystems/" + encodeURIComponent(NQN))).body.allowed_hosts.length === 0);
  },
  async cleanup() {
    for (const name of ["real-clone", "real-vol"]) {
      await open("#/volume/" + name, "#unexport-volume");
      await dialog("#unexport-volume");
      await submit();
      await page.waitForSelector("#export-volume");
    }
    await open("#/volume/real-vol", "#delete-volume");
    check("un-exported volumes can be deleted", !(await page.isDisabled("#delete-volume")));
    await open("#/snapshots", "main tbody tr");
    await row("real-snap").locator("button", { hasText: "Delete" }).click();
    await page.waitForSelector("dialog[open]");
    await submit();
    for (const name of ["real-clone", "real-vol"]) {
      await open("#/volume/" + name, "#delete-volume");
      await dialog("#delete-volume");
      await page.fill("#field-confirm", name);
      await submit();
      await page.waitForSelector("#create-volume");
    }
    // Un-exporting the last volume offered to remove the subsystem as well, and that's what it does by default.
    check("the subsystem went with its last volume", (await api("/v1/subsystems/" + encodeURIComponent(NQN))).status === 404);
    await open("#/hosts", "#add-host");
    await row(args[0]).locator("button", { hasText: "Delete" }).click();
    await page.waitForSelector("dialog[open]");
    await submit();
    await open("#/ports", "main tbody tr");
    await page.locator("main tbody tr button", { hasText: "Delete" }).click();
    await page.waitForSelector("dialog[open]");
    await submit();
    await open("#/pools", "main tbody tr");
    await row("real").locator("button", { hasText: "Forget" }).click();
    await page.waitForSelector("dialog[open]");
    await submit();
    const sys = (await api("/v1/system")).body;
    check("everything can be taken apart from the interface", sys.volumes + sys.snapshots + sys.subsystems + sys.hosts + sys.ports + sys.pools === 0, JSON.stringify(sys).slice(0, 200));
    await page.click("#signout");
    await page.waitForSelector("#username");
    check("and signing out works", (await api("/v1/system")).status === 401);
  },
};

try {
  await steps[step]();
} catch (e) {
  check(`step ${step} finished`, false, e.message.split("\n")[0]);
  await page.screenshot({ path: `/work/failed-${step}.png` }).catch(() => {});
}
check(`nothing went wrong in the browser's script, in ${step}`, problems.length === 0, problems.join("; "));
await context.storageState({ path: stateFile });
await browser.close();
process.exit(fail ? 1 : 0);

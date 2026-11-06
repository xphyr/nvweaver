// The nvweaver interface: read-only screens over the public API, with no dependencies and no build step.
// Everything from the API reaches the page through textContent, by way of el(), so a name can't become markup.

const REFRESH_MS = 10000;
const TABLE_PAGE = 100;
const TOKEN_KEY = "nvweaver-token";

// --- The API ---

class Unauthorized extends Error {}

function token() {
  try { return sessionStorage.getItem(TOKEN_KEY) || ""; } catch { return ""; }
}

function saveToken(value) {
  try {
    if (value) sessionStorage.setItem(TOKEN_KEY, value);
    else sessionStorage.removeItem(TOKEN_KEY);
  } catch { /* A private window can refuse this: the token then lasts until the page is reloaded. */ }
  memoryToken = value;
}

let memoryToken = "";

async function api(path, { retries = 5 } = {}) {
  const headers = { Accept: "application/json" };
  const t = memoryToken || token();
  if (t) headers.Authorization = "Bearer " + t;
  const resp = await fetch(path, { headers, cache: "no-store" });
  if (resp.status === 401) throw new Unauthorized();
  if (resp.status === 503 && retries > 0) {
    await new Promise((done) => setTimeout(done, 1000));
    return api(path, { retries: retries - 1 });
  }
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({}));
    throw new Error(body.error || `The API answered with status ${resp.status}`);
  }
  return resp.json();
}

// --- Building the page ---

// el makes an element. Strings become text, never markup.
function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? "" : String(value));
  }
  for (const child of children.flat(Infinity)) {
    if (child === undefined || child === null || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function humanSize(bytes) {
  if (bytes === undefined || bytes === null) return "-";
  const units = [["PiB", 2 ** 50], ["TiB", 2 ** 40], ["GiB", 2 ** 30], ["MiB", 2 ** 20], ["KiB", 2 ** 10]];
  for (const [name, size] of units) {
    if (bytes >= size) {
      const value = bytes / size;
      return (Number.isInteger(value) ? value : value.toFixed(1)) + " " + name;
    }
  }
  return bytes + " B";
}

function humanTime(iso) {
  const when = new Date(iso);
  return Number.isNaN(when.getTime()) ? iso : when.toLocaleString();
}

const pill = (text, kind) => el("span", { class: "pill " + (kind || "") }, text);
const stateKind = (state) => (state === "online" ? "ok" : "bad");
const hash = (...parts) => "#/" + parts.map(encodeURIComponent).join("/");
const link = (text, ...parts) => el("a", { href: hash(...parts) }, text);
const mono = (text) => el("span", { class: "mono" }, text);
const labelsText = (labels) => Object.entries(labels || {}).map(([k, v]) => `${k}=${v}`).join(", ");

function usageBar(usage) {
  if (!usage) return el("span", { class: "muted" }, "The storage can't say at the moment.");
  const percent = usage.size_bytes ? (100 * usage.used_bytes) / usage.size_bytes : 0;
  const fill = el("div");
  fill.style.width = Math.min(percent, 100) + "%";
  return el("div", {},
    `${humanSize(usage.used_bytes)} of ${humanSize(usage.size_bytes)} (${percent < 1 && percent > 0 ? "under 1" : percent.toFixed(0)}%)`,
    el("div", { class: "bar", role: "img", "aria-label": `${percent.toFixed(0)} percent allocated` }, fill),
    el("span", { class: "sub muted" }, "Includes what's shared with its snapshots and clones, so a pool's volumes can add up to more than the pool has used."));
}

function capacityBar(capacity, warningPercent) {
  if (!capacity) return el("span", { class: "muted" }, "unknown");
  const percent = capacity.used_percent || 0;
  const kind = percent >= 95 ? "bad" : percent >= (warningPercent || 80) ? "warn" : "";
  const fill = el("div");
  fill.style.width = Math.min(percent, 100) + "%";
  return el("div", {},
    `${humanSize(capacity.used_bytes)} of ${humanSize(capacity.total_bytes)} (${percent.toFixed(0)}%)`,
    el("div", { class: "bar " + kind, role: "img", "aria-label": `${percent.toFixed(0)} percent used` }, fill),
    capacity.provisioned_bytes > capacity.total_bytes && el("span", { class: "sub" }, `${humanSize(capacity.provisioned_bytes)} promised to volumes`));
}

// table draws rows, with a filter box and a page at a time, as there can be thousands.
// A column is { title, cell(row), num, text(row) }, where text says what the filter searches.
function table(columns, rows, { empty, filter = true, state }) {
  const wrap = el("div", {});
  const view = state || { text: "", shown: TABLE_PAGE };
  const haystack = (row) => columns.map((c) => (c.text ? c.text(row) : "")).join(" ").toLowerCase();
  const body = el("div", {});
  function draw() {
    const needle = view.text.trim().toLowerCase();
    const matching = needle ? rows.filter((row) => haystack(row).includes(needle)) : rows;
    const shown = matching.slice(0, view.shown);
    count.textContent = needle ? `${matching.length} of ${rows.length}` : `${rows.length}`;
    if (!rows.length) { body.replaceChildren(el("div", { class: "table-wrap" }, el("div", { class: "empty" }, empty))); return; }
    if (!matching.length) { body.replaceChildren(el("div", { class: "table-wrap" }, el("div", { class: "empty" }, "Nothing matches that."))); return; }
    body.replaceChildren(el("div", { class: "table-wrap" }, el("table", {},
      el("thead", {}, el("tr", {}, columns.map((c) => el("th", { scope: "col", class: c.num ? "num" : "" }, c.title)))),
      el("tbody", {}, shown.map((row) => el("tr", {}, columns.map((c) => el("td", { class: c.num ? "num" : "" }, c.cell(row)))))))));
    if (matching.length > shown.length) {
      body.append(el("div", { class: "more" },
        el("span", { class: "count" }, `Showing ${shown.length} of ${matching.length}`),
        el("button", { onclick: () => { view.shown += TABLE_PAGE; draw(); } }, "Show more")));
    }
  }
  const count = el("span", { class: "count" });
  if (filter) {
    wrap.append(el("div", { class: "toolbar" },
      el("input", { type: "search", placeholder: "Filter", "aria-label": "Filter", value: view.text,
        oninput: (e) => { view.text = e.target.value; view.shown = TABLE_PAGE; draw(); } }), count));
  }
  wrap.append(body);
  draw();
  return wrap;
}

// --- Screens. Each returns what to show, and is called again to refresh. ---

// Screen state that should survive a refresh, such as what's typed in a filter.
let screenState = {};
const stateOf = (key) => (screenState[key] ||= { text: "", shown: TABLE_PAGE });

async function dashboard() {
  const [sys, pools] = await Promise.all([api("/v1/system"), api("/v1/pools")]);
  const card = (label, value, href) => el(href ? "a" : "div", { class: "card", href }, el("div", { class: "value" }, value), el("div", { class: "label" }, label));
  return [
    el("h1", {}, "Overview"),
    !sys.target_ready && el("div", { class: "panel-notice bad" }, `The ${sys.backend} target isn't ready, so hosts can't be served.`),
    el("div", { class: "cards" },
      card("Pools", sys.pools, "#/pools"), card("Volumes", sys.volumes, "#/volumes"), card("Snapshots", sys.snapshots, "#/snapshots"),
      card("Subsystems", sys.subsystems, "#/subsystems"), card("Hosts", sys.hosts, "#/hosts"), card("Ports", sys.ports, "#/ports")),
    el("h2", {}, "Pools"),
    poolTable(pools, { filter: false }),
    el("h2", {}, "This service"),
    el("dl", { class: "facts" },
      fact("Version", sys.version), fact("Target", sys.backend),
      fact("Ready", sys.target_ready ? pill("Yes", "ok") : pill("No", "bad")),
      fact("Kernel", sys.kernel), fact("Transports", (sys.capabilities.transports || []).join(", ")),
      fact("Pool types", (sys.pool_types || []).join(", "))),
  ];
}

function fact(name, value) {
  return [el("dt", {}, name), el("dd", {}, value)];
}

function poolTable(pools, options = {}) {
  return table([
    { title: "Name", cell: (p) => el("strong", {}, p.name), text: (p) => p.name },
    { title: "State", cell: (p) => pill(p.state, stateKind(p.state)), text: (p) => p.state },
    { title: "Type", cell: (p) => p.type, text: (p) => p.type },
    { title: "Where", cell: (p) => mono(p.volume_group ? `${p.volume_group}/${p.thin_pool}` : p.path), text: (p) => `${p.volume_group || ""} ${p.path || ""}` },
    { title: "Space", cell: (p) => capacityBar(p.capacity, p.warning_percent) },
    { title: "Volumes", num: true, cell: (p) => link(p.volumes, "volumes", p.name) },
    { title: "Snapshots", num: true, cell: (p) => p.snapshots },
    { title: "Labels", cell: (p) => labelsText(p.labels) || "-", text: (p) => labelsText(p.labels) },
  ], pools, { empty: "No pools yet. Adopt one with: nvweaver pool create", ...options, filter: options.filter ?? pools.length > 5, state: stateOf("pools") });
}

async function pools() {
  const list = await api("/v1/pools");
  const troubled = list.flatMap((p) => (p.warnings || []).map((w) => `${p.name}: ${w}`));
  return [el("h1", {}, "Pools"), troubled.map((w) => el("div", { class: "panel-notice warn" }, w)), poolTable(list)];
}

// A paged list from the API, keeping what's loaded so far across refreshes.
async function pagedList(path, query) {
  const key = path + "?" + query;
  const kept = screenState[key] ||= { pages: 1 };
  let items = [];
  let after = "";
  for (let page = 0; page < kept.pages; page++) {
    const params = new URLSearchParams(query);
    if (after) params.set("after", after);
    const result = await api(`${path}?${params}`);
    items = items.concat(result.items || []);
    after = result.next_after || "";
    if (!after) break;
  }
  return { items, more: Boolean(after), loadMore: () => { kept.pages++; rerender(); } };
}

// A choice of pool, shown only if there's a choice to make.
function poolPicker(pools, current) {
  if (pools.length < 2) return null;
  return el("label", { class: "picker" }, "Pool ",
    el("select", { onchange: (e) => { location.hash = e.target.value ? hash("volumes", e.target.value) : "#/volumes"; } },
      el("option", { value: "", selected: !current }, "All pools"),
      pools.map((p) => el("option", { value: p.name, selected: p.name === current }, `${p.name} (${p.volumes})`))));
}

async function volumes(pool) {
  const [{ items, more, loadMore }, allPools] = await Promise.all([
    pagedList("/v1/volumes", pool ? `pool=${encodeURIComponent(pool)}` : ""),
    api("/v1/pools"),
  ]);
  const mapped = items.filter((v) => v.mapped_to).length;
  return [
    el("h1", {}, pool ? `Volumes in ${pool}` : "Volumes"),
    el("div", { class: "toolbar" }, poolPicker(allPools, pool)),
    table([
      { title: "Name", cell: (v) => [el("strong", {}, link(v.name, "volume", v.name)), v.source && el("span", { class: "sub" }, `clone of ${v.source.snapshot || v.source.volume}`)], text: (v) => v.name },
      { title: "Pool", cell: (v) => link(v.pool, "volumes", v.pool), text: (v) => v.pool },
      { title: "Size", num: true, cell: (v) => humanSize(v.size_bytes) },
      { title: "Exported", cell: (v) => (v.mapped_to ? [link(v.mapped_to.subsystem, "subsystems", v.mapped_to.subsystem), el("span", { class: "sub" }, `namespace ${v.mapped_to.nsid}`)] : pill("Not exported", "quiet")), text: (v) => (v.mapped_to ? v.mapped_to.subsystem : "") },
      { title: "NGUID", cell: (v) => mono(v.nguid), text: (v) => v.nguid },
      { title: "Labels", cell: (v) => labelsText(v.labels) || "-", text: (v) => labelsText(v.labels) },
    ], items, { empty: "No volumes yet. Create one with: nvweaver volume create", state: stateOf("volumes" + pool) }),
    el("p", { class: "count" }, `${mapped} of ${items.length} loaded are exported.`),
    more && el("div", { class: "more" }, el("button", { onclick: loadMore }, "Load more")),
  ];
}

async function volume(name) {
  const path = "/v1/volumes/" + encodeURIComponent(name);
  const [v, usage, snaps] = await Promise.all([
    api(path),
    // If the storage can't say, the rest of the page is still worth showing.
    api(path + "/usage", { retries: 0 }).catch((err) => { if (err instanceof Unauthorized) throw err; return null; }),
    pagedList("/v1/snapshots", `volume=${encodeURIComponent(name)}`),
  ]);
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/volumes" }, "Volumes")),
    el("h1", {}, v.name),
    el("dl", { class: "facts" },
      fact("Space", usageBar(usage)),
      fact("Pool", link(v.pool, "volumes", v.pool)),
      fact("Exported", v.mapped_to ? [link(v.mapped_to.subsystem, "subsystems", v.mapped_to.subsystem), ` as namespace ${v.mapped_to.nsid}`] : pill("Not exported", "quiet")),
      v.source && fact("Cloned from", v.source.snapshot ? `snapshot ${v.source.snapshot}` : link(v.source.volume, "volume", v.source.volume)),
      fact("Created", humanTime(v.created_at)),
      fact("UUID", mono(v.uuid)), fact("NGUID", mono(v.nguid)),
      fact("Backed by", mono(v.device_path)),
      fact("Labels", labelsText(v.labels) || "-")),
    el("h2", {}, "Snapshots"),
    table([
      { title: "Name", cell: (s) => el("strong", {}, s.name), text: (s) => s.name },
      { title: "Size", num: true, cell: (s) => humanSize(s.size_bytes) },
      { title: "Taken", cell: (s) => humanTime(s.created_at) },
    ], snaps.items, { filter: snaps.items.length > 5, empty: `No snapshots of this volume. Take one with: nvweaver snapshot create NAME --volume ${v.name}`, state: stateOf("volume-snapshots") }),
    snaps.more && el("div", { class: "more" }, el("button", { onclick: snaps.loadMore }, "Load more")),
  ];
}

async function snapshots() {
  const { items, more, loadMore } = await pagedList("/v1/snapshots", "");
  return [
    el("h1", {}, "Snapshots"),
    table([
      { title: "Name", cell: (s) => el("strong", {}, s.name), text: (s) => s.name },
      { title: "Volume", cell: (s) => link(s.volume, "volume", s.volume), text: (s) => s.volume },
      { title: "Pool", cell: (s) => link(s.pool, "volumes", s.pool), text: (s) => s.pool },
      { title: "Size", num: true, cell: (s) => humanSize(s.size_bytes) },
      { title: "Taken", cell: (s) => humanTime(s.created_at) },
    ], items, { empty: "No snapshots yet. Take one with: nvweaver snapshot create", state: stateOf("snapshots") }),
    more && el("div", { class: "more" }, el("button", { onclick: loadMore }, "Load more")),
  ];
}

async function subsystems() {
  const list = await api("/v1/subsystems");
  return [
    el("h1", {}, "Subsystems"),
    table([
      { title: "NQN", cell: (s) => link(s.nqn, "subsystems", s.nqn), text: (s) => s.nqn },
      { title: "Who can connect", cell: (s) => (s.allow_any_host ? pill("Any host", "warn") : `${(s.allowed_hosts || []).length} allowed`), text: (s) => (s.allowed_hosts || []).join(" ") },
      { title: "Ports", cell: (s) => (s.ports || []).join(", ") || "-" },
      { title: "Namespaces", num: true, cell: (s) => (s.namespaces || []).length },
      { title: "Serial", cell: (s) => mono(s.serial), text: (s) => s.serial },
    ], list, { empty: "No subsystems yet. Create one with: nvweaver subsystem create", state: stateOf("subsystems") }),
  ];
}

async function subsystem(nqn) {
  const s = await api("/v1/subsystems/" + encodeURIComponent(nqn));
  const notServed = (s.namespaces || []).filter((n) => n.enabled && !n.live_enabled);
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/subsystems" }, "Subsystems")),
    el("h1", {}, mono(s.nqn)),
    notServed.length > 0 && el("div", { class: "panel-notice bad" }, `Namespace ${notServed.map((n) => n.nsid).join(", ")} should be served, but isn't.`),
    el("dl", { class: "facts" },
      fact("Serial", mono(s.serial)), fact("Model", s.model),
      s.passthru_device && fact("Passes through", mono(s.passthru_device)),
      fact("Who can connect", s.allow_any_host ? pill("Any host", "warn") : (s.allowed_hosts || []).length ? (s.allowed_hosts).map((h) => el("div", {}, mono(h))) : "Nobody yet"),
      fact("Ports", (s.ports || []).length ? (s.ports).map((id) => el("div", {}, link(`Port ${id}`, "ports"))) : "None, so hosts can't reach it")),
    el("h2", {}, "Namespaces"),
    table([
      { title: "NSID", cell: (n) => n.nsid },
      { title: "Serving", cell: (n) => (n.live_enabled ? pill("Live", "ok") : n.enabled ? pill("Not serving", "bad") : pill("Disabled", "quiet")) },
      { title: "Size", num: true, cell: (n) => humanSize(n.size_bytes) },
      { title: "Backed by", cell: (n) => [n.volume ? link(n.volume, "volume", n.volume) : null, el("span", { class: n.volume ? "sub" : "" }, mono(n.device_path))] },
      { title: "NGUID", cell: (n) => mono(n.nguid) },
    ], s.namespaces || [], { filter: false, empty: "Nothing exported yet. Add something with: nvweaver namespace create" }),
  ];
}

async function hosts() {
  const list = await api("/v1/hosts");
  return [
    el("h1", {}, "Hosts"),
    table([
      { title: "NQN", cell: (h) => mono(h.nqn), text: (h) => h.nqn },
      { title: "Authentication", cell: (h) => (h.has_dhchap_key ? pill(h.has_dhchap_ctrl_key ? "DH-HMAC-CHAP, both ways" : "DH-HMAC-CHAP", "ok") : pill("None", "quiet")) },
      { title: "Hash", cell: (h) => h.dhchap_hash || "-" },
      { title: "DH group", cell: (h) => h.dhchap_dhgroup || "-" },
    ], list, { empty: "No hosts yet. Add one with: nvweaver host create", state: stateOf("hosts") }),
  ];
}

async function ports() {
  const list = await api("/v1/ports");
  return [
    el("h1", {}, "Ports"),
    table([
      { title: "ID", cell: (p) => p.id },
      { title: "Transport", cell: (p) => p.transport },
      { title: "Address", cell: (p) => mono(p.address), text: (p) => p.address },
      { title: "Service", cell: (p) => p.service },
    ], list, { filter: false, empty: "No ports yet. Add one with: nvweaver port create" }),
  ];
}

const NAV = [["Overview", ""], ["Pools", "pools"], ["Volumes", "volumes"], ["Snapshots", "snapshots"], ["Subsystems", "subsystems"], ["Hosts", "hosts"], ["Ports", "ports"]];

function route() {
  const [screen, ...rest] = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  switch (screen) {
    case "": return { nav: "", draw: dashboard };
    case "pools": return { nav: "pools", draw: pools };
    case "volumes": return { nav: "volumes", draw: () => volumes(rest[0]) };
    case "volume": return { nav: "volumes", draw: () => volume(rest[0]) };
    case "snapshots": return { nav: "snapshots", draw: snapshots };
    case "subsystems": return { nav: "subsystems", draw: rest[0] ? () => subsystem(rest[0]) : subsystems };
    case "hosts": return { nav: "hosts", draw: hosts };
    case "ports": return { nav: "ports", draw: ports };
    default: return { nav: null, draw: async () => [el("h1", {}, "Nothing here"), el("p", {}, el("a", { href: "#/" }, "Back to the overview"))] };
  }
}

// --- Drawing, refreshing and signing in ---

const main = document.getElementById("main");
const nav = document.getElementById("nav");
const statusLine = document.getElementById("status");
const warnings = document.getElementById("warnings");
const account = document.getElementById("account");
let generation = 0;

function drawNav(current) {
  nav.replaceChildren(...NAV.map(([label, target]) => el("a", { href: "#/" + target, "aria-current": target === current ? "page" : undefined }, label)));
}

function clearForSignIn() {
  nav.replaceChildren();
  warnings.replaceChildren();
  statusLine.textContent = "";
  account.replaceChildren();
}

function passwordForm(message) {
  clearForSignIn();
  const user = el("input", { type: "text", id: "username", autocomplete: "username", autocapitalize: "none", spellcheck: "false", required: true });
  const password = el("input", { type: "password", id: "password", autocomplete: "current-password", required: true });
  const notice = el("div", {}, message && el("div", { class: "panel-notice bad", role: "alert" }, message));
  const button = el("button", { class: "primary", type: "submit" }, "Sign in");
  main.replaceChildren(el("form", { class: "signin", onsubmit: async (e) => {
    e.preventDefault();
    button.disabled = true;
    try {
      const resp = await fetch("/v1/session", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: user.value, password: password.value }),
      });
      if (resp.ok) {
        rerender();
        return;
      }
      const body = await resp.json().catch(() => ({}));
      password.value = "";
      notice.replaceChildren(el("div", { class: "panel-notice bad", role: "alert" }, body.error || `The service answered with status ${resp.status}`));
      password.focus();
    } catch (err) {
      notice.replaceChildren(el("div", { class: "panel-notice bad", role: "alert" }, "Can't reach the service: " + err.message));
    }
    button.disabled = false;
  } },
    el("h1", {}, "Sign in"), notice,
    el("label", { for: "username" }, "Username"), user,
    el("label", { for: "password" }, "Password"), password,
    button));
  user.focus();
}

async function signOut() {
  await fetch("/v1/session", { method: "DELETE" }).catch(() => {});
  passwordForm();
}

function signIn(message) {
  clearForSignIn();
  const input = el("input", { type: "password", id: "token", autocomplete: "off", required: true });
  main.replaceChildren(el("form", { class: "signin", onsubmit: (e) => {
    e.preventDefault();
    saveToken(input.value.trim());
    rerender();
  } },
    el("h1", {}, "Sign in"),
    el("p", { class: "muted" }, "This service needs its API token, which is NVWEAVER_API_TOKEN where it runs."),
    message && el("div", { class: "panel-notice bad" }, message),
    el("label", { for: "token" }, "API token"), input,
    el("button", { class: "primary", type: "submit" }, "Sign in")));
  input.focus();
}

// A warning about the machine is shown on every screen, as it's easily missed otherwise.
async function drawWarnings() {
  const sys = await api("/v1/system");
  warnings.replaceChildren(...(sys.warnings || []).map((w) => el("p", { class: "notice warn" }, w)));
}

// Who's signed in, with a way to leave. Asking each time also notices a session ending.
let wasSignedIn = false;

async function drawAccount() {
  const how = await api("/v1/session").catch(() => null);
  if (!how || how.method !== "password" || !how.username) {
    account.replaceChildren();
    return;
  }
  wasSignedIn = true;
  account.replaceChildren(el("span", { class: "who" }, how.username), el("button", { id: "signout", onclick: signOut }, "Sign out"));
}

async function rerender() {
  const mine = ++generation;
  const { nav: current, draw } = route();
  try {
    const [content] = await Promise.all([draw(), drawWarnings()]);
    if (mine !== generation) return;
    drawNav(current);
    // Redrawing shouldn't lose someone's place, or what they're typing.
    const active = document.activeElement;
    const filter = active && active.type === "search" ? { start: active.selectionStart } : null;
    main.replaceChildren(...[content].flat(Infinity).filter((n) => n instanceof Node));
    if (filter) {
      const box = main.querySelector('input[type="search"]');
      if (box) { box.focus(); box.setSelectionRange(filter.start, filter.start); }
    }
    statusLine.textContent = "Updated " + new Date().toLocaleTimeString();
    drawAccount();
  } catch (err) {
    if (mine !== generation) return;
    if (err instanceof Unauthorized) {
      const how = await api("/v1/session").catch(() => ({ method: "token" }));
      if (how.method === "password") {
        passwordForm(wasSignedIn ? "You've been signed out. Sign in again to carry on." : "");
        wasSignedIn = false;
        return;
      }
      const had = memoryToken || token();
      saveToken("");
      signIn(had ? "That token wasn't accepted." : "");
      return;
    }
    drawNav(current);
    // Keep what's on the screen if it's only a refresh that failed: it's still the best information there is.
    statusLine.textContent = "Can't reach the service: " + err.message;
    if (!main.childElementCount) main.replaceChildren(el("div", { class: "panel-notice bad" }, err.message));
  }
}

window.addEventListener("hashchange", () => {
  screenState = {};
  main.replaceChildren();
  rerender();
  main.focus({ preventScroll: true });
});
// Not while someone's signing in, which would throw away what they've typed.
const signingIn = () => document.querySelector("form.signin");
setInterval(() => {
  if (!document.hidden && !signingIn()) rerender();
}, REFRESH_MS);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && !signingIn()) rerender();
});

rerender();

// The nvweaver interface: read-only screens over the public API, with no dependencies and no build step.
// Everything from the API reaches the page through textContent, by way of el(), so a name can't become markup.

const REFRESH_MS = 10000;
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

// send changes something. The API says 503 only having changed nothing, so it's safe to try again, for a while.
async function send(method, path, body, tries = 30) {
  const headers = {};
  const t = memoryToken || token();
  if (t) headers.Authorization = "Bearer " + t;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const resp = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  if (resp.status === 401) throw new Unauthorized();
  if (resp.status === 503 && tries > 0) {
    const wait = Number(resp.headers.get("Retry-After")) || 1;
    await new Promise((done) => setTimeout(done, wait * 1000));
    return send(method, path, body, tries - 1);
  }
  const answer = resp.status === 204 ? null : await resp.json().catch(() => null);
  if (!resp.ok) throw new Error((answer && answer.error) || `The API answered with status ${resp.status}`);
  return answer;
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

const PAGE_SIZES = [10, 25, 50, 100, 250];
const DEFAULT_PAGE_SIZE = 25;
const PAGE_SIZE_KEY = "nvweaver-page-size";

// How many rows to show is a preference, so it's kept in this browser. It's not a secret, and it's fine if the browser refuses.
function pageSize() {
  try {
    const saved = Number(localStorage.getItem(PAGE_SIZE_KEY));
    if (PAGE_SIZES.includes(saved)) return saved;
  } catch { /* Fall through to the default. */ }
  return DEFAULT_PAGE_SIZE;
}

function savePageSize(size) {
  try { localStorage.setItem(PAGE_SIZE_KEY, String(size)); } catch { /* It then lasts until the page is reloaded. */ }
  fallbackPageSize = size;
}

let fallbackPageSize = 0;

// table draws rows a page at a time, with a filter box, as there can be thousands.
// A column is { title, cell(row), num, text(row) }, where text says what the filter searches.
// more, if given, fetches another batch from the API, for when the last page of what's loaded is reached.
function table(columns, rows, { empty, filter = true, state, more }) {
  const wrap = el("div", {});
  const view = state || { text: "", page: 0 };
  const haystack = (row) => columns.map((c) => (c.text ? c.text(row) : "")).join(" ").toLowerCase();
  const body = el("div", {});
  const count = el("span", { class: "count" });
  function draw() {
    const needle = view.text.trim().toLowerCase();
    const matching = needle ? rows.filter((row) => haystack(row).includes(needle)) : rows;
    const total = more ? `${rows.length}+` : `${rows.length}`;
    count.textContent = needle ? `${matching.length} of ${total}${more ? " loaded so far" : ""}` : total;
    if (!rows.length) { body.replaceChildren(el("div", { class: "table-wrap" }, el("div", { class: "empty" }, empty))); return; }
    if (!matching.length) { body.replaceChildren(el("div", { class: "table-wrap" }, el("div", { class: "empty" }, "Nothing matches that."))); return; }
    const size = fallbackPageSize || pageSize();
    const pages = Math.max(1, Math.ceil(matching.length / size));
    // What was on this page may have been deleted.
    view.page = Math.min(view.page || 0, pages - 1);
    const start = view.page * size;
    const shown = matching.slice(start, start + size);
    const onLast = view.page === pages - 1;
    body.replaceChildren(el("div", { class: "table-wrap" }, el("table", {},
      el("thead", {}, el("tr", {}, columns.map((c) => el("th", { scope: "col", class: c.num ? "num" : "" }, c.title)))),
      el("tbody", {}, shown.map((row) => el("tr", {}, columns.map((c) => el("td", { class: c.num ? "num" : "" }, c.cell(row)))))))));
    // Only when there's something to choose: a short list doesn't need any of this.
    if (matching.length > PAGE_SIZES[0] || more) {
      body.append(el("div", { class: "pager" },
        el("label", {}, "Rows ", el("select", { id: "page-size", onchange: (e) => {
          // Keep the first row in view, rather than jumping somewhere unrelated.
          const first = view.page * size;
          savePageSize(Number(e.target.value));
          view.page = Math.floor(first / Number(e.target.value));
          draw();
          document.getElementById("page-size").focus();
        } }, PAGE_SIZES.map((n) => el("option", { value: n, selected: n === size }, n)))),
        el("span", { class: "count", id: "page-range" }, `${start + 1}\u2013${start + shown.length} of ${needle ? matching.length : total}`),
        el("button", { id: "page-prev", disabled: view.page === 0, onclick: () => { view.page--; draw(); document.getElementById(view.page === 0 ? "page-next" : "page-prev").focus(); } }, "Previous"),
        el("button", { id: "page-next", disabled: onLast && !(more && !needle), onclick: () => {
          if (onLast) {
            view.page++;
            more();
            return;
          }
          view.page++;
          draw();
          const next = document.getElementById("page-next");
          (next.disabled ? document.getElementById("page-prev") : next).focus();
        } }, "Next")));
    }
  }
  if (filter) {
    wrap.append(el("div", { class: "toolbar" },
      el("input", { type: "search", placeholder: "Filter", "aria-label": "Filter", value: view.text,
        oninput: (e) => { view.text = e.target.value; view.page = 0; draw(); } }), count));
  }
  wrap.append(body);
  draw();
  return wrap;
}

// --- Changing things ---

const SIZE_UNITS = [["MiB", 2 ** 20], ["GiB", 2 ** 30], ["TiB", 2 ** 40]];

function notify(message) {
  const old = document.getElementById("toast");
  if (old) old.remove();
  const toast = el("div", { id: "toast", class: "toast", role: "status" }, message);
  document.body.append(toast);
  setTimeout(() => toast.remove(), 5000);
}

// A size in a whole unit, as bytes. Starts on the largest unit that shows it exactly.
function sizeField(field) {
  const [unitName, unitSize] = [...SIZE_UNITS].reverse().find(([, size]) => field.value && field.value % size === 0) || SIZE_UNITS[1];
  const number = el("input", { type: "number", id: "field-" + field.name, min: 1, step: "any", required: field.required, value: field.value ? field.value / unitSize : undefined });
  const unit = el("select", { "aria-label": `${field.label} unit` }, SIZE_UNITS.map(([name]) => el("option", { value: name, selected: name === unitName }, name)));
  return {
    node: el("div", { class: "size" }, number, unit),
    focus: number,
    value: () => {
      if (number.value === "") return undefined;
      return Math.round(Number(number.value) * SIZE_UNITS.find(([name]) => name === unit.value)[1]);
    },
  };
}

function plainField(field) {
  const attrs = { id: "field-" + field.name, required: field.required, placeholder: field.placeholder, autocomplete: "off", spellcheck: "false" };
  const input = field.options
    ? el("select", attrs, field.options.map((o) => el("option", { value: o.value, selected: o.value === field.value }, o.label)))
    : el("input", { ...attrs, type: "text", value: field.value });
  return { node: input, focus: input, value: () => input.value.trim() };
}

// openDialog asks for something, and stays open showing the API's answer if it's refused, with what was typed still there.
function openDialog({ title, intro, fields = [], submit, danger, confirmText, action, done }) {
  const built = fields.map((field) => ({ field, ...(field.size ? sizeField(field) : plainField(field)) }));
  const notice = el("div", {});
  const go = el("button", { type: "submit", class: danger ? "danger" : "primary", id: "dialog-submit" }, submit);
  const typed = confirmText && el("input", { type: "text", id: "field-confirm", autocomplete: "off", spellcheck: "false", required: true });
  if (typed) {
    go.disabled = true;
    typed.addEventListener("input", () => { go.disabled = typed.value !== confirmText; });
  }
  let working = false;
  const dialog = el("dialog", {}, el("form", { method: "dialog", onsubmit: async (e) => {
    e.preventDefault();
    if (working) return;
    working = true;
    go.disabled = true;
    const label = go.textContent;
    go.textContent = "Working...";
    notice.replaceChildren();
    try {
      const values = Object.fromEntries(built.map((b) => [b.field.name, b.value()]));
      const result = await action(values);
      working = false;
      dialog.close();
      done(result, values);
    } catch (err) {
      working = false;
      if (err instanceof Unauthorized) { dialog.close(); rerender(); return; }
      notice.replaceChildren(el("div", { class: "panel-notice bad", role: "alert" }, err.message));
      go.textContent = label;
      go.disabled = false;
    }
  } },
    el("h2", {}, title),
    intro && el("p", {}, intro),
    notice,
    built.map((b) => [
      el("label", { for: "field-" + b.field.name }, b.field.label),
      b.node,
      b.field.hint && el("p", { class: "hint" }, b.field.hint),
    ]),
    typed && [el("label", { for: "field-confirm" }, ["Type ", el("code", {}, confirmText), " to confirm"]), typed],
    el("div", { class: "dialog-actions" },
      el("button", { type: "button", id: "dialog-cancel", onclick: () => { if (!working) dialog.close(); } }, "Cancel"),
      go)));
  // Escape shouldn't look like it stopped something that's already been sent.
  dialog.addEventListener("cancel", (e) => { if (working) e.preventDefault(); });
  dialog.addEventListener("close", () => dialog.remove());
  // Outside main, so that a refresh underneath doesn't lose what's being typed.
  document.body.append(dialog);
  dialog.showModal();
  (built[0] ? built[0].focus : typed || go).focus();
}

const volumePath = (name) => "/v1/volumes/" + encodeURIComponent(name);
const snapshotPath = (name) => "/v1/snapshots/" + encodeURIComponent(name);
const showVolume = (name) => { location.hash = hash("volume", name); };

async function createVolumeDialog(pool) {
  const pools = await api("/v1/pools");
  openDialog({
    title: "Create a volume",
    submit: "Create",
    fields: [
      { name: "name", label: "Name", required: true },
      ...(pools.length > 1 ? [{ name: "pool", label: "Pool", value: pool, options: pools.map((p) => ({ value: p.name, label: p.name })) }] : []),
      { name: "size", label: "Size", size: true, required: true, value: 10 * 2 ** 30, hint: "Thin storage only allocates this as it's written." },
    ],
    action: (v) => send("POST", "/v1/volumes", { name: v.name, pool: v.pool, size_bytes: v.size }),
    done: (vol) => { notify(`Created ${vol.name}.`); showVolume(vol.name); },
  });
}

function growDialog(vol) {
  openDialog({
    title: `Grow ${vol.name}`,
    intro: `It's ${humanSize(vol.size_bytes)} now. It can only get larger, and connected hosts are told of the new size.`,
    submit: "Grow",
    fields: [{ name: "size", label: "New size", size: true, required: true, value: vol.size_bytes }],
    action: (v) => send("PATCH", volumePath(vol.name), { size_bytes: v.size }),
    done: (updated) => { notify(`${vol.name} is now ${humanSize(updated.size_bytes)}.`); rerender(); },
  });
}

function snapshotDialog(vol) {
  openDialog({
    title: `Take a snapshot of ${vol.name}`,
    intro: "It's instant, and works while hosts are writing. It's as if the power had been cut at that moment, so it's crash-consistent.",
    submit: "Take snapshot",
    fields: [{ name: "name", label: "Name", required: true, value: `${vol.name}-${new Date().toISOString().slice(0, 10)}` }],
    action: (v) => send("POST", "/v1/snapshots", { name: v.name, volume: vol.name }),
    done: (snap) => { notify(`Took snapshot ${snap.name}.`); rerender(); },
  });
}

function cloneDialog(source, what) {
  openDialog({
    title: `Clone ${source}`,
    intro: `This makes a new volume from this ${what}, with its own identity. It shares no future changes with it.`,
    submit: "Clone",
    fields: [{ name: "name", label: "New volume's name", required: true, value: `${source}-copy` }],
    action: (v) => send("POST", "/v1/volumes", { name: v.name, source: { [what]: source } }),
    done: (vol) => { notify(`Created ${vol.name}.`); showVolume(vol.name); },
  });
}

function deleteVolumeDialog(vol) {
  openDialog({
    title: `Delete ${vol.name}`,
    intro: "This destroys its data, and can't be undone. Its snapshots and clones aren't affected.",
    submit: "Delete volume",
    danger: true,
    confirmText: vol.name,
    action: () => send("DELETE", volumePath(vol.name)),
    done: () => { notify(`Deleted ${vol.name}.`); location.hash = "#/volumes"; },
  });
}

function deleteSnapshotDialog(snap) {
  openDialog({
    title: `Delete snapshot ${snap.name}`,
    intro: "This can't be undone. Volumes cloned from it aren't affected.",
    submit: "Delete snapshot",
    danger: true,
    action: () => send("DELETE", snapshotPath(snap.name)),
    done: () => { notify(`Deleted snapshot ${snap.name}.`); rerender(); },
  });
}

const snapshotActions = (s) => el("div", { class: "row-actions" },
  el("button", { onclick: () => cloneDialog(s.name, "snapshot") }, "Clone"),
  el("button", { class: "danger-quiet", onclick: () => deleteSnapshotDialog(s) }, "Delete"));

// --- Screens. Each returns what to show, and is called again to refresh. ---

// Screen state that should survive a refresh, such as what's typed in a filter.
let screenState = {};
const stateOf = (key) => (screenState[key] ||= { text: "", page: 0 });

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
    el("div", { class: "toolbar" }, poolPicker(allPools, pool),
      allPools.length
        ? el("button", { class: "primary", id: "create-volume", onclick: () => createVolumeDialog(pool) }, "Create volume")
        : el("span", { class: "muted" }, "Adopt a pool to create volumes, with: nvweaver pool create")),
    table([
      { title: "Name", cell: (v) => [el("strong", {}, link(v.name, "volume", v.name)), v.source && el("span", { class: "sub" }, `clone of ${v.source.snapshot || v.source.volume}`)], text: (v) => v.name },
      { title: "Pool", cell: (v) => link(v.pool, "volumes", v.pool), text: (v) => v.pool },
      { title: "Size", num: true, cell: (v) => humanSize(v.size_bytes) },
      { title: "Exported", cell: (v) => (v.mapped_to ? [link(v.mapped_to.subsystem, "subsystems", v.mapped_to.subsystem), el("span", { class: "sub" }, `namespace ${v.mapped_to.nsid}`)] : pill("Not exported", "quiet")), text: (v) => (v.mapped_to ? v.mapped_to.subsystem : "") },
      { title: "NGUID", cell: (v) => mono(v.nguid), text: (v) => v.nguid },
      { title: "Labels", cell: (v) => labelsText(v.labels) || "-", text: (v) => labelsText(v.labels) },
    ], items, { empty: "No volumes yet.", state: stateOf("volumes" + pool), more: more ? loadMore : undefined }),
    !more && items.length > 0 && el("p", { class: "count" }, `${mapped} of ${items.length} are exported.`),
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
    el("div", { class: "toolbar" },
      el("button", { class: "primary", id: "take-snapshot", onclick: () => snapshotDialog(v) }, "Take snapshot"),
      el("button", { id: "clone-volume", onclick: () => cloneDialog(v.name, "volume") }, "Clone"),
      el("button", { id: "grow-volume", onclick: () => growDialog(v) }, "Grow"),
      el("button", { class: "danger-quiet", id: "delete-volume", disabled: Boolean(v.mapped_to), onclick: () => deleteVolumeDialog(v) }, "Delete"),
      v.mapped_to && el("span", { class: "muted", id: "delete-hint" }, "It can't be deleted while it's exported.")),
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
      { title: "", cell: snapshotActions },
    ], snaps.items, { filter: snaps.items.length > 5, empty: "No snapshots of this volume.", state: stateOf("volume-snapshots"), more: snaps.more ? snaps.loadMore : undefined }),
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
      { title: "", cell: snapshotActions },
    ], items, { empty: "No snapshots yet. Open a volume to take one.", state: stateOf("snapshots"), more: more ? loadMore : undefined }),
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
    ], s.namespaces || [], { filter: false, empty: "Nothing exported yet. Add something with: nvweaver namespace create", state: stateOf("namespaces") }),
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
    ], list, { filter: false, empty: "No ports yet. Add one with: nvweaver port create", state: stateOf("ports") }),
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
    const focused = active && main.contains(active) && active.id ? active.id : "";
    main.replaceChildren(...[content].flat(Infinity).filter((n) => n instanceof Node));
    if (filter) {
      const box = main.querySelector('input[type="search"]');
      if (box) { box.focus(); box.setSelectionRange(filter.start, filter.start); }
    } else if (focused) {
      const again = document.getElementById(focused);
      if (again && !again.disabled) again.focus({ preventScroll: true });
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

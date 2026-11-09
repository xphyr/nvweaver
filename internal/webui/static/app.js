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
// A volume or snapshot whose storage can't be found, which is rare enough to show only then.
const missing = (x) => x.state && x.state !== "online" && pill("Storage missing", "bad");
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
const DEFAULT_PAGE_SIZE = 10;
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

// A list of tickboxes, whose value is the ones ticked.
function checksField(field) {
  const boxes = field.options.map((o) => el("input", { type: "checkbox", value: o.value, checked: (field.value || []).includes(o.value) }));
  return {
    node: el("div", { class: "checks", id: "field-" + field.name, role: "group", "aria-label": field.label },
      field.options.length ? boxes.map((box, i) => el("label", { class: "check" }, box, el("span", { class: "mono" }, field.options[i].label))) : el("p", { class: "hint" }, field.empty)),
    focus: boxes[0],
    value: () => boxes.filter((b) => b.checked).map((b) => (/^\d+$/.test(b.value) ? Number(b.value) : b.value)),
  };
}

function tickField(field) {
  const box = el("input", { type: "checkbox", id: "field-" + field.name, checked: field.value });
  return { node: el("label", { class: "check" }, box, el("span", {}, field.text)), focus: box, value: () => box.checked, noLabel: true };
}

// A choice of one, as radio buttons, which decides what else the dialog asks.
function radioField(field) {
  const radios = field.options.map((o) => el("input", { type: "radio", name: "field-" + field.name, value: o.value, checked: o.value === field.value, disabled: o.disabled }));
  return {
    node: el("div", { id: "field-" + field.name, role: "radiogroup", "aria-label": field.label },
      radios.map((r, i) => el("label", { class: "check" }, r, el("span", {}, field.options[i].label)))),
    focus: radios.find((r) => r.checked) || radios[0],
    value: () => (radios.find((r) => r.checked) || {}).value,
  };
}

function plainField(field) {
  const attrs = { id: "field-" + field.name, required: field.required, placeholder: field.placeholder, autocomplete: "off", spellcheck: "false" };
  const input = field.options
    ? el("select", attrs, field.options.map((o) => el("option", { value: o.value, selected: o.value === field.value }, o.label)))
    : el("input", { ...attrs, type: field.secret ? "password" : "text", value: field.value, autocomplete: field.secret ? "new-password" : "off", list: field.suggestions ? `suggest-${field.name}` : undefined });
  // Suggestions, rather than a dropdown, as there can be thousands to search.
  const list = field.suggestions && el("datalist", { id: `suggest-${field.name}` }, field.suggestions.map((value) => el("option", { value })));
  return { node: list ? el("div", {}, input, list) : input, focus: input, value: () => input.value.trim() };
}

// openDialog asks for something, and stays open showing the API's answer if it's refused, with what was typed still there.
function openDialog({ title, intro, warning, fields = [], submit, danger, confirmText, action, done }) {
  const makers = { checks: checksField, tick: tickField, radio: radioField };
  const built = fields.map((field) => ({ field, ...(field.size ? sizeField(field) : (makers[field.kind] || plainField)(field)) }));
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
      const result = await action(currentValues());
      working = false;
      dialog.close();
      done(result);
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
    warning,
    notice,
    built.map((b) => {
      b.wrap = el("div", { class: "field" },
        !b.noLabel && el("label", { for: "field-" + b.field.name }, b.field.label),
        b.node,
        b.field.hint && el("p", { class: "hint" }, b.field.hint));
      return b.wrap;
    }),
    typed && [el("label", { for: "field-confirm" }, ["Type ", el("code", {}, confirmText), " to confirm"]), typed],
    el("div", { class: "dialog-actions" },
      el("button", { type: "button", id: "dialog-cancel", onclick: () => { if (!working) dialog.close(); } }, "Cancel"),
      go)));
  const currentValues = () => Object.fromEntries(built.map((b) => [b.field.name, b.value()]));
  // Some fields only apply to some choices. A hidden one mustn't stop the form being submitted.
  const applyVisibility = () => {
    const values = currentValues();
    for (const b of built) {
      const show = !b.field.when || b.field.when(values);
      b.wrap.hidden = !show;
      if (b.field.required && b.focus) b.focus.required = show;
    }
  };
  dialog.addEventListener("change", applyVisibility);
  applyVisibility();
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

// What's asked for by a new subsystem's name. It's only a suggestion, as the domain should be the admin's.
const suggestedNQN = (name) => `nqn.${new Date().toISOString().slice(0, 7)}.local.nvweaver:${name}`;

// What a new subsystem asks for, shared by creating one and exporting a volume to one.
function newSubsystemFields(hosts, ports, name, when = () => true) {
  return [
    { name: "nqn", label: "Subsystem name (NQN)", required: true, when, value: suggestedNQN(name) },
    { name: "anyHost", kind: "tick", when, text: "Let any host connect, with no list of who may" },
    { name: "hosts", kind: "checks", label: "Hosts that may connect", when: (v) => when(v) && !v.anyHost, options: hosts.map((h) => ({ value: h.nqn, label: h.nqn })), empty: "No hosts yet, so add one below." },
    { name: "newHost", label: "Add a host", placeholder: "nqn.2014-08.org.nvmexpress:uuid:...", when: (v) => when(v) && !v.anyHost, hint: "From /etc/nvme/hostnqn on the host." },
    ...(ports.length
      ? [{ name: "ports", kind: "checks", label: "Ports to offer it on", when, value: ports.length === 1 ? [String(ports[0].id)] : [], options: ports.map((p) => ({ value: String(p.id), label: `${p.address}:${p.service} (port ${p.id})` })) }]
      : [{ name: "address", label: "Address to listen on", required: true, when, placeholder: "192.0.2.10", hint: "There are no ports yet, so this makes one, on the usual TCP port, 4420." }]),
  ];
}

// Adds what was asked for, and returns the new subsystem's path. A host that's already known is fine: it's only being added to the list.
async function createSubsystem(v, hosts) {
  const allowed = [...(v.hosts || [])];
  if (v.newHost && !v.anyHost) {
    await send("POST", "/v1/hosts", { nqn: v.newHost }).catch((err) => {
      if (!hosts.some((h) => h.nqn === v.newHost)) throw err;
    });
    if (!allowed.includes(v.newHost)) allowed.push(v.newHost);
  }
  let portIds = v.ports || [];
  if (v.address) {
    const made = await send("POST", "/v1/ports", { address: v.address });
    portIds = [made.id];
  }
  await send("POST", "/v1/subsystems", { nqn: v.nqn, allow_any_host: Boolean(v.anyHost), allowed_hosts: v.anyHost ? [] : allowed, ports: portIds });
  return subsystemPath(v.nqn);
}

const subsystemPath = (nqn) => "/v1/subsystems/" + encodeURIComponent(nqn);

async function createSubsystemDialog() {
  const [hosts, ports] = await Promise.all([api("/v1/hosts"), api("/v1/ports")]);
  let made = "";
  openDialog({
    title: "Create a subsystem",
    intro: "This is what hosts connect to. Export volumes into it afterwards.",
    submit: "Create",
    fields: newSubsystemFields(hosts, ports, "my-subsystem"),
    action: async (v) => { made = v.nqn; await createSubsystem(v, hosts); },
    done: () => { notify(`Created ${made}.`); location.hash = hash("subsystems", made); },
  });
}

async function exportDialog(vol) {
  const [hosts, ports, subsystems] = await Promise.all([api("/v1/hosts"), api("/v1/ports"), api("/v1/subsystems")]);
  const usable = subsystems.filter((s) => !s.passthru_device);
  const isNew = (v) => v.mode !== "existing";
  openDialog({
    title: `Export ${vol.name}`,
    intro: "This lets hosts connect to it over NVMe/TCP.",
    submit: "Export",
    fields: [
      ...(usable.length ? [{ name: "mode", kind: "radio", label: "Where", value: "new", options: [
        { value: "new", label: "A new subsystem" },
        { value: "existing", label: "An existing subsystem" },
      ] }] : []),
      ...(usable.length ? [{ name: "existing", label: "Subsystem", when: (v) => v.mode === "existing", value: usable[0].nqn, options: usable.map((s) => ({ value: s.nqn, label: `${s.nqn} (${(s.namespaces || []).length})` })) }] : []),
      ...newSubsystemFields(hosts, ports, vol.name, isNew),
    ],
    action: async (v) => {
      if (!isNew(v)) {
        await send("POST", `${subsystemPath(v.existing)}/namespaces`, { volume: vol.name });
        return;
      }
      const path = await createSubsystem(v, hosts);
      try {
        await send("POST", path + "/namespaces", { volume: vol.name });
      } catch (err) {
        // Don't leave an empty subsystem behind, so that trying again works.
        await send("DELETE", path).catch(() => {});
        throw err;
      }
    },
    done: () => { notify(`Exported ${vol.name}.`); rerender(); },
  });
}

// Who's connected, or null if it can't be found out, which shouldn't get in the way of what someone came to do.
async function connectionsTo(nqn) {
  try {
    return await api("/v1/connections?subsystem=" + encodeURIComponent(nqn));
  } catch (err) {
    if (err instanceof Unauthorized) throw err;
    return null;
  }
}

// The kernel's target usually only knows who's connected to a port, so what's said about them has to be that careful.
function connectionsNotice(list) {
  if (list === null) return el("div", { class: "panel-notice warn", id: "who-is-connected" }, "Couldn't check who's connected.");
  if (list.length === 0) return el("div", { class: "panel-notice", id: "who-is-connected" }, "Nothing is connected right now.");
  const names = [...new Set(list.map((c) => c.host_nqn || c.host_address))].join(", ");
  return el("div", { class: "panel-notice warn", id: "who-is-connected" }, list.every((c) => c.host_nqn)
    ? `Connected right now: ${names}.`
    : `Connected to its ports right now: ${names}. This target can't say which subsystem they're using, so they may not be using this one.`);
}

async function deleteSubsystemDialog(sub) {
  const exported = (sub.namespaces || []).length;
  const warning = connectionsNotice(await connectionsTo(sub.nqn));
  openDialog({
    title: "Delete this subsystem",
    warning,
    intro: `Connected hosts lose access at once.${exported ? ` ${exported === 1 ? "The volume or file in it is" : `The ${exported} volumes or files in it are`} kept, but no longer exported.` : ""}`,
    submit: "Delete subsystem",
    danger: true,
    confirmText: sub.nqn,
    action: () => send("DELETE", subsystemPath(sub.nqn)),
    done: () => { notify(`Deleted ${sub.nqn}.`); location.hash = "#/subsystems"; },
  });
}

async function allowHostDialog(sub) {
  const hosts = await api("/v1/hosts");
  const others = hosts.filter((h) => !(sub.allowed_hosts || []).includes(h.nqn)).map((h) => h.nqn);
  openDialog({
    title: "Allow a host",
    intro: "It can then connect to this subsystem.",
    submit: "Allow",
    fields: [{ name: "host", label: "Host", required: true, suggestions: others, placeholder: "Type to search, or enter a new NQN", hint: "A new one is added to the hosts." }],
    action: async (v) => {
      if (!hosts.some((h) => h.nqn === v.host)) await send("POST", "/v1/hosts", { nqn: v.host });
      await send("PUT", `${subsystemPath(sub.nqn)}/hosts/${encodeURIComponent(v.host)}`);
      return v.host;
    },
    done: (host) => { notify(`${host} may connect.`); rerender(); },
  });
}

function denyHostDialog(sub, host) {
  openDialog({
    title: `Remove ${host}`,
    intro: "It's cut off at once, and anything using it will see I/O errors. Its writes stop reaching the disk. On the kernel target, other hosts on this subsystem also pause for about 10 s while they reconnect, with no errors.",
    submit: "Remove host",
    danger: true,
    action: () => send("DELETE", `${subsystemPath(sub.nqn)}/hosts/${encodeURIComponent(host)}`),
    done: () => { notify(`${host} can no longer connect.`); rerender(); },
  });
}

function setAnyHostDialog(sub, allow) {
  openDialog({
    title: allow ? "Let any host connect" : "Require a list of hosts",
    intro: allow ? "Any host that can reach its ports can then use what's in this subsystem, with no list of who may." : "Nobody is on the list yet, so connected hosts are cut off at once until you add them.",
    submit: allow ? "Let any host connect" : "Require a list",
    danger: true,
    action: () => send("PATCH", subsystemPath(sub.nqn), { allow_any_host: allow }),
    done: () => { notify(allow ? "Any host can now connect." : "Only hosts on the list can now connect."); rerender(); },
  });
}

async function attachPortDialog(sub, ports) {
  const free = ports.filter((p) => !(sub.ports || []).includes(p.id));
  openDialog({
    title: "Offer it on a port",
    submit: "Offer",
    fields: [{ name: "port", label: "Port", value: String(free[0].id), options: free.map((p) => ({ value: String(p.id), label: `${p.address}:${p.service} (port ${p.id})` })) }],
    action: async (v) => {
      await send("PUT", `${subsystemPath(sub.nqn)}/ports/${v.port}`);
      return v.port;
    },
    done: (port) => { notify(`Offered on port ${port}.`); rerender(); },
  });
}

function detachPortDialog(sub, port) {
  openDialog({
    title: `Stop offering it on port ${port.id}`,
    intro: `Hosts connected through ${port.address}:${port.service} lose access to this subsystem at once.`,
    submit: "Stop offering",
    danger: true,
    action: () => send("DELETE", `${subsystemPath(sub.nqn)}/ports/${port.id}`),
    done: () => { notify(`No longer offered on port ${port.id}.`); rerender(); },
  });
}

function addBackingDialog(sub) {
  const isFile = (v) => v.kind === "file";
  openDialog({
    title: "Add a device or file",
    intro: "To export a volume, use Add volume, as its identity then stays with it.",
    submit: "Add",
    fields: [
      { name: "kind", kind: "radio", label: "What", value: "device", options: [
        { value: "device", label: "A block device or file that's already there" },
        { value: "file", label: "A new file, made by this service" },
      ] },
      { name: "path", label: "Path", required: true, when: (v) => !isFile(v), placeholder: "/dev/disk/by-id/...", hint: "As the service sees it. A disk in use, such as one that's mounted, is refused." },
      { name: "file", label: "File name", required: true, when: isFile, placeholder: "data.img" },
      { name: "size", label: "Size", size: true, required: true, when: isFile, value: 10 * 2 ** 30 },
    ],
    action: (v) => send("POST", `${subsystemPath(sub.nqn)}/namespaces`, isFile(v) ? { file: { name: v.file, size_bytes: v.size } } : { device_path: v.path }),
    done: () => { notify("Added."); rerender(); },
  });
}

function removeBackingDialog(sub, ns) {
  openDialog({
    title: `Remove namespace ${ns.nsid}`,
    intro: "Connected hosts lose access to it at once.",
    submit: "Remove",
    danger: true,
    fields: ns.managed_file ? [{ name: "deleteFile", kind: "tick", text: "Also delete its file, which this service made, and its data" }] : [],
    action: (v) => send("DELETE", `${subsystemPath(sub.nqn)}/namespaces/${ns.nsid}${v.deleteFile ? "?delete_file=true" : ""}`),
    done: () => { notify(`Removed namespace ${ns.nsid}.`); rerender(); },
  });
}

function addPortDialog() {
  openDialog({
    title: "Add a port",
    intro: "This is an address that hosts connect to.",
    submit: "Add",
    fields: [
      { name: "address", label: "Address", required: true, placeholder: "192.0.2.10", hint: "An address on this machine." },
      { name: "service", label: "TCP port", required: true, value: "4420" },
    ],
    action: (v) => send("POST", "/v1/ports", { address: v.address, service: Number(v.service) }),
    done: (p) => { notify(`Added port ${p.id}.`); rerender(); },
  });
}

function editPortDialog(port) {
  openDialog({
    title: `Change port ${port.id}`,
    intro: "Hosts connected through it lose their connection, and have to reconnect to the new address.",
    submit: "Change",
    fields: [
      { name: "address", label: "Address", required: true, value: port.address },
      { name: "service", label: "TCP port", required: true, value: String(port.service) },
    ],
    action: (v) => send("PATCH", `/v1/ports/${port.id}`, { address: v.address, service: Number(v.service) }),
    done: () => { notify(`Changed port ${port.id}.`); rerender(); },
  });
}

function deletePortDialog(port) {
  openDialog({
    title: `Delete port ${port.id}`,
    intro: `This stops listening on ${port.address}:${port.service}. A port that subsystems are offered on is refused.`,
    submit: "Delete port",
    danger: true,
    action: () => send("DELETE", `/v1/ports/${port.id}`),
    done: () => { notify(`Deleted port ${port.id}.`); rerender(); },
  });
}

const keyFields = [
  { name: "key", label: "DH-HMAC-CHAP key", secret: true, placeholder: "DHHC-1:00:...", hint: "It can't be shown again. Make one with: nvme gen-dhchap-key" },
  { name: "ctrlKey", label: "Controller key", secret: true, placeholder: "Optional", hint: "With this, the host checks the target too." },
];

function addHostDialog() {
  openDialog({
    title: "Add a host",
    submit: "Add",
    fields: [{ name: "nqn", label: "Host name (NQN)", required: true, placeholder: "nqn.2014-08.org.nvmexpress:uuid:...", hint: "From /etc/nvme/hostnqn on the host." }, ...keyFields],
    action: (v) => send("POST", "/v1/hosts", { nqn: v.nqn, dhchap_key: v.key || undefined, dhchap_ctrl_key: v.ctrlKey || undefined }),
    done: (h) => { notify(`Added ${h.nqn}.`); rerender(); },
  });
}

// Saving a key drops the sessions that logged in with the old one, so the host must have it first. See decision 30.
async function hostKeysDialog(host) {
  const sys = await api("/v1/system");
  const others = sys.backend === "spdk"
    ? "Only this host is dropped."
    : "On the kernel target, other hosts on each subsystem that lists this host also pause for about 10 s while they reconnect, with no errors.";
  openDialog({
    title: "Set authentication keys",
    intro: `For ${host.nqn}. Put the new key on the host first: saving it here drops the sessions that logged in with the old one, and the host can only reconnect with the new key. ${others} Keys can be replaced, but to remove them, delete the host and add it again.`,
    submit: "Save",
    fields: keyFields,
    action: (v) => {
      const body = {};
      if (v.key) body.dhchap_key = v.key;
      if (v.ctrlKey) body.dhchap_ctrl_key = v.ctrlKey;
      if (!Object.keys(body).length) throw new Error("Enter a key to set.");
      return send("PATCH", `/v1/hosts/${encodeURIComponent(host.nqn)}`, body);
    },
    done: () => { notify("Keys saved."); rerender(); },
  });
}

function deleteHostDialog(host) {
  openDialog({
    title: "Delete this host",
    intro: `${host.nqn}, and any keys it has. A host that's allowed on a subsystem is refused.`,
    submit: "Delete host",
    danger: true,
    action: () => send("DELETE", `/v1/hosts/${encodeURIComponent(host.nqn)}`),
    done: () => { notify("Deleted the host."); rerender(); },
  });
}

async function adoptPoolDialog() {
  const sys = await api("/v1/system");
  const types = sys.pool_types || [];
  const isLVM = (v) => v.type === "lvm-thin";
  openDialog({
    title: "Adopt a pool",
    intro: "This uses storage that's already set up. It never initializes a disk.",
    submit: "Adopt",
    fields: [
      { name: "name", label: "Name", required: true },
      { name: "type", label: "Type", value: types.includes("reflink") ? "reflink" : types[0], options: types.map((t) => ({ value: t, label: t === "lvm-thin" ? "LVM thin pool" : t === "reflink" ? "Reflink files, on XFS or btrfs" : t })) },
      { name: "path", label: "Directory", required: true, when: (v) => !isLVM(v), placeholder: "/srv/nvweaver" },
      { name: "vg", label: "Volume group", required: true, when: isLVM },
      { name: "thin", label: "Thin pool", required: true, when: isLVM, value: "pool0" },
    ],
    action: (v) => send("POST", "/v1/pools", isLVM(v)
      ? { name: v.name, type: v.type, volume_group: v.vg, thin_pool: v.thin }
      : { name: v.name, type: v.type, path: v.path }),
    done: (p) => { notify(`Adopted ${p.name}.`); rerender(); },
  });
}

function forgetPoolDialog(pool) {
  openDialog({
    title: `Forget ${pool.name}`,
    intro: "This only stops using it: its storage is left alone. A pool with volumes or snapshots is refused.",
    submit: "Forget pool",
    danger: true,
    action: () => send("DELETE", "/v1/pools/" + encodeURIComponent(pool.name)),
    done: () => { notify(`Forgot ${pool.name}.`); rerender(); },
  });
}

// Every volume that isn't exported, following the API's pages.
async function unexportedVolumes() {
  const names = [];
  let after = "";
  do {
    const params = new URLSearchParams();
    if (after) params.set("after", after);
    const page = await api(`/v1/volumes?${params}`);
    names.push(...(page.items || []).filter((v) => !v.mapped_to).map((v) => v.name));
    after = page.next_after || "";
  } while (after);
  return names;
}

async function addVolumeDialog(subsystem) {
  const names = await unexportedVolumes();
  openDialog({
    title: "Add a volume",
    intro: `Hosts allowed on ${subsystem.nqn} can then reach it.`,
    submit: "Add volume",
    fields: [{ name: "volume", label: "Volume", required: true, suggestions: names, placeholder: names.length ? "Type to search" : "There are none that aren't exported",
      hint: names.length ? `${names.length} aren't exported.` : undefined }],
    action: async (v) => {
      await send("POST", `${subsystemPath(subsystem.nqn)}/namespaces`, { volume: v.volume });
      return v.volume;
    },
    done: (name) => { notify(`Exported ${name}.`); rerender(); },
  });
}

async function unexportDialog(vol, subsystem, keepSubsystem) {
  const nsid = vol.mapped_to.nsid;
  const alone = (subsystem.namespaces || []).length <= 1;
  const warning = connectionsNotice(await connectionsTo(subsystem.nqn));
  openDialog({
    title: `Un-export ${vol.name}`,
    warning,
    intro: "The volume and its data are kept, but connected hosts lose access to it at once, so anything using it will see I/O errors.",
    submit: "Un-export",
    danger: true,
    fields: alone ? [{ name: "removeSubsystem", kind: "tick", value: !keepSubsystem, text: `Also remove ${subsystem.nqn}, as nothing else is in it` }] : [],
    action: async (v) => {
      const path = "/v1/subsystems/" + encodeURIComponent(subsystem.nqn);
      await send("DELETE", `${path}/namespaces/${nsid}`);
      if (v.removeSubsystem) await send("DELETE", path);
      return v.removeSubsystem;
    },
    done: (removed) => {
      notify(`${vol.name} is no longer exported.`);
      // Its own page has gone, if that's where this was.
      if (removed && location.hash.startsWith("#/subsystems/")) location.hash = "#/subsystems";
      else rerender();
    },
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
    ...(options.manage ? [{ title: "", cell: (p) => el("div", { class: "row-actions" }, el("button", { class: "danger-quiet", onclick: () => forgetPoolDialog(p) }, "Forget")) }] : []),
  ], pools, { empty: "No pools yet.", ...options, filter: options.filter ?? pools.length > 5, state: stateOf("pools") });
}

async function pools() {
  const list = await api("/v1/pools");
  const troubled = list.flatMap((p) => (p.warnings || []).map((w) => `${p.name}: ${w}`));
  return [
    el("h1", {}, "Pools"),
    el("div", { class: "toolbar" }, el("button", { class: "primary", id: "adopt-pool", onclick: adoptPoolDialog }, "Adopt pool")),
    troubled.map((w) => el("div", { class: "panel-notice warn" }, w)),
    poolTable(list, { manage: true }),
  ];
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
        : el("span", { class: "muted" }, ["Adopt a pool to create volumes: see ", el("a", { href: "#/pools" }, "Pools")])),
    table([
      { title: "Name", cell: (v) => [el("strong", {}, link(v.name, "volume", v.name)), missing(v), v.source && el("span", { class: "sub" }, `clone of ${v.source.snapshot || v.source.volume}`)], text: (v) => v.name },
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
  const [v, usage, snaps, allPorts] = await Promise.all([
    api(path),
    // If the storage can't say, the rest of the page is still worth showing.
    api(path + "/usage", { retries: 0 }).catch((err) => { if (err instanceof Unauthorized) throw err; return null; }),
    pagedList("/v1/snapshots", `volume=${encodeURIComponent(name)}`),
    api("/v1/ports"),
  ]);
  const sub = v.mapped_to ? await api("/v1/subsystems/" + encodeURIComponent(v.mapped_to.subsystem)) : null;
  const offered = sub ? allPorts.filter((p) => (sub.ports || []).includes(p.id)) : [];
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/volumes" }, "Volumes")),
    el("h1", {}, v.name),
    el("div", { class: "toolbar" },
      sub
        ? el("button", { class: "danger-quiet", id: "unexport-volume", onclick: () => unexportDialog(v, sub) }, "Un-export")
        : el("button", { class: "primary", id: "export-volume", onclick: () => exportDialog(v) }, "Export"),
      el("button", { id: "take-snapshot", onclick: () => snapshotDialog(v) }, "Take snapshot"),
      el("button", { id: "clone-volume", onclick: () => cloneDialog(v.name, "volume") }, "Clone"),
      el("button", { id: "grow-volume", onclick: () => growDialog(v) }, "Grow"),
      el("button", { class: "danger-quiet", id: "delete-volume", disabled: Boolean(v.mapped_to), onclick: () => deleteVolumeDialog(v) }, "Delete"),
      v.mapped_to && el("span", { class: "muted", id: "delete-hint" }, "It can't be deleted while it's exported.")),
    el("dl", { class: "facts" },
      missing(v) && fact("Storage", [missing(v), el("div", { class: "muted", id: "missing-hint" }, ["Its device can't be found: ", mono(v.device_path), ". Hosts can't use it until it's back. An LVM volume may need activating."])]),
      fact("Space", usageBar(usage)),
      fact("Pool", link(v.pool, "volumes", v.pool)),
      fact("Exported", v.mapped_to ? [link(v.mapped_to.subsystem, "subsystems", v.mapped_to.subsystem), ` as namespace ${v.mapped_to.nsid}`] : pill("Not exported", "quiet")),
      sub && fact("Who can connect", sub.allow_any_host ? pill("Any host", "warn") : (sub.allowed_hosts || []).length ? sub.allowed_hosts.map((h) => el("div", {}, mono(h))) : pill("Nobody yet", "warn")),
      sub && fact("To connect", offered.length
        ? offered.map((p) => el("div", {}, el("code", { class: "command" }, `nvme connect -t ${p.transport} -a ${p.address} -s ${p.service} -n ${sub.nqn}`)))
        : pill("It isn't on any port, so hosts can't reach it", "warn")),
      v.source && fact("Cloned from", v.source.snapshot ? `snapshot ${v.source.snapshot}` : link(v.source.volume, "volume", v.source.volume)),
      fact("Created", humanTime(v.created_at)),
      fact("UUID", mono(v.uuid)), fact("NGUID", mono(v.nguid)),
      fact("Backed by", mono(v.device_path)),
      fact("Labels", labelsText(v.labels) || "-")),
    el("h2", {}, "Snapshots"),
    table([
      { title: "Name", cell: (s) => [el("strong", {}, s.name), missing(s)], text: (s) => s.name },
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
      { title: "Name", cell: (s) => [el("strong", {}, s.name), missing(s)], text: (s) => s.name },
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
    el("div", { class: "toolbar" }, el("button", { class: "primary", id: "create-subsystem", onclick: createSubsystemDialog }, "Create subsystem")),
    table([
      { title: "NQN", cell: (s) => link(s.nqn, "subsystems", s.nqn), text: (s) => s.nqn },
      { title: "Who can connect", cell: (s) => (s.allow_any_host ? pill("Any host", "warn") : `${(s.allowed_hosts || []).length} allowed`), text: (s) => (s.allowed_hosts || []).join(" ") },
      { title: "Ports", cell: (s) => (s.ports || []).join(", ") || "-" },
      { title: "Namespaces", num: true, cell: (s) => (s.namespaces || []).length },
      { title: "Serial", cell: (s) => mono(s.serial), text: (s) => s.serial },
    ], list, { empty: "No subsystems yet.", state: stateOf("subsystems") }),
  ];
}

async function subsystem(nqn) {
  const [s, allPorts, connections] = await Promise.all([api(subsystemPath(nqn)), api("/v1/ports"), connectionsTo(nqn)]);
  const notServed = (s.namespaces || []).filter((n) => n.enabled && !n.live_enabled);
  const allowed = (s.allowed_hosts || []).map((h) => ({ nqn: h }));
  const offered = allPorts.filter((p) => (s.ports || []).includes(p.id));
  const canAttach = allPorts.length > offered.length;
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/subsystems" }, "Subsystems")),
    el("h1", {}, mono(s.nqn)),
    el("div", { class: "toolbar" }, el("button", { class: "danger-quiet", id: "delete-subsystem", onclick: () => deleteSubsystemDialog(s) }, "Delete")),
    notServed.length > 0 && el("div", { class: "panel-notice bad" }, `Namespace ${notServed.map((n) => n.nsid).join(", ")} should be served, but isn't.`),
    el("dl", { class: "facts" },
      fact("Serial", mono(s.serial)), fact("Model", s.model),
      s.passthru_device && fact("Passes through", mono(s.passthru_device))),
    el("div", { class: "section-head" },
      el("h2", {}, "Who can connect"),
      el("div", { class: "row-actions" },
        s.allow_any_host
          ? el("button", { id: "require-list", onclick: () => setAnyHostDialog(s, false) }, "Require a list")
          : [el("button", { id: "any-host", disabled: allowed.length > 0, title: allowed.length ? "Remove the hosts on the list first" : undefined, onclick: () => setAnyHostDialog(s, true) }, "Let any host connect"),
            el("button", { class: "primary", id: "allow-host", onclick: () => allowHostDialog(s) }, "Allow a host")])),
    s.allow_any_host
      ? el("div", { class: "panel-notice warn", id: "any-host-notice" }, "Any host that can reach its ports can connect.")
      : table([
        { title: "Host", cell: (h) => mono(h.nqn), text: (h) => h.nqn },
        { title: "", cell: (h) => el("div", { class: "row-actions" }, el("button", { class: "danger-quiet", onclick: () => denyHostDialog(s, h.nqn) }, "Remove")) },
      ], allowed, { filter: allowed.length > 5, empty: "Nobody yet, so no host can connect.", state: stateOf("allowed") }),
    el("div", { class: "section-head" },
      el("h2", {}, "Ports"),
      el("button", { class: "primary", id: "attach-port", disabled: !canAttach, title: canAttach ? undefined : "It's on every port there is", onclick: () => attachPortDialog(s, allPorts) }, "Offer on a port")),
    table([
      { title: "ID", cell: (p) => p.id },
      { title: "Address", cell: (p) => mono(`${p.address}:${p.service}`) },
      { title: "To connect", cell: (p) => el("code", { class: "command" }, `nvme connect -t ${p.transport} -a ${p.address} -s ${p.service} -n ${s.nqn}`) },
      { title: "", cell: (p) => el("div", { class: "row-actions" }, el("button", { class: "danger-quiet", onclick: () => detachPortDialog(s, p) }, "Stop offering")) },
    ], offered, { filter: false, empty: "None, so hosts can't reach it.", state: stateOf("sub-ports") }),
    el("h2", {}, "Connected now"),
    connections === null
      ? connectionsNotice(null)
      : [
        connections.some((c) => !c.host_nqn) && el("p", { class: "hint", id: "connections-hint" }, "This target only shows who's connected to these ports, who may be using another subsystem on them."),
        table([
          { title: "Host", cell: (c) => (c.host_nqn ? mono(c.host_nqn) : "-") },
          { title: "From", cell: (c) => mono(c.host_address) },
          { title: "Port", cell: (c) => c.port },
          { title: "Queues", num: true, cell: (c) => c.queues },
        ], connections, { filter: false, empty: "Nothing is connected right now.", state: stateOf("connections") }),
      ],
    el("div", { class: "section-head" },
      el("h2", {}, "Namespaces"),
      !s.passthru_device && el("div", { class: "row-actions" },
        el("button", { id: "add-backing", onclick: () => addBackingDialog(s) }, "Add device or file"),
        el("button", { class: "primary", id: "add-volume", onclick: () => addVolumeDialog(s) }, "Add volume"))),
    table([
      { title: "NSID", cell: (n) => n.nsid },
      { title: "Serving", cell: (n) => (n.live_enabled ? pill("Live", "ok") : n.enabled ? pill("Not serving", "bad") : pill("Disabled", "quiet")) },
      { title: "Size", num: true, cell: (n) => humanSize(n.size_bytes) },
      { title: "Backed by", cell: (n) => [n.volume ? link(n.volume, "volume", n.volume) : null, el("span", { class: n.volume ? "sub" : "" }, mono(n.device_path))] },
      { title: "NGUID", cell: (n) => mono(n.nguid) },
      { title: "", cell: (n) => el("div", { class: "row-actions" }, n.volume
        ? el("button", { class: "danger-quiet", onclick: () => unexportDialog({ name: n.volume, mapped_to: { nsid: n.nsid } }, s, true) }, "Un-export")
        : el("button", { class: "danger-quiet", onclick: () => removeBackingDialog(s, n) }, "Remove")) },
    ], s.namespaces || [], { filter: false, empty: "Nothing is exported here yet.", state: stateOf("namespaces") }),
  ];
}

async function hosts() {
  const list = await api("/v1/hosts");
  return [
    el("h1", {}, "Hosts"),
    el("div", { class: "toolbar" }, el("button", { class: "primary", id: "add-host", onclick: addHostDialog }, "Add host")),
    table([
      { title: "NQN", cell: (h) => mono(h.nqn), text: (h) => h.nqn },
      { title: "Authentication", cell: (h) => (h.has_dhchap_key ? pill(h.has_dhchap_ctrl_key ? "DH-HMAC-CHAP, both ways" : "DH-HMAC-CHAP", "ok") : pill("None", "quiet")) },
      { title: "Hash", cell: (h) => h.dhchap_hash || "-" },
      { title: "DH group", cell: (h) => h.dhchap_dhgroup || "-" },
      { title: "", cell: (h) => el("div", { class: "row-actions" },
        el("button", { onclick: () => hostKeysDialog(h) }, "Set keys"),
        el("button", { class: "danger-quiet", onclick: () => deleteHostDialog(h) }, "Delete")) },
    ], list, { empty: "No hosts yet.", state: stateOf("hosts") }),
  ];
}

async function ports() {
  const list = await api("/v1/ports");
  return [
    el("h1", {}, "Ports"),
    el("div", { class: "toolbar" }, el("button", { class: "primary", id: "add-port", onclick: addPortDialog }, "Add port")),
    table([
      { title: "ID", cell: (p) => p.id },
      { title: "Transport", cell: (p) => p.transport },
      { title: "Address", cell: (p) => mono(p.address), text: (p) => p.address },
      { title: "Service", cell: (p) => p.service },
      { title: "", cell: (p) => el("div", { class: "row-actions" },
        el("button", { onclick: () => editPortDialog(p) }, "Change"),
        el("button", { class: "danger-quiet", onclick: () => deletePortDialog(p) }, "Delete")) },
    ], list, { filter: false, empty: "No ports yet.", state: stateOf("ports") }),
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

"use strict";

const COLORS = ["#6cc7a3", "#5a9bd8", "#e0a96d", "#c98bdb", "#e07a7a", "#7ad0d0", "#b5c46c", "#d88bb0"];
const state = {
  dim: "overall",
  metric: "selfPctBusy",
  topN: 50,
  selected: [], // {id, label, color}
  matrix: null,
  // sort.kind: "group" (col = group index) or "pct" (% change column).
  sort: { kind: "group", col: -1, dir: -1 }, // dir -1 desc / 1 asc
  categories: { user: true, node_modules: true, native: true, idle: false },
};

const $ = (sel) => document.querySelector(sel);
const el = (tag, attrs = {}, ...kids) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v);
  }
  for (const kid of kids) if (kid != null) n.append(kid);
  return n;
};

function gidString(id) {
  return `${id.env}/${id.service}/${id.date}/${id.buildTag}`;
}

// ---------- API ----------
async function api(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  let data;
  try { data = text ? JSON.parse(text) : {}; } catch { data = { error: text }; }
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// ---------- Browser ----------
let browsePath = {}; // {env, service, date, buildTag}

async function loadBrowse() {
  const params = new URLSearchParams();
  for (const k of ["env", "service", "date", "buildTag"]) {
    if (browsePath[k]) params.set(k, browsePath[k]);
  }
  renderBreadcrumb();
  const browser = $("#browser");
  browser.innerHTML = "<li class='meta'>loading…</li>";
  try {
    const res = await api("/api/groups?" + params.toString());
    renderBrowse(res);
  } catch (e) {
    browser.innerHTML = "";
    browser.append(el("li", { class: "error", text: e.message }));
  }
}

function renderBreadcrumb() {
  const bc = $("#breadcrumb");
  bc.innerHTML = "";
  const crumbs = [["root", null]];
  for (const k of ["env", "service", "date", "buildTag"]) {
    if (browsePath[k]) crumbs.push([browsePath[k], k]);
  }
  crumbs.forEach(([label, key], i) => {
    if (i > 0) bc.append(document.createTextNode(" / "));
    bc.append(el("a", {
      text: label,
      onclick: () => {
        // Reset everything from this level down.
        const order = ["env", "service", "date", "buildTag"];
        const idx = key ? order.indexOf(key) : -1;
        browsePath = {};
        for (let j = 0; j <= idx; j++) browsePath[order[j]] = crumbs[j + 1][0];
        loadBrowse();
      },
    }));
  });
}

function renderBrowse(res) {
  const browser = $("#browser");
  browser.innerHTML = "";

  if (res.groups && res.groups.length) {
    for (const g of res.groups) {
      browser.append(groupItem(g));
    }
    return;
  }
  if (res.children && res.children.length) {
    const nextKey = nextLevelKey();
    for (const c of res.children) {
      browser.append(el("li", {
        text: "📁 " + c,
        onclick: () => { browsePath[nextKey] = c; loadBrowse(); },
      }));
    }
    return;
  }
  browser.append(el("li", { class: "meta", text: "(empty)" }));
}

function nextLevelKey() {
  if (!browsePath.env) return "env";
  if (!browsePath.service) return "service";
  if (!browsePath.date) return "date";
  return "buildTag";
}

function groupItem(g) {
  const label = `${g.id.date} · ${g.id.buildTag}`;
  const li = el("li", { class: "group" },
    el("span", { text: "📊 " + label }),
    el("span", { class: "meta", text: `${g.members.length} file(s)` }),
    el("button", { class: "btn-ghost", text: "+ add", onclick: () => addSelected(g.id) }),
  );
  return li;
}

// ---------- Selection ----------
function addSelected(id) {
  const key = gidString(id);
  if (state.selected.some((s) => gidString(s.id) === key)) return;
  const color = COLORS[state.selected.length % COLORS.length];
  state.selected.push({ id, label: `${id.service}/${id.date}/${id.buildTag}`, color });
  renderSelected();
}

function removeSelected(key) {
  state.selected = state.selected.filter((s) => gidString(s.id) !== key);
  // Reassign colors so they stay consistent with order.
  state.selected.forEach((s, i) => (s.color = COLORS[i % COLORS.length]));
  renderSelected();
}

function renderSelected() {
  // The same action views one profile group or compares several; label it for
  // whichever the current selection will do.
  const btn = $("#refresh");
  if (btn) btn.textContent = state.selected.length >= 2 ? "Compare" : "View";

  const ul = $("#selected");
  ul.innerHTML = "";
  if (!state.selected.length) {
    ul.append(el("li", { class: "meta", text: "none selected" }));
    return;
  }
  for (const s of state.selected) {
    const key = gidString(s.id);
    ul.append(el("li", {},
      el("span", { class: "swatch", style: `background:${s.color}` }),
      el("span", { text: s.label }),
      el("span", { class: "x", text: "✕", onclick: () => removeSelected(key) }),
    ));
  }
}

// ---------- Compare (streaming) ----------
function enabledCategories() {
  return Object.entries(state.categories).filter(([, on]) => on).map(([k]) => k);
}

async function runCompare() {
  if (state.dim === "blocking") return runBlocking();
  if (state.selected.length === 0) {
    showEmpty("Select a group to view, or two or more to compare.");
    return;
  }
  const result = $("#result");
  result.innerHTML = "<p class='empty'>Comparing…</p>";
  showProgress(0, 0);
  try {
    const res = await fetch("/api/compare", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        groups: state.selected.map((s) => s.id),
        dimension: state.dim,
        metric: state.metric,
        topN: Number(state.topN) || 0,
        categories: enabledCategories(),
      }),
    });
    if (!res.ok || !res.body) {
      throw new Error(await res.text() || res.statusText);
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let matrix = null;
    let err = null;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") showProgress(msg.done, msg.total);
        else if (msg.type === "result") matrix = msg.matrix;
        else if (msg.type === "error") err = msg.error;
      }
    }
    hideProgress();
    if (err) throw new Error(err);
    if (!matrix) throw new Error("no result returned");

    state.matrix = matrix;
    state.sort = { kind: "group", col: -1, dir: -1 };
    renderSummaries(matrix);
    renderMatrix(matrix);
  } catch (e) {
    hideProgress();
    result.innerHTML = "";
    result.append(el("div", { class: "error", text: e.message }));
  }
}

// ---------- Blocking (event-loop long tasks) ----------
async function runBlocking() {
  if (state.selected.length === 0) {
    showEmpty("Select a group to see where the event loop is blocked.");
    return;
  }
  $("#summaries").innerHTML = "";
  const result = $("#result");
  result.innerHTML = "<p class='empty'>Loading…</p>";
  const sel = state.selected[0];
  const id = sel.id;
  const topN = Number(state.topN) || 25;
  const path = `/api/group/${seg(id.env)}/${seg(id.service)}/${seg(id.date)}/${seg(id.buildTag)}/blocking?topN=${topN}`;
  try {
    renderBlocking(await api(path), sel);
  } catch (e) {
    result.innerHTML = "";
    result.append(el("div", { class: "error", text: e.message }));
  }
}

function renderBlocking(bv, sel) {
  const result = $("#result");
  result.innerHTML = "";
  if (state.selected.length > 1) {
    result.append(el("p", { class: "hint", text: `Showing blocking for ${sel.label} (the first selected group).` }));
  }
  if (!bv.episodes) {
    result.append(el("p", { class: "empty", text: `No long tasks ≥ ${fmtMicros(bv.thresholdMicros || 0)} in this group.` }));
    return;
  }
  result.append(el("div", { class: "summary-card", style: `border-left-color:${sel.color}` },
    el("div", { class: "title", text: `Event-loop blocking · threshold ${fmtMicros(bv.thresholdMicros)}` }),
    kv("Episodes", `${bv.episodes} (${bv.episodesPerProfile.toFixed(2)}/profile)`),
    kv("Blocked total", `${fmtMicros(bv.blockedMicros)} (${fmtMicros(bv.blockedMicrosPerProfile)}/profile)`),
    kv("Worst episode", fmtMicros(bv.maxEpisodeMicros)),
  ));
  result.append(blockingStalls(bv.stalls));
  result.append(blockingRows("Top blocking functions", bv.functions));
  result.append(blockingRows("Top blocking contexts (routes / APIs)", bv.contexts));
}

// blockingStalls lists the worst individual episodes; each expands to its
// call stack (shown leaf-first, the most relevant frame on top).
function blockingStalls(stalls) {
  const sec = el("div", { class: "bd-section" });
  sec.append(el("h3", { text: "Worst stalls (exact stacks)" }));
  if (!stalls || !stalls.length) {
    sec.append(el("p", { class: "bd-empty muted-cell", text: "none" }));
    return sec;
  }
  for (const s of stalls) {
    const head = el("summary", {},
      el("b", { text: fmtMicros(s.durationMicros) }),
      el("span", { class: "muted-cell", text: `  ${s.samples} sample(s)` }),
      s.context ? el("span", { class: "pkg-tag", text: "  " + s.context }) : null,
    );
    const stack = el("ol", { class: "stall-stack" });
    (s.stack || []).slice().reverse().forEach((frame) => stack.append(el("li", { text: frame })));
    sec.append(el("details", { class: "stall" }, head,
      el("div", { class: "stall-leaf entity", text: s.leafDisplay || "" }), stack));
  }
  return sec;
}

// blockingRows renders a ranked function/context table with a proportional bar.
function blockingRows(title, rows) {
  const sec = el("div", { class: "bd-section" });
  sec.append(el("h3", { text: title }));
  if (!rows || !rows.length) {
    sec.append(el("p", { class: "bd-empty muted-cell", text: "none" }));
    return sec;
  }
  const max = Math.max(...rows.map((r) => r.blockedMicros), 1);
  const tbody = el("tbody");
  tbody.append(el("tr", { class: "bd-colhead muted-cell" },
    el("td", {}), el("td", { text: "blocked" }), el("td", { text: "episodes" }), el("td", { text: "worst" }),
  ));
  for (const r of rows) {
    const nameCell = el("td", { class: "bd-name" });
    const w = Math.max(0, Math.min(1, r.blockedMicros / max)) * 100;
    nameCell.append(el("span", { class: "bd-bar", style: `width:${w.toFixed(1)}%` }));
    nameCell.append(el("span", { class: "bd-label entity", title: r.display }, document.createTextNode(r.display)));
    tbody.append(el("tr", {},
      nameCell,
      el("td", { text: fmtMicros(r.blockedMicros) }),
      el("td", { class: "muted-cell", text: String(r.episodes) }),
      el("td", { class: "muted-cell", text: fmtMicros(r.maxEpisodeMicros) }),
    ));
  }
  sec.append(el("table", { class: "bd-table" }, tbody));
  return sec;
}

// ---------- Progress bar ----------
function showProgress(done, total) {
  const wrap = $("#progress");
  wrap.classList.remove("hidden");
  const pct = total > 0 ? (done / total) * 100 : 0;
  $("#progress-fill").style.width = pct.toFixed(1) + "%";
  $("#progress-text").textContent = total > 0
    ? `Processing ${done} / ${total} profiles`
    : "Preparing…";
}
function hideProgress() {
  $("#progress").classList.add("hidden");
}

function fmtMicros(us) {
  if (us >= 1e6) return (us / 1e6).toFixed(2) + " s";
  if (us >= 1e3) return (us / 1e3).toFixed(1) + " ms";
  return us.toLocaleString(undefined, { maximumFractionDigits: 1 }) + " µs";
}
function isPctMetric(m) {
  return m === "selfPct" || m === "totalPct" || m === "selfPctBusy" || m === "totalPctBusy";
}
function metricValue(cell, metric) {
  switch (metric) {
    case "totalMicros": return cell.totalMicros;
    case "selfSamples": return cell.selfSamples;
    case "totalSamples": return cell.totalSamples;
    case "selfPct": return cell.selfPct;
    case "totalPct": return cell.totalPct;
    case "selfPctBusy": return cell.selfPctBusy;
    case "totalPctBusy": return cell.totalPctBusy;
    default: return cell.selfMicros;
  }
}
function fmtSamples(v) {
  return v.toLocaleString(undefined, { maximumFractionDigits: 1 });
}
function fmtMetric(v, metric) {
  if (isPctMetric(metric)) return v.toFixed(1) + "%";
  return metric.endsWith("Micros") ? fmtMicros(v) : fmtSamples(v);
}

function renderSummaries(matrix) {
  const wrap = $("#summaries");
  wrap.innerHTML = "";
  matrix.summaries.forEach((sm, i) => {
    const color = state.selected.find((s) => gidString(s.id) === gidString(sm.id))?.color || COLORS[i % COLORS.length];
    const profilesLabel = sm.totalProfiles > sm.profileCount
      ? `${sm.profileCount} of ${sm.totalProfiles} (sampled)`
      : String(sm.profileCount);
    const card = el("div", { class: "summary-card", style: `border-left-color:${color}` },
      el("div", { class: "title", text: `${sm.id.service} · ${sm.id.date} · ${sm.id.buildTag}` }),
      kv("Self / profile", fmtMicros(sm.overallMicros)),
      kv("Samples / profile", fmtSamples(sm.overallSamples)),
      kv("Profiles", profilesLabel),
    );
    if (sm.timingApproximate) card.append(el("div", { class: "approx", text: "⚠ timing approximated from hitCounts" }));
    wrap.append(card);
  });
}
function kv(k, v) {
  return el("div", { class: "row" }, el("span", { text: k }), el("b", { text: v }));
}

function renderMatrix(matrix) {
  const result = $("#result");
  result.innerHTML = "";
  if (!matrix.rows.length) {
    showEmpty("No data for this dimension.");
    return;
  }

  const groups = matrix.groups;
  const multi = groups.length >= 2;
  const table = el("table");
  const thead = el("thead");
  const headRow = el("tr");
  const label = state.dim === "overall" ? "" :
    state.dim === "package" ? "Package" :
    state.dim === "file" ? "File" :
    state.dim === "context" ? "Context" : "Function";
  headRow.append(el("th", { text: label || "Metric" }));
  groups.forEach((g, i) => {
    const th = el("th", {
      text: `${g.date} · ${g.buildTag}`,
      onclick: () => sortBy("group", i),
    });
    if (state.sort.kind === "group" && state.sort.col === i) th.classList.add("sorted");
    headRow.append(th);
  });
  if (multi) {
    const first = groups[0], last = groups[groups.length - 1];
    const pp = isPctMetric(state.metric);
    const th = el("th", {
      text: pp ? "Δ pp" : "% Change",
      title: `${pp ? "percentage-point" : "percent"} change from (${first.date} · ${first.buildTag}) to (${last.date} · ${last.buildTag})`,
      onclick: () => sortBy("pct", -1),
    });
    if (state.sort.kind === "pct") th.classList.add("sorted");
    headRow.append(th);
  }
  if (multi) headRow.append(el("th", { text: "Trend" }));
  thead.append(headRow);
  table.append(thead);

  const tbody = el("tbody");
  const rows = sortedRows(matrix);
  for (const row of rows) {
    const tr = el("tr");
    const name = el("td", {});
    const drillTitle = {
      function: "Show callers / callees / contexts",
      package: "Show functions / files / contexts",
      file: "Show functions / contexts",
      context: "Show functions / packages / files",
    }[state.dim];
    if (drillTitle) {
      name.append(el("a", {
        class: "entity drill",
        text: row.display,
        title: drillTitle,
        onclick: () => openBreakdown(state.dim, row.key, row.display),
      }));
    } else {
      name.append(el("span", { class: "entity", text: row.display }));
    }
    if (row.package && state.dim !== "package") {
      name.append(el("span", { class: "pkg-tag", text: "  " + row.package }));
    }
    tr.append(name);

    row.cells.forEach((cell) => {
      const v = metricValue(cell, state.metric);
      tr.append(el("td", { text: fmtMetric(v, state.metric) }));
    });

    if (multi) {
      tr.append(pctChangeCell(row));
      tr.append(el("td", {}, sparkline(row.trend)));
    }
    tbody.append(tr);
  }
  table.append(tbody);
  result.append(table);
}

// changeInfo computes the first→last change of the selected metric. For
// percentage metrics it is a percentage-point delta (pp); otherwise a relative
// percent change. Returns null when not computable.
function changeInfo(row) {
  if (row.cells.length < 2) return null;
  const first = metricValue(row.cells[0], state.metric);
  const last = metricValue(row.cells[row.cells.length - 1], state.metric);
  if (isPctMetric(state.metric)) {
    return { pp: true, value: last - first };
  }
  if (first === 0) return { pp: false, value: last === 0 ? 0 : Infinity };
  return { pp: false, value: ((last - first) / first) * 100 };
}

function pctChangeCell(row) {
  const c = changeInfo(row);
  if (c === null) return el("td", { text: "—" });
  if (c.value === Infinity) return el("td", {}, el("span", { class: "delta-up", text: "new" }));
  const unit = c.pp ? " pp" : "%";
  if (Math.abs(c.value) < 0.05) return el("td", { class: "muted-cell", text: "0" + unit });
  const cls = c.value > 0 ? "delta-up" : "delta-down";
  const arrow = c.value > 0 ? "▲ " : "▼ ";
  return el("td", {}, el("span", { class: cls, text: arrow + Math.abs(c.value).toFixed(1) + unit }));
}

function sortBy(kind, col) {
  if (state.sort.kind === kind && state.sort.col === col) state.sort.dir = -state.sort.dir;
  else state.sort = { kind, col, dir: -1 };
  renderMatrix(state.matrix);
}

function sortedRows(matrix) {
  const rows = matrix.rows.slice();
  const { kind, col, dir } = state.sort;
  if (kind === "pct") {
    rows.sort((a, b) => (pctRank(a) - pctRank(b)) * dir);
    return rows;
  }
  if (col < 0) return rows;
  rows.sort((a, b) => {
    const av = metricValue(a.cells[col], state.metric);
    const bv = metricValue(b.cells[col], state.metric);
    return (av - bv) * dir;
  });
  return rows;
}

// pctRank maps a row's change to a sortable number (null/NaN sort last).
function pctRank(row) {
  const c = changeInfo(row);
  if (c === null || Number.isNaN(c.value)) return -Infinity;
  return c.value;
}

// ---------- Sparkline ----------
function sparkline(values) {
  const w = 90, h = 22, pad = 2;
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("width", w);
  svg.setAttribute("height", h);
  svg.setAttribute("class", "spark");
  const max = Math.max(...values, 1);
  if (values.length === 1) {
    const bar = document.createElementNS(svg.namespaceURI, "rect");
    const bh = (values[0] / max) * (h - pad * 2);
    bar.setAttribute("x", w / 2 - 6); bar.setAttribute("width", 12);
    bar.setAttribute("y", h - pad - bh); bar.setAttribute("height", Math.max(bh, 1));
    bar.setAttribute("fill", "#5a9bd8");
    svg.append(bar);
    return svg;
  }
  const step = (w - pad * 2) / (values.length - 1);
  const pts = values.map((v, i) => {
    const x = pad + i * step;
    const y = h - pad - (v / max) * (h - pad * 2);
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(" ");
  const poly = document.createElementNS(svg.namespaceURI, "polyline");
  poly.setAttribute("points", pts);
  poly.setAttribute("fill", "none");
  poly.setAttribute("stroke", "#6cc7a3");
  poly.setAttribute("stroke-width", "1.5");
  svg.append(poly);
  return svg;
}

function showEmpty(msg) {
  $("#result").innerHTML = "";
  $("#result").append(el("p", { class: "empty", text: msg }));
}

// ---------- Upload ----------
async function uploadFiles(fileList) {
  const files = [...fileList];
  if (!files.length) return;
  const fd = new FormData();
  for (const f of files) fd.append("files", f);
  const date = $("#up-date").value.trim();
  const buildTag = $("#up-buildtag").value.trim();
  const service = $("#up-service").value.trim();
  if (date) fd.append("date", date);
  if (buildTag) fd.append("buildTag", buildTag);
  if (service) fd.append("service", service);

  const status = $("#upload-status");
  status.textContent = `Uploading ${files.length} file(s)…`;
  try {
    const res = await api("/api/upload", { method: "POST", body: fd });
    status.textContent = `Added ${res.group.members.length} file(s) to ${gidString(res.group.id)}`;
    addSelected(res.group.id);
  } catch (e) {
    status.textContent = "Upload failed: " + e.message;
  }
}

// ---------- Entity breakdown modal ----------
// Drilling any entity row opens a centered modal whose sections adapt to the
// dimension: a function shows callers / callees / contexts; a package its
// functions / files / contexts; a file its functions / contexts; a context its
// functions / packages / files. Names inside the modal are themselves drillable,
// so you can navigate the graph. One selected group at a time; the group
// selector (and, for functions, the stitch toggle) re-fetch.
const bdState = { dim: "function", key: null, display: "", groupIdx: 0, stitch: true, contextSort: "micros" };

function seg(s) { return encodeURIComponent(s); }

function ensureDrawer() {
  if ($("#bd-drawer")) return;
  document.body.append(
    el("div", { id: "bd-backdrop", class: "drawer-backdrop hidden", onclick: closeBreakdown }),
    el("div", { id: "bd-drawer", class: "drawer hidden" }),
  );
}

function closeBreakdown() {
  $("#bd-backdrop")?.classList.add("hidden");
  $("#bd-drawer")?.classList.add("hidden");
}

function openBreakdown(dim, key, display) {
  if (!state.selected.length) return;
  bdState.dim = dim || "function";
  bdState.key = key;
  bdState.display = display;
  if (bdState.groupIdx >= state.selected.length) bdState.groupIdx = 0;
  ensureDrawer();
  $("#bd-backdrop").classList.remove("hidden");
  $("#bd-drawer").classList.remove("hidden");
  refreshBreakdown();
}

// dimLabel names the kind of entity being drilled, for the modal header.
function dimLabel(dim) {
  return { package: "Package", file: "File", context: "Context" }[dim] || "Function";
}

async function refreshBreakdown() {
  const drawer = $("#bd-drawer");
  drawer.innerHTML = "";

  drawer.append(el("div", { class: "bd-head" },
    el("div", { class: "bd-title-wrap" },
      el("div", { class: "bd-kind", text: dimLabel(bdState.dim) }),
      el("div", { class: "bd-title entity", text: bdState.display }),
    ),
    el("span", { class: "x", text: "✕", title: "Close (Esc)", onclick: closeBreakdown }),
  ));

  const controls = el("div", { class: "bd-controls" });
  if (state.selected.length > 1) {
    const sel = el("select", {
      onchange: (e) => { bdState.groupIdx = Number(e.target.value); refreshBreakdown(); },
    });
    state.selected.forEach((s, i) => {
      const opt = el("option", { value: String(i), text: s.label });
      if (i === bdState.groupIdx) opt.selected = true;
      sel.append(opt);
    });
    controls.append(el("label", {}, document.createTextNode("Group "), sel));
  }
  // Stitching only applies to a function's callers.
  if (bdState.dim === "function") {
    const stitchCb = el("input", {
      type: "checkbox",
      onchange: (e) => { bdState.stitch = e.target.checked; refreshBreakdown(); },
    });
    stitchCb.checked = bdState.stitch;
    controls.append(el("label", {
      title: "Resolve callers through async/native trampoline frames (e.g. runMicrotasks) up to the nearest real frame",
    }, stitchCb, document.createTextNode(" Stitch async")));
  }
  drawer.append(controls);

  const body = el("div", { class: "bd-body" }, el("p", { class: "empty", text: "Loading…" }));
  drawer.append(body);

  const sel = state.selected[bdState.groupIdx];
  if (!sel) return;
  const id = sel.id;
  const cats = enabledCategories();
  const catParam = cats.length ? `&categories=${cats.join(",")}` : "";
  const path = `/api/group/${seg(id.env)}/${seg(id.service)}/${seg(id.date)}/${seg(id.buildTag)}`
    + `/breakdown?dim=${seg(bdState.dim)}&key=${encodeURIComponent(bdState.key)}&stitch=${bdState.stitch}`
    + `&contextSort=${bdState.contextSort}&topN=50${catParam}`;
  try {
    const bd = await api(path);
    body.innerHTML = "";
    if (bd.package) body.append(el("div", { class: "bd-pkg pkg-tag", text: bd.package }));
    renderBdSections(body, bd);
  } catch (e) {
    body.innerHTML = "";
    body.append(el("div", { class: "error", text: e.message }));
  }
}

// renderBdSections lays out the sections appropriate to the drilled dimension.
// "self" sections chart self time (the partitioning figure); "total" sections
// chart inclusive time. showPct surfaces each row's share of the route.
function renderBdSections(body, bd) {
  switch (bd.dimension) {
    case "package":
      body.append(bdEdgeSection("Functions", bd.functions, { value: "self", drill: "function" }));
      body.append(bdEdgeSection("Files", bd.files, { value: "self", drill: "file" }));
      body.append(bdEdgeSection("Contexts", bd.contexts, { value: "self", showPct: true, drill: "context" }));
      break;
    case "file":
      body.append(bdEdgeSection("Functions", bd.functions, { value: "self", drill: "function" }));
      body.append(bdEdgeSection("Contexts", bd.contexts, { value: "self", showPct: true, drill: "context" }));
      break;
    case "context":
      body.append(bdEdgeSection("Functions", bd.functions, { value: "total", showPct: true, drill: "function" }));
      body.append(bdEdgeSection("Packages", bd.packages, { value: "self", showPct: true, drill: "package" }));
      body.append(bdEdgeSection("Files", bd.files, { value: "self", showPct: true, drill: "file" }));
      break;
    default: // function
      body.append(bdEdgeSection("Callers", bd.callers, { value: "total", showAsync: true, drill: "function" }));
      body.append(bdEdgeSection("Callees", bd.callees, { value: "total", drill: "function" }));
      body.append(bdContextSection(bd.contexts));
  }
}

// bdEdgeSection renders one breakdown list. opts.value picks self vs inclusive
// time for the bar and "time" column; opts.showPct shows each row's share of the
// route instead of samples; opts.showAsync tags stitched callers; opts.drill, if
// set, makes each name drill into that dimension.
function bdEdgeSection(title, edges, opts = {}) {
  const { value = "total", showAsync = false, showPct = false, drill = null } = opts;
  const valOf = (e) => (value === "self" ? e.selfMicros || 0 : e.totalMicros || 0);
  const sampOf = (e) => (value === "self" ? e.selfSamples || 0 : e.totalSamples || 0);
  const sec = el("div", { class: "bd-section" });
  sec.append(el("h3", { text: title }));
  if (!edges || !edges.length) {
    sec.append(el("p", { class: "bd-empty muted-cell", text: "none" }));
    return sec;
  }
  const max = Math.max(...edges.map(valOf), 1);
  const tbody = el("tbody");
  tbody.append(el("tr", { class: "bd-colhead muted-cell" },
    el("td", {}),
    el("td", { text: "time" }),
    el("td", showPct
      ? { text: "% of route", title: "this entity's share of the route's own CPU" }
      : { text: "samples" }),
  ));
  for (const e of edges) {
    tbody.append(el("tr", {},
      bdNameCell(e, valOf(e) / max, showAsync, drill),
      el("td", { text: fmtMicros(valOf(e)) }),
      showPct
        ? el("td", { text: fmtPct(e.pctOfContext) })
        : el("td", { class: "muted-cell", text: fmtSamples(sampOf(e)) }),
    ));
  }
  sec.append(el("table", { class: "bd-table" }, tbody));
  return sec;
}

// bdNameCell builds the first cell of a breakdown row: a proportional background
// bar (frac in 0..1, clamped) behind the name. The name is clamped to two lines
// with the full string on hover, so long file paths / routes don't blow up row
// height and the numeric columns stay aligned. When drillDim is set, the name is
// a link that re-opens the modal on that entity, so the graph is navigable.
function bdNameCell(e, frac, showAsync, drillDim) {
  const cell = el("td", { class: "bd-name" });
  const w = Math.max(0, Math.min(1, frac || 0)) * 100;
  cell.append(el("span", { class: "bd-bar", style: `width:${w.toFixed(1)}%` }));
  const label = el("span", { class: "bd-label", title: e.display });
  label.append(drillDim
    ? el("a", { class: "entity drill", text: e.display, title: "Drill into this " + drillDim, onclick: () => openBreakdown(drillDim, e.key, e.display) })
    : el("span", { class: "entity", text: e.display }));
  if (e.package) label.append(el("span", { class: "pkg-tag", text: e.package }));
  if (showAsync && e.viaAsync) label.append(el("span", { class: "async-tag", text: "async" }));
  cell.append(label);
  return cell;
}

// bdContextSection renders the logical owners (routes/jobs) of the function with
// both shares: pctOfFunction (this route's slice of the function) and
// pctOfContext (the function's slice of the route's own CPU). A sort toggle
// re-fetches so the topN cap keeps the right rows.
function bdContextSection(edges) {
  const sec = el("div", { class: "bd-section" });
  const head = el("div", { class: "bd-sec-head" });
  head.append(el("h3", { text: "Contexts" }));
  if (edges && edges.length) {
    const sel = el("select", {
      class: "bd-ctxsort",
      title: "Order contexts by absolute time, or by each route's share (the function's share of that route's own CPU)",
      onchange: (e) => { bdState.contextSort = e.target.value; refreshBreakdown(); },
    });
    [["micros", "by time"], ["pctOfContext", "by route share"]].forEach(([v, label]) => {
      const opt = el("option", { value: v, text: label });
      if (v === bdState.contextSort) opt.selected = true;
      sel.append(opt);
    });
    head.append(sel);
  }
  sec.append(head);

  if (!edges || !edges.length) {
    sec.append(el("p", { class: "bd-empty muted-cell", text: "none (no async-context data)" }));
    return sec;
  }
  // The background bar tracks whatever the rows are sorted by, so its length and
  // the row order agree: time when sorted "by time", route share when sorted "by
  // route share".
  const byRouteShare = bdState.contextSort === "pctOfContext";
  const barOf = byRouteShare ? (e) => e.pctOfContext || 0 : (e) => e.totalMicros;
  const max = Math.max(...edges.map(barOf), 1);
  const tbody = el("tbody");
  tbody.append(el("tr", { class: "bd-colhead muted-cell" },
    el("td", {}),
    el("td", { class: byRouteShare ? "" : "bd-sortcol", text: "time" }),
    el("td", { text: "% of fn", title: "this route's share of the function's total inclusive time" }),
    el("td", { class: byRouteShare ? "bd-sortcol" : "", text: "% of route", title: "the function's share of this route's own busy CPU — how much optimizing it would save the route" }),
  ));
  for (const e of edges) {
    tbody.append(el("tr", {},
      bdNameCell(e, barOf(e) / max, false, "context"),
      el("td", { text: fmtMicros(e.totalMicros) }),
      el("td", { class: "muted-cell", text: fmtPct(e.pctOfFunction) }),
      el("td", { text: fmtPct(e.pctOfContext) }),
    ));
  }
  sec.append(el("table", { class: "bd-table bd-ctx-table" }, tbody));
  return sec;
}

// fmtPct renders a percentage value (already 0-100), or an em dash when absent.
function fmtPct(v) {
  if (!v) return "—";
  return v.toFixed(1) + "%";
}

// ---------- Wiring ----------
function init() {
  api("/api/health").then((h) => {
    const badge = $("#gcs-status");
    badge.textContent = h.gcsEnabled ? "GCS connected" : "upload-only";
    badge.classList.add(h.gcsEnabled ? "on" : "off");
  }).catch(() => {});

  loadBrowse();
  renderSelected();

  $("#tabs").addEventListener("click", (e) => {
    const btn = e.target.closest(".tab");
    if (!btn) return;
    document.querySelectorAll(".tab").forEach((t) => t.classList.remove("active"));
    btn.classList.add("active");
    state.dim = btn.dataset.dim;
    if (state.selected.length) runCompare();
  });

  $("#metric").addEventListener("change", (e) => {
    state.metric = e.target.value;
    // Re-fetch so server-side Top-N ranking and trend match the chosen metric.
    if (state.selected.length) runCompare();
  });
  $("#topn").addEventListener("change", (e) => { state.topN = e.target.value; });
  $("#refresh").addEventListener("click", runCompare);

  $("#filters").addEventListener("change", (e) => {
    const cb = e.target.closest("input[data-cat]");
    if (!cb) return;
    state.categories[cb.dataset.cat] = cb.checked;
    if (state.selected.length) runCompare();
  });
  $("#clear-selected").addEventListener("click", () => { state.selected = []; renderSelected(); });

  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeBreakdown(); });

  const dz = $("#dropzone");
  const input = $("#file-input");
  dz.addEventListener("click", () => input.click());
  input.addEventListener("change", () => uploadFiles(input.files));
  dz.addEventListener("dragover", (e) => { e.preventDefault(); dz.classList.add("drag"); });
  dz.addEventListener("dragleave", () => dz.classList.remove("drag"));
  dz.addEventListener("drop", (e) => {
    e.preventDefault();
    dz.classList.remove("drag");
    uploadFiles(e.dataTransfer.files);
  });
}

init();

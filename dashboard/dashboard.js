"use strict";

const REFRESH_SECONDS = 5;

// el builds a DOM node safely. Text content is set via textContent, never
// innerHTML, so container names and images from Docker can't inject markup.
function el(tag, props, ...children) {
  const node = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === null || v === undefined) continue;
      if (k === "class") node.className = v;
      else if (k === "title") node.title = v;
      else node.setAttribute(k, v);
    }
  }
  for (const c of children) {
    if (c === null || c === undefined) continue;
    node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return node;
}

// fmtRelative renders an ISO timestamp as a compact "x ago" / "in x" string.
function fmtRelative(iso) {
  if (!iso) return null;
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return null;
  let secs = Math.round((Date.now() - then) / 1000);
  const future = secs < 0;
  secs = Math.abs(secs);
  const txt = humanDuration(secs);
  if (txt === "0s") return "just now";
  return future ? "in " + txt : txt + " ago";
}

function humanDuration(secs) {
  const units = [
    ["d", 86400],
    ["h", 3600],
    ["m", 60],
    ["s", 1],
  ];
  for (const [label, size] of units) {
    if (secs >= size) return Math.floor(secs / size) + label;
  }
  return "0s";
}

// uptimeText strips the health parenthetical from Docker's status string,
// leaving e.g. "Up 2 hours".
function uptimeText(c) {
  if (c.state !== "running") return null;
  return (c.status || "").replace(/\s*\(health[^)]*\)\s*/i, "").replace(/\s*\((un)?healthy\)\s*/i, "").trim() || null;
}

function stateCell(c) {
  let dot = "dot-gray";
  if (c.health === "healthy") dot = "dot-green";
  else if (c.health === "unhealthy") dot = "dot-red";
  else if (c.health === "starting") dot = "dot-amber";
  else if (c.state === "running") dot = "dot-green";
  else if (c.state === "exited" || c.state === "dead") dot = "dot-red";

  const children = [
    el("span", { class: "dot " + dot }),
    el("span", { class: "state-text" }, c.state || "unknown"),
  ];
  if (c.health) children.push(el("div", { class: "health-sub" }, c.health));
  return el("td", null, ...children);
}

// restartsCell renders Docker's restart count for the container. A nil count
// (container could not be inspected) shows "—"; zero is dimmed as the unremarkable
// healthy case; a positive count is highlighted to flag instability.
function restartsCell(c) {
  const n = c.restarts;
  if (n === undefined || n === null) return el("td", { class: "up-na" }, "—");
  if (n === 0) return el("td", { class: "up-na" }, "0");
  const title = "Restarted " + n + " time" + (n !== 1 ? "s" : "") + " since the container was last (re)created";
  return el("td", { class: "restarts-warn", title }, String(n));
}

function autoUpdateCell(c) {
  if (!c.auto_update) {
    return el("td", null, el("span", { class: "badge badge-manual", title: "Not monitored by docker-updater" }, "Manual"));
  }
  const cls = c.mode === "git" ? "badge badge-git" : "badge badge-auto";
  return el("td", null, el("span", { class: cls }, "Auto · " + (c.mode || "image")));
}

function upstreamCell(c) {
  const td = el("td", null);
  if (!c.auto_update) {
    td.appendChild(el("span", { class: "up-na" }, "—"));
    return td;
  }
  if (c.error) {
    td.appendChild(el("span", { class: "up-error" }, "error"));
    td.appendChild(el("div", { class: "detail" }, c.error));
    return td;
  }
  if (c.update_available) {
    td.appendChild(el("span", { class: "up-available" }, "update available"));
    let detail = "";
    if (c.current_ref || c.available_ref) {
      detail = (c.current_ref || "?") + " → " + (c.available_ref || "?");
    }
    if (c.skipped) detail = (detail ? detail + " · " : "") + "skipped: " + (c.skip_reason || "pre-check");
    if (detail) td.appendChild(el("div", { class: "detail" }, detail));
    return td;
  }
  // Up to date.
  td.appendChild(el("span", { class: "up-uptodate" }, "up to date"));
  const updated = fmtRelative(c.last_updated);
  if (updated) td.appendChild(el("div", { class: "detail" }, "updated " + updated));
  return td;
}

function row(c) {
  const nameCell = el("td", null,
    el("span", { class: "cname" }, c.name || "—"),
    el("div", { class: "cmeta" },
      c.image || "—",
      c.image_id ? " · " : null,
      c.image_id ? el("span", { class: "cref" }, c.image_id) : null,
    ),
  );

  const uptime = uptimeText(c);
  const lastChecked = c.auto_update ? (fmtRelative(c.last_checked) || "—") : "—";
  const lastPulled = c.auto_update ? (fmtRelative(c.last_pulled) || "—") : "—";

  return el("tr", null,
    nameCell,
    autoUpdateCell(c),
    stateCell(c),
    el("td", { class: uptime ? null : "up-na" }, uptime || "—"),
    restartsCell(c),
    el("td", { class: lastChecked === "—" ? "up-na" : null }, lastChecked),
    el("td", { class: lastPulled === "—" ? "up-na" : null }, lastPulled),
    upstreamCell(c),
  );
}

function setText(id, text) {
  const node = document.getElementById(id);
  if (node) node.textContent = text;
}

function render(data) {
  const containers = data.containers || [];

  const auto = containers.filter((c) => c.auto_update).length;
  const updates = containers.filter((c) => c.auto_update && c.update_available).length;
  const errors = containers.filter((c) => c.auto_update && c.error).length;

  setText("stat-total", containers.length);
  setText("stat-auto", auto);
  setText("stat-manual", containers.length - auto);
  setText("stat-updates", updates);
  setText("stat-errors", errors);

  document.getElementById("dry-run-badge").classList.toggle("hidden", !data.dry_run);
  setText("cfg-interval", data.interval || "—");
  setText("cfg-label", data.label || "—");
  setText("refresh-interval", REFRESH_SECONDS);

  const lastCycle = fmtRelative(data.last_cycle);
  setText("last-cycle", lastCycle ? "Last check " + lastCycle : "No check yet");
  const nextCycle = fmtRelative(data.next_cycle);
  setText("next-cycle", nextCycle ? "Next check " + nextCycle : "");
  setText("refreshed", "Updated " + new Date(data.generated_at).toLocaleTimeString());

  const isExited = (c) => c.state === "exited" || c.state === "dead";
  const active = containers.filter((c) => !isExited(c));
  const exited = containers.filter(isExited);

  const tbody = document.getElementById("rows");
  tbody.replaceChildren(...active.map(row));

  const exitedTbody = document.getElementById("exited-rows");
  exitedTbody.replaceChildren(...exited.map(row));

  const exitedSection = document.getElementById("exited-section");
  exitedSection.classList.toggle("hidden", exited.length === 0);
  document.getElementById("exited-summary").textContent =
    exited.length + " exited container" + (exited.length !== 1 ? "s" : "");

  document.getElementById("empty").classList.toggle("hidden", containers.length > 0);
}

async function refresh() {
  const banner = document.getElementById("error-banner");
  try {
    const resp = await fetch("api/containers", { cache: "no-store" });
    if (!resp.ok) throw new Error("HTTP " + resp.status + ": " + (await resp.text()));
    render(await resp.json());
    banner.classList.add("hidden");
  } catch (err) {
    banner.textContent = "Failed to load container data: " + err.message;
    banner.classList.remove("hidden");
  }
}

refresh();
setInterval(refresh, REFRESH_SECONDS * 1000);

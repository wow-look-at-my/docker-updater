// This file is the source of truth for the dashboard's behaviour.

const REFRESH_SECONDS = 5;

// A row whose container was updated (recreated on a new image/commit/ base) within this window gets a green background that fades linearly.
const UPDATED_HIGHLIGHT_MS = 10 * 60 * 1000;
const UPDATED_HIGHLIGHT_MAX_ALPHA = 0.22;
// rgb of --green (#3fb950) in dashboard.css.
const GREEN_RGB = "63,185,80";

// ApiContainer mirrors the JSON emitted by apiContainer in dashboard.go. Keeping
// it in sync is the whole point of the TypeScript rewrite. A renamed or
// wrong-typed field here is a compile error instead of a silent `undefined`.
interface ApiContainer {
  name: string;
  image: string;
  image_id: string;
  // The commit the running image was built from, and its page, when the image's OCI labels name one.
  commit?: string;
  commit_url?: string;
  state: string;
  status: string;
  health: string;
  created: number;
  restarts?: number | null;
  auto_update: boolean;
  mode?: string;
  // Who keeps this container current: "docker-updater", "watchtower", "both", or absent when nothing updates it.
  updater?: string;
  // Still served by /api/containers, but not rendered: every monitored container is checked in the same cycle.
  last_checked?: string | null;
  last_pulled?: string | null;
  last_updated?: string | null;
  update_available: boolean;
  current_ref?: string;
  available_ref?: string;
  // The commit the available image was built from, and its page.
  available_commit?: string;
  available_commit_url?: string;
  error?: string;
  skipped?: boolean;
  skip_reason?: string;
  // Why a container carrying the enable label can never be checked as it stands.
  unmonitorable?: string;
  // Present once the updater has failed to move this container for more than one consecutive cycle: how many.
  stuck_cycles?: number;
  stuck_since?: string | null;
  // Configuration notes, not failures: no standard /.well-known/docker-updater/ endpoints.
  warnings?: string[];
  // The last line a failing container logged. Served only for a container that is restarting or exited non-zero.
  log_excerpt?: string;
  // Set when the running config no longer matches the compose file, or when nothing could compare them.
  compose_drift?: ApiComposeDrift;
  // When an operator queued a recreate from the compose file. Absent when none is queued.
  recreate_queued?: string | null;
}

// ApiComposeDrift mirrors ComposeDrift in compose_drift.go.
interface ApiComposeDrift {
  state: string;
  service: string;
  config_files: string[];
  running_hash?: string;
  file_hash?: string;
  command: string;
  error?: string;
  // Why the updater cannot apply the file itself. The operator must run command.
  blocked?: string;
  // The fields where the file and the container disagree, or why they could not be listed.
  differences?: ApiComposeDifference[];
  differences_error?: string;
}

// ApiComposeDifference mirrors ComposeDifference in compose_diff.go.
interface ApiComposeDifference {
  field: string;
  file: string;
  container: string;
}

// ApiResponse mirrors apiResponse in dashboard.go (the payload of /api/containers).
interface ApiResponse {
  generated_at: string;
  interval: string;
  dry_run: boolean;
  label: string;
  version?: string;
  last_cycle?: string | null;
  next_cycle?: string | null;
  // Base URL of the simple-stats-api the resource graphs poll. Absent = none configured.
  stats_url?: string;
  containers: ApiContainer[];
}

type Props = Record<string, string | null | undefined> | null;
type Child = string | Node | null | undefined;

// el builds a DOM node safely. Text content is set via textContent, never
// innerHTML, so container names and images from Docker can't inject markup.
function el(tag: string, props?: Props, ...children: Child[]): HTMLElement {
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
function fmtRelative(iso: string | null | undefined): string | null {
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

function humanDuration(secs: number): string {
  const units: [string, number][] = [
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
function uptimeText(c: ApiContainer): string | null {
  if (c.state !== "running") return null;
  return (c.status || "").replace(/\s*\(health[^)]*\)\s*/i, "").replace(/\s*\((un)?healthy\)\s*/i, "").trim() || null;
}

// The text of each table cell lives in its own function, and both the cell
// builder and stateSnapshot() call it. Anything the table draws is therefore
// reachable by the copy button by construction. A value the snapshot could
// omit is a value no column can render either.

// stateIndicator is the colour of the row's state dot: the reported health
// when Docker has one, else the container's own state.
function stateIndicator(c: ApiContainer): string {
  if (c.health === "healthy") return "green";
  if (c.health === "unhealthy") return "red";
  if (c.health === "starting") return "amber";
  if (c.state === "running") return "green";
  // Restarting is red for the same reason it is offline: the state persists for a whole crash loop.
  if (c.state === "exited" || c.state === "dead" || c.state === "restarting") return "red";
  return "gray";
}

function stateText(c: ApiContainer): string {
  return c.state || "unknown";
}

function stateCell(c: ApiContainer): HTMLElement {
  const children: Child[] = [
    el("span", { class: "dot dot-" + stateIndicator(c) }),
    el("span", { class: "state-text" }, stateText(c)),
  ];
  if (c.health) children.push(el("div", { class: "health-sub" }, c.health));
  return el("td", null, ...children);
}

// restartsText is the Restarts column: Docker's restart count, or "—" when the
// container could not be inspected.
function restartsText(c: ApiContainer): string {
  const n = c.restarts;
  return n === undefined || n === null ? "—" : String(n);
}

// restartsCell dims the unremarkable cases (uninspectable, zero) and highlights
// a positive count to flag instability.
function restartsCell(c: ApiContainer): HTMLElement {
  const n = c.restarts;
  const text = restartsText(c);
  if (n === undefined || n === null || n === 0) return el("td", { class: "up-na" }, text);
  const title = "Restarted " + n + " time" + (n !== 1 ? "s" : "") + " since the container was last (re)created";
  return el("td", { class: "restarts-warn", title }, text);
}

// watchtowerOwns mirrors the Go predicate: watchtower keeps this container
// current, whether docker-updater claims it as well.
function watchtowerOwns(c: ApiContainer): boolean {
  return c.updater === "watchtower" || c.updater === "both";
}

// monitoringText is the Auto-update column's badge text.
function monitoringText(c: ApiContainer): string {
  if (c.updater === "both") return "Both";
  if (c.auto_update) return "Auto · " + (c.mode || "image");
  if (watchtowerOwns(c)) return "Watchtower";
  return "Manual";
}

// autoUpdateCell says who keeps the container current. Reporting watchtower's
// containers as "Manual" was the defect. They ARE updated, so the empty columns
// beside them read as neglect and send the operator after a missing label.
function autoUpdateCell(c: ApiContainer): HTMLElement {
  if (c.updater === "both") {
    const title = "Claimed by docker-updater and watchtower at once: each will recreate what the other just made. Remove one of the two labels.";
    return el("td", null, el("span", { class: "badge badge-conflict", title }, monitoringText(c)));
  }
  if (c.auto_update) {
    const cls = c.mode === "git" ? "badge badge-git" : "badge badge-auto";
    return el("td", null, el("span", { class: cls }, monitoringText(c)));
  }
  if (watchtowerOwns(c)) {
    const title = "Updated by watchtower, not by docker-updater. The columns to the right are this updater's own checks, so they stay empty for it.";
    return el("td", null, el("span", { class: "badge badge-watchtower", title }, monitoringText(c)));
  }
  return el("td", null, el("span", { class: "badge badge-manual", title: "No updater claims this container" }, monitoringText(c)));
}

// lastPulledText is the Last pulled column: how long ago a newer image was
// downloaded. Unmonitored containers are never pulled.
function lastPulledText(c: ApiContainer): string {
  return c.auto_update ? (fmtRelative(c.last_pulled) || "—") : "—";
}

// heldBackReason explains why a detected update has not been applied.
function heldBackReason(c: ApiContainer): string {
  if (c.error) return "error: " + c.error;
  if (c.skipped) return "skipped: " + (c.skip_reason || "pre-check");
  return "dry-run — not applied";
}

// UpstreamView is the Upstream column.
interface UpstreamView {
  cls: string;
  status: string;
  detail: string | null;
}

// stuckPrefix opens the detail line of a container the updater has failed to
// move for more than one cycle.
function stuckPrefix(c: ApiContainer): string {
  if (!c.stuck_cycles) return "";
  const since = fmtRelative(c.stuck_since);
  return "STUCK" + (since ? " " + since : "") + " (" + c.stuck_cycles + " cycles) · ";
}

// recreateWaits says what holds a queued recreate back. Dry-run mode holds every one until it is forced.
function recreateWaits(c: ApiContainer, dryRun: boolean): string {
  if (c.error || c.skipped) return heldBackReason(c);
  return dryRun ? "dry-run — not applied" : "waits for the next check";
}

function upstreamView(c: ApiContainer, dryRun: boolean): UpstreamView {
  // This updater never checks a watchtower container, so it has no verdict to
  // give.
  if (!c.auto_update && watchtowerOwns(c)) {
    return { cls: "up-na", status: "watchtower's", detail: "not checked here" };
  }
  if (!c.auto_update) return { cls: "up-na", status: "—", detail: null };
  // Terminal, so it precedes every check outcome: nothing polls this
  // container.
  if (c.unmonitorable) {
    return { cls: "up-error", status: "cannot update", detail: stuckPrefix(c) + c.unmonitorable };
  }
  // An available update means one was detected but held back. Always surface it
  // as such — even when it also errored — so the row matches the "updates
  // pending" count instead of hiding behind a bare "error".
  if (c.update_available) {
    let detail = "";
    if (c.current_ref || c.available_ref) {
      detail = (c.current_ref || "?") + " → " + (c.available_ref || "?");
    }
    const reason = heldBackReason(c);
    return { cls: "up-available", status: "update available", detail: stuckPrefix(c) + (detail ? detail + " · " + reason : reason) };
  }
  // The image is current, but the compose file changed. Nothing applies it until an operator queues a recreate.
  if (outOfSync(c)) {
    if (!c.recreate_queued) {
      return { cls: "up-drift", status: "out of sync", detail: "compose file changed · not applied until a recreate is queued" };
    }
    return { cls: "up-drift", status: "recreate queued", detail: stuckPrefix(c) + "queued " + (fmtRelative(c.recreate_queued) || "") + " · " + recreateWaits(c, dryRun) };
  }
  // Errored with no newer ref detected: a plain failure, not a pending update.
  if (c.error) return { cls: "up-error", status: "error", detail: stuckPrefix(c) + c.error };
  // Up to date.
  const updated = fmtRelative(c.last_updated);
  return { cls: "up-uptodate", status: "up to date", detail: updated ? "updated " + updated : null };
}

// commitLink is a short commit id that opens the commit's page. Without a
// page (an image whose labels name no source) it is plain text.
function commitLink(sha: string, url?: string): HTMLElement {
  const short = sha.slice(0, 12);
  if (!url) return el("span", { class: "cref" }, short);
  return el("a", { class: "cref commit", href: url, target: "_blank", rel: "noopener", title: sha }, short);
}

// differenceLines lists a drift's differences as "field: container → file".
function differenceLines(d: ApiComposeDrift): string[] {
  if (d.differences_error) return ["differences unavailable: " + d.differences_error];
  const diffs = d.differences || [];
  if (diffs.length === 0) {
    return ["the container matches what compose would create. The hash also covers settings compose applies outside the create request, such as post_start hooks"];
  }
  return diffs.map((x) => x.field + ": " + x.container + " → " + x.file);
}

// recreateWhatText names the file and its differences: the change the operator is about to approve.
function recreateWhatText(d: ApiComposeDrift): string {
  return "\nservice " + d.service + " in " + d.config_files.join(", ") +
    "\n\nThe file changes (container → file):\n" + differenceLines(d).map((l) => "  " + l).join("\n");
}

// queueConfirmText is the question the recreate button asks first.
function queueConfirmText(c: ApiContainer, d: ApiComposeDrift): string {
  return "Queue a recreate of " + c.name + " from its compose file?" + recreateWhatText(d) +
    "\n\nIt runs at the next check, and waits for the pre-update check and dry-run mode like an update. " +
    "The file replaces the container's config, mounts included, and compose cannot restore the old one.";
}

// forceConfirmText is the question the force button asks first. It names the
// gate the action skips, because that gate is the reason it waits.
function forceConfirmText(c: ApiContainer): string {
  if (!pending(c) && c.compose_drift) {
    return "Recreate " + c.name + " from its compose file now?" + recreateWhatText(c.compose_drift) +
      "\n\nHeld back by: " + recreateWaits(c, Boolean(latestData && latestData.dry_run)) +
      "\n\nThe recreate skips the pre-update check and dry-run mode. The file replaces the container's config, mounts included, and compose cannot restore the old one.";
  }
  const refs = c.current_ref || c.available_ref
    ? "\n" + (c.current_ref || "?") + " → " + (c.available_ref || "?")
    : "";
  return "Force-update " + c.name + " now?" + refs +
    "\n\nHeld back by: " + heldBackReason(c) +
    "\n\nThe update skips the pre-update check and dry-run mode, and recreates the container.";
}

// onForceUpdate asks for confirmation, then queues the update. The outcome of
// the update itself shows on a later poll, in the Upstream column.
async function onForceUpdate(c: ApiContainer): Promise<void> {
  if (!window.confirm(forceConfirmText(c))) return;
  forceRequests[c.name] = "queuing…";
  repaint();
  try {
    const res = await fetch("api/force-update?container=" + encodeURIComponent(c.name), {
      method: "POST",
      headers: { "X-Docker-Updater-Action": "force-update" },
    });
    forceRequests[c.name] = res.ok ? "queued" : "failed: HTTP " + res.status + ": " + (await res.text()).trim();
  } catch (err) {
    forceRequests[c.name] = "failed: " + String(err);
  }
  repaint();
}

// onRecreate queues (POST) or cancels (DELETE) a recreate from the compose
// file, then polls at once so the row shows the server's new state.
async function onRecreate(c: ApiContainer, method: "POST" | "DELETE"): Promise<void> {
  if (method === "POST" && c.compose_drift && !window.confirm(queueConfirmText(c, c.compose_drift))) return;
  recreateRequests[c.name] = method === "POST" ? "queuing…" : "cancelling…";
  repaint();
  try {
    const res = await fetch("api/recreate?container=" + encodeURIComponent(c.name), {
      method,
      headers: { "X-Docker-Updater-Action": "recreate" },
    });
    if (res.ok) delete recreateRequests[c.name];
    else recreateRequests[c.name] = "failed: HTTP " + res.status + ": " + (await res.text()).trim();
  } catch (err) {
    recreateRequests[c.name] = "failed: " + String(err);
  }
  await refresh();
}

// forceButton applies a held-back update, or a queued recreate from the
// compose file, now. While a request is in flight or queued, the button shows
// that state and does nothing.
function forceButton(c: ApiContainer, ui: UiState): HTMLElement {
  const status = ui.force_requests[c.name];
  const busy = status === "queuing…" || status === "queued";
  // An update applies a queued recreate too, so only a drift with no update behind it gets the recreate button.
  const recreate = !pending(c);
  const btn = el("button", {
    class: recreate ? "copy-btn force-btn force-btn-drift" : "copy-btn force-btn",
    type: "button",
    title: recreate
      ? "Recreate this container from its compose file now, past the pre-update check and dry-run mode. Asks first."
      : "Apply this update now, past the pre-update check and dry-run mode. Asks first.",
    disabled: busy ? "" : null,
  }, busy ? status : recreate ? "force recreate" : "force update");
  btn.addEventListener("click", (ev) => {
    ev.stopPropagation();
    void onForceUpdate(c);
  });
  return btn;
}

// recreateButton is the drift row's own button: "recreate" queues one, and "cancel" drops a queued one.
function recreateButton(c: ApiContainer, ui: UiState, method: "POST" | "DELETE"): HTMLElement {
  const status = ui.recreate_requests[c.name];
  const busy = status === "queuing…" || status === "cancelling…";
  const btn = el("button", {
    class: method === "POST" ? "copy-btn force-btn force-btn-drift recreate-btn" : "copy-btn cancel-btn",
    type: "button",
    title: method === "POST"
      ? "Queue a recreate from the compose file. It runs at the next check, past the same gates as an update. Asks first."
      : "Drop the queued recreate. The container keeps its config.",
    disabled: busy ? "" : null,
  }, busy ? status : method === "POST" ? "recreate" : "cancel");
  btn.addEventListener("click", (ev) => {
    ev.stopPropagation();
    void onRecreate(c, method);
  });
  return btn;
}

// actionButtons are the Upstream cell's buttons, and the failure of the last request one of them sent.
function actionButtons(c: ApiContainer, ui: UiState): HTMLElement[] {
  const buttons: HTMLElement[] = [];
  if (pending(c) || queuedRecreate(c)) buttons.push(forceButton(c, ui));
  if (queuedRecreate(c)) buttons.push(recreateButton(c, ui, "DELETE"));
  else if (outOfSync(c)) buttons.push(recreateButton(c, ui, "POST"));
  if (buttons.length === 0) return [];
  const out: HTMLElement[] = [el("div", { class: "actions" }, ...buttons)];
  for (const status of [ui.force_requests[c.name], ui.recreate_requests[c.name]]) {
    if (status && status.startsWith("failed")) out.push(el("div", { class: "detail up-error" }, status));
  }
  return [el("div", { class: "force" }, ...out)];
}

function upstreamCell(c: ApiContainer, ui: UiState, dryRun: boolean): HTMLElement {
  const view = upstreamView(c, dryRun);
  const td = el("td", null, el("span", { class: view.cls }, view.status));
  if (view.detail) td.appendChild(el("div", { class: "detail" }, view.detail));
  // The digests above say an update exists. The commits say what is in it.
  if (c.update_available && c.available_commit) {
    td.appendChild(el("div", { class: "detail" },
      c.commit ? commitLink(c.commit, c.commit_url) : "?",
      " → ",
      commitLink(c.available_commit, c.available_commit_url),
    ));
  }
  for (const node of actionButtons(c, ui)) td.appendChild(node);
  return td;
}

// driftApplies mirrors ComposeDrift.applies in compose_drift.go: the updater
// can recreate this container.
function driftApplies(d: ApiComposeDrift | undefined): boolean {
  return Boolean(d && d.state === "drift" && !d.blocked);
}

// outOfSync reports a monitored container whose compose file changed, with no
// image update pending.
function outOfSync(c: ApiContainer): boolean {
  return Boolean(c.auto_update && !c.update_available && driftApplies(c.compose_drift));
}

// queuedRecreate reports an out-of-sync container whose recreate an operator queued.
function queuedRecreate(c: ApiContainer): boolean {
  return outOfSync(c) && Boolean(c.recreate_queued);
}

// errored is the exact predicate behind the "errors" card: a monitored
// container whose last check or update attempt failed.
function errored(c: ApiContainer): boolean {
  const d = c.compose_drift;
  return Boolean(c.auto_update && (c.error || c.unmonitorable || (d && !driftApplies(d))));
}

// driftText states a compose drift with every fact needed to act on it: the
// service and the files. This also covers both hashes or the error, and the
// command that applies the file.
function driftText(d: ApiComposeDrift): string {
  const where = "service " + d.service + " in " + d.config_files.join(", ");
  if (d.state === "unknown") {
    return "compose drift unknown for " + where + ": " + (d.error || "?") + ". Apply the file: " + d.command;
  }
  const what = "drifted from compose file: " + where + " hashes to " + (d.file_hash || "?").slice(0, 12) +
    ", the container runs " + (d.running_hash || "?").slice(0, 12);
  if (d.blocked) return what + ". " + d.blocked + ". Apply the file: " + d.command;
  return what + ". Queue a recreate to apply the file";
}

// errorReport renders every current error as plain text for pasting into an
// issue or a chat. One block per container: what it is, what it was trying to
// move to, and the full untruncated error the Upstream. Column can only show
// a clipped version of.
function errorReport(data: ApiResponse): string {
  const rows = (data.containers || []).filter(errored);
  const header = "docker-updater — " + rows.length + " error" + (rows.length === 1 ? "" : "s") +
    " at " + (data.generated_at || new Date().toISOString()) +
    (data.version ? " (build " + data.version + ")" : "");
  const blocks = rows.map((c) => {
    const lines = [(c.name || "?") + "  [" + (c.image || "?") + "]"];
    lines.push("  state: " + (c.state || "unknown") + (c.health ? " (" + c.health + ")" : ""));
    if (c.stuck_cycles) lines.push("  stuck: " + c.stuck_cycles + " cycles since " + (c.stuck_since || "?"));
    if (c.update_available) {
      const refs = c.current_ref || c.available_ref
        ? ": " + (c.current_ref || "?") + " -> " + (c.available_ref || "?")
        : "";
      lines.push("  update available" + refs);
    }
    if (c.skipped) lines.push("  skipped: " + (c.skip_reason || "pre-check"));
    if (c.unmonitorable) lines.push("  cannot update: " + c.unmonitorable);
    if (c.compose_drift) lines.push("  " + driftText(c.compose_drift));
    if (c.error) lines.push("  error: " + c.error);
    return lines.join("\n");
  });
  return [header, ...blocks].join("\n\n") + "\n";
}

// One object: the served /api/containers payload, plus the state the server
// cannot know.

interface UiState {
  page_url: string;
  refresh_seconds: number;
  // The filter box verbatim, so a re-render restores what was typed rather than the normalized form matching uses.
  query: string;
  expanded: Record<string, boolean>;
  // Set only when the last poll failed — and then the containers below it are the last payload that arrived.
  error_banner: string | null;
  // The state of each force-update request this page sent, by container name: in flight, queued, or the failure.
  force_requests: Record<string, string>;
  // The state of each recreate queue or cancel request this page sent, by container name: in flight, or the failure.
  recreate_requests: Record<string, string>;
  // The containers whose list of compose differences is open.
  drift_open: Record<string, boolean>;
}

type DashboardState = ApiResponse & { ui: UiState };

// stateSnapshot serializes the state the page was drawn from, stamped with the
// moment of the copy.
function stateSnapshot(state: DashboardState): string {
  return JSON.stringify({ captured_at: new Date().toISOString(), ...state }, null, 2) + "\n";
}

// copyText writes to the clipboard, falling back to a hidden textarea +
// execCommand. The dashboard is normally served over plain http on a LAN, where
// navigator.clipboard does not exist (non-secure context).
async function copyText(text: string): Promise<boolean> {
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
  }
  const ta = el("textarea", { style: "position:fixed;top:-1000px;opacity:0;" }) as HTMLTextAreaElement;
  ta.value = text;
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  ta.remove();
  return ok;
}

// How long a copy button shows "copied" / "copy failed" before its real label comes back.
const COPY_FEEDBACK_MS = 1500;

// copyResetTimers holds the pending label-restore timer per button id.
const copyResetTimers = new Map<string, number>();

// flashCopyResult copies text and shows the outcome on the button itself, then
// hands the label back to updateCopyButtons once the flash expires.
async function flashCopyResult(btn: HTMLButtonElement, text: string): Promise<void> {
  const ok = await copyText(text);
  btn.textContent = ok ? "copied" : "copy failed";
  const pending = copyResetTimers.get(btn.id);
  if (pending !== undefined) clearTimeout(pending);
  copyResetTimers.set(btn.id, setTimeout(() => {
    copyResetTimers.delete(btn.id);
    updateCopyButtons(currentState);
  }, COPY_FEEDBACK_MS));
}

function errorCount(data: ApiResponse | null): number {
  return data ? (data.containers || []).filter(errored).length : 0;
}

// updateCopyButtons keeps both copy buttons' labels and enabled state in step
// with the latest payload. Called from render(), so it tracks every poll.
// Neither button is enabled while it has nothing to copy: the errors button
// until something errors, the state button until the first payload arrives.
function updateCopyButtons(data: ApiResponse | null): void {
  const errors = errorCount(data);
  const errorsBtn = document.getElementById("copy-errors") as HTMLButtonElement | null;
  if (errorsBtn && !copyResetTimers.has("copy-errors")) {
    errorsBtn.disabled = errors === 0;
    errorsBtn.textContent = errors > 0 ? "copy " + errors : "copy";
  }
  const stateBtn = document.getElementById("copy-state") as HTMLButtonElement | null;
  if (stateBtn && !copyResetTimers.has("copy-state")) {
    stateBtn.disabled = data === null;
    stateBtn.textContent = "copy JSON";
  }
}

async function onCopyErrors(): Promise<void> {
  const btn = document.getElementById("copy-errors") as HTMLButtonElement | null;
  if (!btn || !currentState) return;
  await flashCopyResult(btn, errorReport(currentState));
}

async function onCopyState(): Promise<void> {
  const btn = document.getElementById("copy-state") as HTMLButtonElement | null;
  if (!btn || !currentState) return;
  await flashCopyResult(btn, stateSnapshot(currentState));
}

// pending reports whether a container has an update detected but not yet
// applied.
function pending(c: ApiContainer): boolean {
  return Boolean(c.auto_update && c.update_available);
}

// needsAction is a held-back update or a container out of sync with its
// compose file: the exact condition the "updates pending" card counts.
function needsAction(c: ApiContainer): boolean {
  return pending(c) || outOfSync(c);
}

// isOnline splits containers into the online/offline group halves. Online is
// a container that is up: running, paused, or removing.
function isOnline(c: ApiContainer): boolean {
  return c.state === "running" || c.state === "paused" || c.state === "removing";
}

// isManaged asks whether ANY updater keeps this container current, which is
// the question the Managed sections answer.
function isManaged(c: ApiContainer): boolean {
  return c.auto_update || watchtowerOwns(c);
}

// Those dashboard sections, in display order.
interface Group {
  id: string;
  label: string;
  match: (c: ApiContainer) => boolean;
}

const GROUPS: Group[] = [
  { id: "group-managed-online", label: "Managed · online", match: (c) => isManaged(c) && isOnline(c) },
  { id: "group-managed-offline", label: "Managed · offline", match: (c) => isManaged(c) && !isOnline(c) },
  { id: "group-unmanaged-online", label: "Unmanaged · online", match: (c) => !isManaged(c) && isOnline(c) },
  { id: "group-unmanaged-offline", label: "Unmanaged · offline", match: (c) => !isManaged(c) && !isOnline(c) },
];

// searchBoxValue returns the filter box's text as typed.
function searchBoxValue(): string {
  const box = document.getElementById("search") as HTMLInputElement | null;
  return box ? box.value : "";
}

function normalizeQuery(raw: string): string {
  return raw.trim().toLowerCase();
}

// matchesQuery is a case-insensitive substring match against the container
// name and image; either matching keeps the row visible. `query` is normalized.
function matchesQuery(c: ApiContainer, query: string): boolean {
  if (!query) return true;
  return (c.name || "").toLowerCase().includes(query) || (c.image || "").toLowerCase().includes(query);
}

// expandedGroups reads which sections are open. Collapsing one hides its rows,
// so it is part of what the page is showing and belongs in the state.
function expandedGroups(): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  for (const g of GROUPS) {
    const section = document.getElementById(g.id) as HTMLDetailsElement | null;
    if (section) out[g.id] = section.open;
  }
  return out;
}

// updatedHighlight computes the fading green background for a container whose
// last update was applied within UPDATED_HIGHLIGHT_MS. This covers full
// strength when fresh, linearly down to zero at the end of the window. Returns
// null once the window has passed (or when the container was never updated).
function updatedHighlight(c: ApiContainer, now: number): { background: string; title: string } | null {
  if (!c.last_updated) return null;
  const then = new Date(c.last_updated).getTime();
  if (Number.isNaN(then)) return null;
  const age = Math.max(0, now - then); // clamp so slight clock skew reads as fresh
  if (age >= UPDATED_HIGHLIGHT_MS) return null;
  const alpha = UPDATED_HIGHLIGHT_MAX_ALPHA * (1 - age / UPDATED_HIGHLIGHT_MS);
  return {
    background: "rgba(" + GREEN_RGB + "," + alpha.toFixed(3) + ")",
    title: "updated " + (fmtRelative(c.last_updated) || "just now"),
  };
}

// openLogs shows a container's recent output in a modal.
async function openLogs(name: string): Promise<void> {
  const dialog = document.getElementById("logs-dialog") as HTMLDialogElement | null;
  const title = document.getElementById("logs-title");
  const body = document.getElementById("logs-body");
  if (!dialog || !title || !body) return;

  title.textContent = name;
  body.textContent = "loading…";
  if (!dialog.open) dialog.showModal();

  try {
    const res = await fetch("api/logs?container=" + encodeURIComponent(name) + "&tail=500", { cache: "no-store" });
    const text = await res.text();
    // A failed read still says something.
    body.textContent = res.ok ? text : "could not read logs (HTTP " + res.status + "): " + text;
  } catch (err) {
    body.textContent = "could not read logs: " + String(err);
  }
  body.scrollTop = body.scrollHeight;
}

// warningLines renders a container's configuration warnings under its name:
// amber, one line each.
function warningLines(c: ApiContainer): HTMLElement[] {
  return (c.warnings || []).map((w) => el("div", { class: "warn", title: w }, "⚠ " + w));
}

// driftLine renders a compose drift under the container's name. It is red when
// only the operator can apply the file, and purple when a recreate can. A found
// drift carries a toggle that lists what differs.
function driftLine(c: ApiContainer, ui: UiState): HTMLElement[] {
  const d = c.compose_drift;
  if (!d) return [];
  const text = driftText(d);
  const line = driftApplies(d)
    ? el("div", { class: "drift drift-applies", title: text }, "↻ " + text)
    : el("div", { class: "drift", title: text }, "✖ " + text);
  if (d.state !== "drift") return [line];

  const open = Boolean(ui.drift_open[c.name]);
  const n = (d.differences || []).length;
  const toggle = el("button", { class: "logs-link diff-toggle", type: "button", title: "What the compose file changes" },
    open ? "hide differences" : n === 0 ? "show details" : "show " + (n === 1 ? "1 difference" : n + " differences"));
  toggle.addEventListener("click", (ev) => {
    ev.stopPropagation();
    if (driftOpen[c.name]) delete driftOpen[c.name];
    else driftOpen[c.name] = true;
    repaint();
  });
  line.appendChild(toggle);
  if (!open) return [line];
  const list = el("ul", { class: "drift-diff" },
    el("li", { class: "drift-diff-head" }, "container → file"),
    ...differenceLines(d).map((l) => el("li", null, l)));
  return [line, list];
}

// logsLink opens the container's own output. Consider a container in a restart
// loop. That container explains itself there and nowhere else, and the row
// used to end at "Restarting (126)" — a status that names the failure. It
// hides the reason.
function logsLink(c: ApiContainer): HTMLElement {
  const a = el("button", { class: "logs-link", type: "button", title: "Show this container's recent output" }, "logs");
  a.addEventListener("click", (ev) => {
    ev.stopPropagation();
    void openLogs(c.name);
  });
  return a;
}

// reasonLine is the last line a failing container logged, under its name. It is
// the line an operator would have run docker logs to read.
function reasonLine(c: ApiContainer): HTMLElement[] {
  if (!c.log_excerpt) return [];
  return [el("div", { class: "log-excerpt", title: c.log_excerpt }, c.log_excerpt)];
}

function row(c: ApiContainer, ui: UiState, dryRun: boolean): HTMLElement {
  const nameCell = el("td", null,
    el("span", { class: "cname" }, c.name || "—"),
    logsLink(c),
    el("div", { class: "cmeta" },
      c.image || "—",
      c.image_id ? " · " : null,
      c.image_id ? el("span", { class: "cref" }, c.image_id) : null,
      c.commit ? " · " : null,
      c.commit ? commitLink(c.commit, c.commit_url) : null,
    ),
    ...driftLine(c, ui),
    ...warningLines(c),
    ...reasonLine(c),
  );

  const uptime = uptimeText(c);
  const lastPulled = lastPulledText(c);

  const highlight = updatedHighlight(c, Date.now());
  const rowClass = pending(c) ? "row-pending" : outOfSync(c) ? "row-pending row-drift" : null;
  const tr = el("tr", { class: rowClass, title: highlight ? highlight.title : null },
    nameCell,
    autoUpdateCell(c),
    stateCell(c),
    el("td", { class: uptime ? null : "up-na" }, uptime || "—"),
    restartsCell(c),
    el("td", { class: lastPulled === "—" ? "up-na" : null }, lastPulled),
    upstreamCell(c, ui, dryRun),
    statsCell(c),
  );
  // Inline style so the alpha can fade with age.
  if (highlight) tr.style.background = highlight.background;
  return tr;
}

function setText(id: string, text: string | number): void {
  const node = document.getElementById(id);
  if (node) node.textContent = String(text);
}

// render draws the state. Everything it puts on screen is derived here, from
// that one object. Nothing is read back out of the DOM, and nothing it draws is
// stored. Given the same state it produces the same page, which is what makes a
// copied snapshot reproducible.
function render(state: DashboardState): void {
  const data: ApiResponse = state;
  const ui = state.ui;
  const containers = data.containers || [];

  // The summary cards are fleet totals: computed over every container, never narrowed by the search filter.
  const managed = containers.filter(isManaged).length;
  const updates = containers.filter(needsAction).length;
  const errors = errorCount(data);

  setText("stat-total", containers.length);
  setText("stat-auto", managed);
  setText("stat-manual", containers.length - managed);
  setText("stat-updates", updates);
  setText("stat-errors", errors);

  // The card jumps to the held-back rows; only make it actionable when there are any.
  const updatesCard = document.getElementById("card-updates");
  if (updatesCard) updatesCard.classList.toggle("clickable", updates > 0);

  updateCopyButtons(state);

  document.getElementById("dry-run-badge")!.classList.toggle("hidden", !data.dry_run);
  // Derived from the state so a page drawn from a copy shows the same note.
  if (!data.stats_url) setStatsNote("no stats source (DOCKER_UPDATER_STATS_URL unset)", "badge-dim");
  setText("cfg-interval", data.interval || "—");
  setText("cfg-label", data.label || "—");
  setText("refresh-interval", ui.refresh_seconds);

  // The banner is the thing on the page no payload can carry.
  const banner = document.getElementById("error-banner")!;
  banner.textContent = ui.error_banner || "";
  banner.classList.toggle("hidden", ui.error_banner === null);

  // The updater's own build hash: shortened for the footer, full SHA on hover.
  const versionNode = document.getElementById("build-version");
  if (versionNode) {
    const v = data.version || "";
    versionNode.textContent = v ? v.slice(0, 12) : "—";
    versionNode.title = v ? "docker-updater build " + v : "";
  }

  const lastCycle = fmtRelative(data.last_cycle);
  setText("last-cycle", lastCycle ? "Last check " + lastCycle : "No check yet");
  const nextCycle = fmtRelative(data.next_cycle);
  setText("next-cycle", nextCycle ? "Next check " + nextCycle : "");
  setText("refreshed", "Updated " + new Date(data.generated_at).toLocaleTimeString());

  // Writing the box back is what lets a state rendered into a fresh page carry its filter with it.
  const box = document.getElementById("search") as HTMLInputElement;
  if (box.value !== ui.query) box.value = ui.query;

  const query = normalizeQuery(ui.query);
  const visible = containers.filter((c) => matchesQuery(c, query));

  // Row contents, summary counts, the hidden class, and the open state change
  // per render. Applying `open` from the state is what makes a re-render
  // reproduce the page. In normal use it writes back the value it was read
  // from, so an expand/collapse survives every poll.
  for (const g of GROUPS) {
    const members = visible.filter(g.match);
    document.getElementById(g.id + "-rows")!.replaceChildren(...members.map((c) => row(c, ui, data.dry_run)));
    document.getElementById(g.id + "-summary")!.textContent = g.label + " (" + members.length + ")";
    const section = document.getElementById(g.id) as HTMLDetailsElement;
    section.classList.toggle("hidden", members.length === 0);
    const open = ui.expanded[g.id];
    if (open !== undefined && section.open !== open) section.open = open;
  }

  // Every container lands in exactly one group, so "all groups hidden" is exactly "no visible containers": none exist.
  const empty = document.getElementById("empty")!;
  empty.textContent = query && containers.length > 0
    ? 'No containers match "' + ui.query.trim() + '".'
    : "No containers found.";
  empty.classList.toggle("hidden", visible.length > 0);
}

// Both inputs the state is assembled from: the last payload that arrived (kept so the filter can re-render without refetching).
let latestData: ApiResponse | null = null;
let pollFailure: string | null = null;
let forceRequests: Record<string, string> = {};
let recreateRequests: Record<string, string> = {};
let driftOpen: Record<string, boolean> = {};

// currentState is the object the page was last drawn from, and the one the copy button serializes.
let currentState: DashboardState | null = null;

// repaint rebuilds the state from those inputs plus the browser-side state, and
// draws it. Every path that changes anything the page shows ends here, so the
// object and the screen are never out of step.
function repaint(): void {
  if (!latestData) {
    // Nothing has arrived yet: there is no state to draw, only the failure to report.
    const banner = document.getElementById("error-banner")!;
    banner.textContent = pollFailure || "";
    banner.classList.toggle("hidden", pollFailure === null);
    return;
  }
  currentState = {
    ui: {
      page_url: location.href,
      refresh_seconds: REFRESH_SECONDS,
      query: searchBoxValue(),
      expanded: expandedGroups(),
      error_banner: pollFailure,
      force_requests: { ...forceRequests },
      recreate_requests: { ...recreateRequests },
      drift_open: { ...driftOpen },
    },
    ...latestData,
  };
  render(currentState);
}

// OUT_OF_SYNC_HINT is the human-readable diagnosis for mixed dashboard
// assets.
const OUT_OF_SYNC_HINT =
  "Dashboard assets are out of sync (a cache or proxy served mismatched files). " +
  "Hard-refresh (Ctrl+Shift+R); if it persists, purge your CDN cache for this host " +
  "and check for two running docker-updater containers.";

// isNullElementError spots render() blowing up on a missing element (a
// TypeError dereferencing null/undefined).
function isNullElementError(err: unknown): boolean {
  return err instanceof TypeError && /null|undefined/i.test(err.message);
}

async function refresh(): Promise<void> {
  try {
    const resp = await fetch("api/containers", { cache: "no-store" });
    if (!resp.ok) throw new Error("HTTP " + resp.status + ": " + (await resp.text()));
    latestData = (await resp.json()) as ApiResponse;
    pollFailure = null;
    bootStats(latestData);
    const containers = latestData.containers || [];
    const stillPending = new Set(containers.filter((c) => pending(c) || queuedRecreate(c)).map((c) => c.name));
    forceRequests = Object.fromEntries(Object.entries(forceRequests).filter(([name]) => stillPending.has(name)));
    const stillDrifted = new Set(containers.filter((c) => c.compose_drift).map((c) => c.name));
    recreateRequests = Object.fromEntries(Object.entries(recreateRequests).filter(([name]) => stillDrifted.has(name)));
    driftOpen = Object.fromEntries(Object.entries(driftOpen).filter(([name]) => stillDrifted.has(name)));
  } catch (err) {
    pollFailure = isNullElementError(err)
      ? OUT_OF_SYNC_HINT
      : "Failed to load container data: " + (err instanceof Error ? err.message : String(err));
  }
  repaint();
}

// jumpToPending scrolls to the first container with a held-back update, flashing
// it so "N updates pending" always points at concrete rows. Every row lives in
// a collapsible group, so reveal the group holding the target first.
function jumpToPending(): void {
  const first = document.querySelector<HTMLElement>(".row-pending");
  if (!first) return;
  const section = first.closest("details");
  if (section) section.open = true;
  first.scrollIntoView({ behavior: "smooth", block: "center" });
  first.classList.remove("row-flash");
  void first.offsetWidth; // reflow so the animation restarts on repeat clicks
  first.classList.add("row-flash");
}

// Typing in the filter and expanding a section both change what the page
// shows without any new data, so both rebuild the state — no refetch.
function onUiChange(): void {
  repaint();
}

// REQUIRED_IDS is the startup contract between this script and index.html.
const REQUIRED_IDS: string[] = [
  "search",
  "empty",
  "error-banner",
  "dry-run-badge",
  "stat-total",
  "stat-auto",
  "stat-manual",
  "stat-updates",
  "stat-errors",
  ...GROUPS.flatMap((g) => [g.id, g.id + "-rows", g.id + "-summary"]),
];

// renderOutOfSyncBanner replaces the cryptic "replaceChildren of null" crash
// with a readable full-width explanation of what happened. Inline styles on
// purpose: in the mixed-asset scenario the stylesheet may be from yet another
// build. The banner cannot rely on any class existing.
function renderOutOfSyncBanner(missing: string[]): void {
  const banner = el(
    "div",
    {
      class: "error-banner",
      style:
        "display:block;margin:16px;padding:12px 16px;border:1px solid #f85149;" +
        "border-radius:6px;background:#3d1214;color:#ffdcd7;" +
        "font:14px/1.5 system-ui,sans-serif;",
    },
    OUT_OF_SYNC_HINT + " (missing elements: " + missing.join(", ") + ")",
  );
  document.body.prepend(banner);
}

// initDashboard wires the listeners and starts the poll loop — only ever
// called against a document that passed the REQUIRED_IDS check below.
function initDashboard(): void {
  const updatesCard = document.getElementById("card-updates");
  if (updatesCard) updatesCard.addEventListener("click", jumpToPending);

  const copyErrorsBtn = document.getElementById("copy-errors");
  if (copyErrorsBtn) copyErrorsBtn.addEventListener("click", () => void onCopyErrors());

  const copyStateBtn = document.getElementById("copy-state");
  if (copyStateBtn) copyStateBtn.addEventListener("click", () => void onCopyState());

  const searchBox = document.getElementById("search") as HTMLInputElement | null;
  if (searchBox) searchBox.addEventListener("input", onUiChange);

  // Expanding or collapsing a section changes what is on screen.
  for (const g of GROUPS) {
    const section = document.getElementById(g.id);
    if (section) section.addEventListener("toggle", onUiChange);
  }

  // Keyboard niceties: "/" focuses the filter box, Escape clears it.
  document.addEventListener("keydown", (e: KeyboardEvent) => {
    if (!searchBox) return;
    const target = e.target instanceof HTMLElement ? e.target : null;
    const typing = target !== null &&
      (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable);
    if (e.key === "/" && !typing) {
      e.preventDefault();
      searchBox.focus();
      searchBox.select();
    } else if (e.key === "Escape" && target === searchBox) {
      searchBox.value = "";
      onUiChange();
      searchBox.blur();
    }
  });

  void refresh();
  setInterval(refresh, REFRESH_SECONDS * 1000);
}

// Startup gate: if the document is missing anything this script requires.
const missingIds = REQUIRED_IDS.filter((id) => document.getElementById(id) === null);
if (missingIds.length > 0) {
  renderOutOfSyncBanner(missingIds);
} else {
  initDashboard();
}

// -- Resource graphs
//
// Host cpu/ram/disk/net/gpu in the top bar and one strip per container row, every number from the simple-stats-api. That simple-stats-api is named by the payload's
// stats_url.

const PERF_GRAPH_URL = "https://sites.pazer.build/js-snippets/branch/library/ui/perf-graph.js";
const STATS_SEED_HISTORY = 300;
const STATS_RETRY_MS = 5000; // fixed cadence, never grows
const STATS_MIN_POLL_MS = 1000;

interface PerfGraphLike extends HTMLElement {
  push(value: number): void;
  clear(): void;
}

interface StatMetric {
  value: number;
  percent?: number | null;
  history?: number[];
}

interface StatContainer {
  cpu?: { percent?: StatMetric };
  ram?: { used?: StatMetric };
  network?: { rx_rate?: StatMetric; tx_rate?: StatMetric };
  disk?: { io?: { read_rate?: StatMetric; write_rate?: StatMetric } };
  gpu?: { utilization?: StatMetric };
}

interface StatsResponse {
  sampling?: { intervalSeconds?: number };
  metrics?: {
    cpu?: { percent?: StatMetric };
    ram?: { percent?: StatMetric };
    disk?: { percent?: StatMetric };
    network?: { rx_rate?: StatMetric; tx_rate?: StatMetric };
    gpu?: Array<{ utilization?: StatMetric }>;
    docker?: { containers?: StatMetric };
    containers?: Record<string, StatContainer>;
  };
}

interface StatGauge {
  label: string;
  unit: string;
  min: number | null;
  max: number | null;
  series: (m: unknown) => number[] | null; // newest first, or null when unreported
  absent: string;
}

function statSeries(m: StatMetric | undefined): number[] | null {
  if (!m || typeof m.value !== "number") return null;
  return [m.value, ...(m.history || [])];
}

// Share of the possible range, newest first (the API's percent is only the current value).
function statPercentSeries(m: StatMetric | undefined): number[] | null {
  if (!m || typeof m.value !== "number") return null;
  const cur = typeof m.percent === "number" ? m.percent : m.value;
  if (m.value === 0) return [cur, ...(m.history || []).map(() => 0)];
  const scale = cur / m.value;
  return [cur, ...(m.history || []).map((v) => v * scale)];
}

function statSumSeries(a: StatMetric | undefined, b: StatMetric | undefined, scale: number): number[] | null {
  const sa = statSeries(a);
  const sb = statSeries(b);
  if (!sa && !sb) return null;
  if (!sa) return sb!.map((v) => v * scale);
  if (!sb) return sa.map((v) => v * scale);
  const n = Math.min(sa.length, sb.length);
  const out: number[] = [];
  for (let i = 0; i < n; i++) out.push((sa[i] + sb[i]) * scale);
  return out;
}

const STAT_MB = 1 / 1e6;
const STAT_MIB = 1 / 1048576;

const HOST_STAT_GAUGES: StatGauge[] = [
  { label: "cpu", unit: "%", min: 0, max: 100, absent: "no cpu figure from the stats source",
    series: (m) => statPercentSeries((m as StatsResponse["metrics"])?.cpu?.percent) },
  { label: "ram", unit: "%", min: 0, max: 100, absent: "no ram figure from the stats source",
    series: (m) => statSeries((m as StatsResponse["metrics"])?.ram?.percent) },
  { label: "disk", unit: "%", min: 0, max: 100, absent: "no disk figure from the stats source (Linux only)",
    series: (m) => statSeries((m as StatsResponse["metrics"])?.disk?.percent) },
  { label: "net", unit: "MB/s", min: 0, max: null, absent: "no network figure from the stats source",
    series: (m) => { const n = (m as StatsResponse["metrics"])?.network; return statSumSeries(n?.rx_rate, n?.tx_rate, STAT_MB); } },
  { label: "gpu", unit: "%", min: 0, max: 100, absent: "no GPU reported by the stats source",
    series: (m) => statSeries((m as StatsResponse["metrics"])?.gpu?.[0]?.utilization) },
];

const CONTAINER_STAT_GAUGES: StatGauge[] = [
  { label: "cpu", unit: "%", min: 0, max: 100, absent: "no cpu figure for this container",
    series: (m) => statPercentSeries((m as StatContainer).cpu?.percent) },
  { label: "ram", unit: "MiB", min: 0, max: null, absent: "no memory figure for this container",
    series: (m) => { const s = statSeries((m as StatContainer).ram?.used); return s ? s.map((v) => v * STAT_MIB) : null; } },
  { label: "disk", unit: "MB/s", min: 0, max: null, absent: "no block I/O figure for this container",
    series: (m) => { const io = (m as StatContainer).disk?.io; return statSumSeries(io?.read_rate, io?.write_rate, STAT_MB); } },
  { label: "net", unit: "MB/s", min: 0, max: null, absent: "no network figure for this container",
    series: (m) => { const n = (m as StatContainer).network; return statSumSeries(n?.rx_rate, n?.tx_rate, STAT_MB); } },
  { label: "gpu", unit: "%", min: 0, max: 100, absent: "no GPU work attributed to this container",
    series: (m) => statSeries((m as StatContainer).gpu?.utilization) },
];

// A row of gauges bound to one metrics subtree.
class StatStrip {
  readonly el: HTMLElement;
  private graphs: Array<{ gauge: StatGauge; el: PerfGraphLike; seeded: boolean; present: boolean }> = [];

  constructor(gauges: StatGauge[]) {
    this.el = el("div", { class: "stat-strip" });
    for (const gauge of gauges) {
      const g = document.createElement("perf-graph") as PerfGraphLike;
      g.setAttribute("compact", "");
      g.setAttribute("label", gauge.label);
      g.setAttribute("unit", gauge.unit);
      g.setAttribute("history", String(STATS_SEED_HISTORY));
      if (gauge.min !== null) g.setAttribute("min", String(gauge.min));
      if (gauge.max !== null) g.setAttribute("max", String(gauge.max));
      this.el.appendChild(g);
      this.graphs.push({ gauge, el: g, seeded: false, present: false });
    }
  }

  // Feed one reading. A seed reading carries history and is replayed oldest-first.
  update(m: unknown, seed: boolean): void {
    for (const g of this.graphs) {
      const s = g.gauge.series(m);
      if (!s) {
        if (g.present || !g.el.title) {
          g.el.clear();
          g.el.title = g.gauge.absent;
          g.el.classList.add("absent");
          g.present = false;
        }
        continue;
      }
      if (!g.present) {
        g.el.title = "";
        g.el.classList.remove("absent");
        g.present = true;
      }
      if (seed || !g.seeded) {
        g.el.clear();
        for (let i = s.length - 1; i >= 0; i--) g.el.push(s[i]);
        g.seeded = true;
      } else {
        g.el.push(s[0]);
      }
    }
  }

  setStale(stale: boolean, why: string): void {
    this.el.classList.toggle("stale", stale);
    if (stale) this.el.title = why;
    else this.el.removeAttribute("title");
  }
}

// Per-container strips outlive rows: row() asks for the strip by name and the
// node moves into the new row.
const containerStrips = new Map<string, StatStrip>();
let hostStrip: StatStrip | null = null;
let latestStats: StatsResponse | null = null;
let statsBooted = false;

function stripFor(name: string): StatStrip {
  let s = containerStrips.get(name);
  if (!s) {
    s = new StatStrip(CONTAINER_STAT_GAUGES);
    containerStrips.set(name, s);
    const m = latestStats?.metrics?.containers?.[name];
    if (m) s.update(m, true);
    else s.setStale(true, "the stats source reports nothing for this container");
  }
  return s;
}

// statsCell is the row's Resources column. It stays empty until the graphs
// component is loaded, so a page with no stats source draws a plain table.
function statsCell(c: ApiContainer): HTMLElement {
  return el("td", { class: "stats-cell" }, statsBooted && c.name ? stripFor(c.name).el : null);
}

function setStatsNote(text: string, cls: string): void {
  const n = document.getElementById("stats-note");
  if (!n) return;
  n.textContent = text;
  n.className = "badge " + cls;
  n.classList.toggle("hidden", text === "");
}

function applyStats(r: StatsResponse, seed: boolean): void {
  latestStats = r;
  const m = r.metrics;
  hostStrip?.update(m, seed);
  const all = m?.containers || {};
  for (const [name, strip] of containerStrips) {
    const cm = all[name];
    if (cm) {
      strip.update(cm, seed);
      strip.setStale(false, "");
    } else {
      strip.setStale(true, m?.docker ? "the stats source reports nothing for this container" : "the stats source reports no Docker containers (no daemon socket)");
    }
  }
  // Drop strips for containers that left the page.
  const onPage = new Set((latestData?.containers || []).map((c) => c.name));
  for (const name of containerStrips.keys()) if (!onPage.has(name)) containerStrips.delete(name);
}

// tsc is configured with module "none", which rejects a dynamic import in source; the browser supports it in a classic script.
const importModule = new Function("u", "return import(u)") as (u: string) => Promise<unknown>;

async function loadPerfGraphForever(): Promise<void> {
  for (let attempt = 0; ; attempt++) {
    try {
      await importModule(attempt === 0 ? PERF_GRAPH_URL : PERF_GRAPH_URL + "?retry=" + attempt);
      return;
    } catch (e) {
      console.error("perf-graph: component load failed (retry in " + STATS_RETRY_MS + "ms):", e);
      await new Promise((r) => setTimeout(r, STATS_RETRY_MS));
    }
  }
}

async function pollStatsForever(base: string): Promise<void> {
  let seed = true;
  let intervalMs = STATS_MIN_POLL_MS;
  for (;;) {
    try {
      const res = await fetch(base + "/api/v1/stats.json?history=" + (seed ? STATS_SEED_HISTORY : 0), { cache: "no-store" });
      if (!res.ok) throw new Error("HTTP " + res.status);
      applyStats((await res.json()) as StatsResponse, seed);
      hostStrip?.setStale(false, "");
      setStatsNote("", "");
      const iv = (latestStats?.sampling?.intervalSeconds) ?? 0;
      if (iv > 0) intervalMs = Math.max(STATS_MIN_POLL_MS, iv * 1000);
      seed = false;
    } catch (e) {
      const why = "stats source unreachable: " + base + " (" + (e instanceof Error ? e.message : String(e)) + ")";
      console.error(why);
      hostStrip?.setStale(true, why);
      for (const s of containerStrips.values()) s.setStale(true, why);
      setStatsNote("stats unreachable", "badge-error");
      seed = true;
      await new Promise((r) => setTimeout(r, STATS_RETRY_MS));
      continue;
    }
    await new Promise((r) => setTimeout(r, intervalMs));
  }
}

// bootStats runs once, on the first payload. Without a stats_url the top bar
// says so and nothing else happens; the rows get no strip.
function bootStats(data: ApiResponse): void {
  if (statsBooted) return;
  const mount = document.getElementById("host-stats");
  if (!mount) return;
  const base = (data.stats_url || "").replace(/\/$/, "");
  if (base === "") return; // render() carries the note, derived from the state
  statsBooted = true;
  setStatsNote("stats loading…", "badge-dim");
  void loadPerfGraphForever().then(() => {
    hostStrip = new StatStrip(HOST_STAT_GAUGES);
    mount.replaceChildren(hostStrip.el);
    repaint(); // rows now get their strips
    void pollStatsForever(base);
  });
}

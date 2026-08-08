// Completeness gate for the "copy JSON" button.
//
// The button's promise is that a paste of it explains what the operator was
// looking at. That is only true if every value on the page is in the copy, and
// the way it stops being true is silent: a column is added, or the API grows a
// field, and the snapshot just doesn't mention it. So this test renders the
// REAL compiled dashboard.js in a DOM, copies, and asserts that every string
// the page draws is present in the copied JSON -- rather than checking a list
// of fields someone remembered to write down.
//
// It drives dashboard.js, not dashboard.ts, so a stale build fails here too.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";

// A payload shaped like a real one: monitored and unmonitored containers,
// online and offline, a configuration warning, a failed check, a held-back
// update, and a container Docker could not inspect (null restarts).
const PAYLOAD = {
	generated_at: "2026-08-08T19:09:05.713435114Z",
	interval: "10m0s",
	dry_run: false,
	label: "docker-updater.enable",
	version: "3d93f61589ca1c34e673d08be7fc189f851156c3",
	last_cycle: "2026-08-08T18:59:27.472951275Z",
	next_cycle: "2026-08-08T19:09:27.472951275Z",
	// Not in ApiContainer/ApiResponse: stands in for a field the server grows
	// later. The copy must carry it without this file being taught about it.
	future_server_field: "must survive the copy",
	containers: [
		{
			name: "github-state-mirror",
			image: "ghcr.io/wow-look-at-my/github-state-mirror:latest",
			image_id: "59f03d035985",
			state: "running",
			status: "Up 5 days",
			health: "",
			created: 1785620105,
			restarts: 0,
			auto_update: true,
			mode: "image",
			last_checked: "2026-08-08T18:59:26.402289353Z",
			update_available: false,
			current_ref: "c83fd7303794",
			warnings: [
				"does not serve /.well-known/docker-updater/health (probed http://172.16.7.2:8080); post-update liveness falls back to Docker HEALTHCHECK. Implement the endpoint, or set docker-updater.well-known=false to silence this",
			],
		},
		{
			name: "webhook-runner",
			image: "ghcr.io/wow-look-at-my/webhook-runner:latest",
			image_id: "ec2348df6660",
			state: "running",
			status: "Up 30 hours (healthy)",
			health: "healthy",
			created: 1786108307,
			restarts: 3,
			auto_update: true,
			mode: "image",
			update_available: true,
			current_ref: "8ff850b581c2",
			available_ref: "aa11bb22cc33",
			skipped: true,
			skip_reason: "pre-update endpoint answered 503",
			warnings: [
				"nonstandard update checks: docker-updater.pre-check.url override the standard /.well-known/docker-updater/ endpoints",
			],
		},
		{
			name: "buildhost",
			image: "ghcr.io/wow-look-at-my/buildhost:latest",
			image_id: "42efd08e905b",
			state: "running",
			status: "Up 31 hours (healthy)",
			health: "healthy",
			created: 1786103469,
			restarts: 0,
			auto_update: true,
			mode: "image",
			update_available: false,
			current_ref: "8949463c73fe",
			error: "pull ghcr.io/wow-look-at-my/buildhost:latest: unauthorized: authentication required",
			warnings: [
				"no standard update endpoints: container exposes no TCP port; set docker-updater.well-known.port",
			],
		},
		{
			name: "goflow2",
			image: "netsampler/goflow2:v2.2.6",
			image_id: "7ef8537117ab",
			state: "created",
			status: "Created",
			health: "",
			created: 1785147541,
			restarts: null,
			auto_update: false,
			update_available: false,
		},
	],
};

interface CopyCapture {
	text: string;
	json: Record<string, unknown>;
}

interface Harness {
	dom: JSDOM;
	win: Window & typeof globalThis & Record<string, unknown>;
	copy: () => CopyCapture;
	rendered: () => string[];
	close: () => void;
}

// boot loads index.html + the compiled dashboard.js into a DOM, answers the
// script's own fetch with PAYLOAD, and waits for the first render.
async function boot(payload: unknown = PAYLOAD): Promise<Harness> {
	const html = readFileSync(new URL("./index.html", import.meta.url), "utf-8");
	const js = readFileSync(new URL("./dashboard.js", import.meta.url), "utf-8");

	const dom = new JSDOM(html, { url: "http://updater.test/", runScripts: "dangerously" });
	const win = dom.window as unknown as Window & typeof globalThis & Record<string, unknown>;

	win.fetch = (async () =>
		({ ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(payload)) })) as unknown as typeof fetch;

	// A <script> element, not window.eval: the compiled bundle is emitted in
	// strict mode, where an eval's function declarations stay inside the eval
	// instead of becoming the page globals the browser gives them.
	const tag = dom.window.document.createElement("script");
	tag.textContent = js;
	dom.window.document.body.appendChild(tag);

	// copyText is a top-level function declaration, so it is a property of the
	// window the script's own calls resolve through: replacing it intercepts
	// what the button would have put on the clipboard. jsdom implements no
	// clipboard, so this is also the only way to read it.
	let captured: string | null = null;
	win.copyText = async (text: string) => {
		captured = text;
		return true;
	};

	// Let the init-time refresh() settle.
	await new Promise((r) => setTimeout(r, 0));

	const doc = dom.window.document;
	return {
		dom,
		win,
		copy: () => {
			captured = null;
			(doc.getElementById("copy-state") as HTMLButtonElement).click();
			assert.ok(captured !== null, "clicking copy JSON produced no text");
			const text = captured as unknown as string;
			return { text, json: JSON.parse(text) as Record<string, unknown> };
		},
		rendered: () => renderedStrings(doc),
		close: () => dom.window.close(),
	};
}

// renderedStrings collects every string the page currently DRAWS from the
// payload: the summary numbers, the header timestamps, the group headings, the
// footer settings, and every text node in the four row tables. Static markup
// (column headers, the brand) is excluded -- it is chrome, not state.
function renderedStrings(doc: Document): string[] {
	const out: string[] = [];
	const push = (s: string | null | undefined): void => {
		const t = (s || "").trim();
		if (t !== "" && t !== "·") out.push(t);
	};

	for (const id of [
		"stat-total", "stat-auto", "stat-manual", "stat-updates", "stat-errors",
		"last-cycle", "next-cycle", "refreshed",
		"cfg-interval", "cfg-label", "build-version",
	]) {
		push(doc.getElementById(id)?.textContent);
	}
	for (const el of Array.from(doc.querySelectorAll("summary, tbody *"))) {
		// Leaf elements only: an ancestor's textContent is its children run
		// together, which no single value in the snapshot can match.
		if (el.children.length === 0) push(el.textContent);
	}
	for (const id of ["empty", "error-banner"]) {
		const el = doc.getElementById(id);
		if (el && !el.classList.contains("hidden")) push(el.textContent);
	}
	return out;
}

// present reports whether a rendered string is findable in the copied JSON.
// The warning marker is decoration the copy has no reason to carry, and JSON
// escapes the quotes a container error can contain.
function present(copied: string, rendered: string): boolean {
	const needle = rendered.replace(/^⚠\s*/, "");
	return copied.includes(needle) || copied.includes(JSON.stringify(needle).slice(1, -1));
}

test("every value the page renders is in the copied JSON", async () => {
	const h = await boot();
	try {
		const { text } = h.copy();
		const missing = h.rendered().filter((s) => !present(text, s));
		assert.deepEqual(missing, [], "rendered on the page but absent from the copy");
	} finally {
		h.close();
	}
});

test("the copy is parseable JSON that dates itself", async () => {
	const h = await boot();
	try {
		const { json } = h.copy();
		assert.equal(json.source, "docker-updater dashboard");
		assert.equal(json.generated_at, PAYLOAD.generated_at, "the served payload's own timestamp is kept");
		assert.match(String(json.captured_at), /^\d{4}-\d\d-\d\dT/, "the click is timestamped too");
		assert.equal(json.page_url, "http://updater.test/");
	} finally {
		h.close();
	}
});

test("every container reports warnings explicitly, present or absent", async () => {
	const h = await boot();
	try {
		const { json } = h.copy();
		const containers = json.containers as Record<string, unknown>[];
		assert.equal(containers.length, PAYLOAD.containers.length);
		for (const [i, c] of containers.entries()) {
			const want = PAYLOAD.containers[i].warnings ?? [];
			assert.deepEqual(c.warnings, want, `${String(c.name)}: warnings`);
			assert.deepEqual((c.row as Record<string, unknown>).warnings, want, `${String(c.name)}: row warnings`);
		}
		// The container with none says so, rather than omitting the key: an
		// absent field reads as "not reported", which is a different claim.
		const unmanaged = containers.find((c) => c.name === "goflow2")!;
		assert.ok("warnings" in unmanaged && "error" in unmanaged && "skip_reason" in unmanaged);
		assert.equal(unmanaged.error, null);
		assert.equal(unmanaged.restarts, null);
	} finally {
		h.close();
	}
});

test("errors and held-back updates carry their full text", async () => {
	const h = await boot();
	try {
		const { json } = h.copy();
		const containers = json.containers as Record<string, unknown>[];
		const failed = containers.find((c) => c.name === "buildhost")!;
		assert.equal(failed.error, PAYLOAD.containers[2].error);
		assert.equal((failed.row as Record<string, unknown>).upstream, "error");

		const held = containers.find((c) => c.name === "webhook-runner")!;
		const row = held.row as Record<string, unknown>;
		assert.equal(row.upstream, "update available");
		assert.equal(row.pending, true);
		assert.match(String(row.upstream_detail), /8ff850b581c2 → aa11bb22cc33/);
		assert.match(String(row.upstream_detail), /pre-update endpoint answered 503/);
	} finally {
		h.close();
	}
});

test("the page's own totals and sections are captured", async () => {
	const h = await boot();
	try {
		const { json } = h.copy();
		assert.deepEqual(json.totals, {
			containers: 4, auto_updated: 3, manual: 1, updates_pending: 1, errors: 1,
		});
		const groups = json.groups as Record<string, unknown>[];
		assert.deepEqual(groups.map((g) => [g.id, g.count, g.shown]), [
			["group-managed-online", 3, true],
			["group-managed-offline", 0, false],
			["group-unmanaged-online", 0, false],
			["group-unmanaged-offline", 1, true],
		]);
		const header = json.header as Record<string, unknown>;
		assert.match(String(header.last_check), /^Last check /);
		assert.equal(header.check_interval, "10m0s");
		assert.equal(header.label, "docker-updater.enable");
		assert.equal(header.dry_run_badge, false);
		assert.equal(json.error_banner, null);
	} finally {
		h.close();
	}
});

test("the filter's effect on what is on screen is captured", async () => {
	const h = await boot();
	try {
		const doc = h.dom.window.document;
		const box = doc.getElementById("search") as HTMLInputElement;
		box.value = "buildhost";
		box.dispatchEvent(new h.dom.window.Event("input"));

		const { text, json } = h.copy();
		const filter = json.filter as Record<string, unknown>;
		assert.equal(filter.query, "buildhost");
		assert.equal(filter.visible, 1);
		assert.equal(filter.hidden, 3);

		// Filtered-out containers stay in the copy -- the paste is the fleet's
		// state, not the search result -- but each says whether it was on
		// screen, so the numbers add up for whoever reads it.
		const containers = json.containers as Record<string, unknown>[];
		assert.equal(containers.length, 4);
		const visible = containers.filter((c) => (c.row as Record<string, unknown>).visible);
		assert.deepEqual(visible.map((c) => c.name), ["buildhost"]);

		// And nothing left on screen went missing while filtered.
		const missing = h.rendered().filter((s) => !present(text, s));
		assert.deepEqual(missing, []);
	} finally {
		h.close();
	}
});

test("a field the server adds later survives the copy", async () => {
	const h = await boot();
	try {
		const { json } = h.copy();
		assert.equal(json.future_server_field, "must survive the copy");
	} finally {
		h.close();
	}
});

test("a failed poll is reported instead of being copied as if fresh", async () => {
	const h = await boot();
	try {
		// The banner is the only sign the numbers below it are stale.
		const banner = h.dom.window.document.getElementById("error-banner")!;
		banner.textContent = "Failed to load container data: HTTP 500";
		banner.classList.remove("hidden");

		const { json } = h.copy();
		assert.equal(json.error_banner, "Failed to load container data: HTTP 500");
	} finally {
		h.close();
	}
});

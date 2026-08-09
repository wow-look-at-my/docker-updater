// The gate for the "copy JSON" button.
//
// The button's promise is that a paste explains the screen. The design that
// makes that true is that the page is DRAWN from the object the button copies,
// so the property to test is a round trip: copy the JSON, render it into a
// fresh empty page, and get the same page back. Anything the screen shows that
// is not in the copy shows up as a difference in that second render — which is
// a stronger check than any list of fields someone remembered to assert, and it
// keeps working when a column is added.
//
// It drives dashboard.js, not dashboard.ts, so a stale build fails here too.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";

// Frozen so the two renders in a round trip resolve "9m ago" identically.
const NOW = Date.UTC(2026, 7, 8, 19, 9, 0);

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
	// Not in ApiResponse: stands in for a field the server grows later. The copy
	// must carry it without this file being taught about it.
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

type Json = Record<string, unknown>;

interface Harness {
	dom: JSDOM;
	win: Window & typeof globalThis & Record<string, unknown>;
	doc: Document;
	copy: () => { text: string; json: Json };
	html: () => string;
	close: () => void;
}

const html = readFileSync(new URL("./index.html", import.meta.url), "utf-8");
const js = readFileSync(new URL("./dashboard.js", import.meta.url), "utf-8");

// load builds a page running the real compiled dashboard. `payload` answers the
// script's own poll; `null` leaves the poll pending forever, giving an untouched
// page to render a copied state into.
async function load(payload: unknown): Promise<Harness> {
	const dom = new JSDOM(html, { url: "http://updater.test/", runScripts: "dangerously" });
	const win = dom.window as unknown as Window & typeof globalThis & Record<string, unknown>;
	win.Date.now = () => NOW;

	win.fetch = (payload === null
		? () => new Promise(() => {})
		: async () => ({ ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(payload)) })
	) as unknown as typeof fetch;

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

	await new Promise((r) => setTimeout(r, 0)); // let the init-time refresh() settle

	const doc = dom.window.document;
	return {
		dom,
		win,
		doc,
		copy: () => {
			captured = null;
			(doc.getElementById("copy-state") as HTMLButtonElement).click();
			assert.ok(captured !== null, "clicking copy JSON produced no text");
			const text = captured as unknown as string;
			return { text, json: JSON.parse(text) as Json };
		},
		html: () => doc.body.innerHTML,
		close: () => dom.window.close(),
	};
}

// bodyWithoutScript drops the injected <script> so two pages are compared on
// what they render, not on how the test loaded them.
function bodyWithoutScript(markup: string): string {
	return markup.replace(/<script>[\s\S]*<\/script>/, "").trim();
}

// roundTrip renders a copied state into a fresh, empty page through the page's
// own render path, and returns that page's markup.
async function roundTrip(state: Json): Promise<{ markup: string; close: () => void }> {
	const blank = await load(null);
	(blank.win.render as (s: unknown) => void)(state);
	return { markup: bodyWithoutScript(blank.html()), close: blank.close };
}

test("the copied JSON re-renders the same page", async () => {
	const page = await load(PAYLOAD);
	try {
		const before = bodyWithoutScript(page.html());
		const { json } = page.copy();
		const replay = await roundTrip(json);
		try {
			assert.equal(replay.markup, before);
		} finally {
			replay.close();
		}
	} finally {
		page.close();
	}
});

test("the round trip carries the filter and the collapsed sections", async () => {
	const page = await load(PAYLOAD);
	try {
		const box = page.doc.getElementById("search") as HTMLInputElement;
		box.value = "BUILD"; // mixed case: what was typed, not the normalized form
		box.dispatchEvent(new page.dom.window.Event("input"));
		const managed = page.doc.getElementById("group-managed-online") as HTMLDetailsElement;
		managed.open = false;
		managed.dispatchEvent(new page.dom.window.Event("toggle"));

		const before = bodyWithoutScript(page.html());
		const { json } = page.copy();
		const ui = json.ui as Json;
		assert.equal(ui.query, "BUILD");
		assert.deepEqual(ui.expanded, {
			"group-managed-online": false,
			"group-managed-offline": true,
			"group-unmanaged-online": true,
			"group-unmanaged-offline": false,
		});

		const replay = await roundTrip(json);
		try {
			assert.equal(replay.markup, before, "a filtered, part-collapsed page must replay as itself");
		} finally {
			replay.close();
		}
	} finally {
		page.close();
	}
});

test("the copy is parseable, dates itself, and keeps the payload whole", async () => {
	const page = await load(PAYLOAD);
	try {
		const { json } = page.copy();
		assert.match(String(json.captured_at), /^\d{4}-\d\d-\d\dT/, "the click is timestamped");
		assert.equal(json.generated_at, PAYLOAD.generated_at, "the payload's own timestamp is kept");
		assert.equal(json.future_server_field, "must survive the copy", "an unknown field rides along");
		assert.deepEqual(json.containers, PAYLOAD.containers, "the payload is copied verbatim");
		const ui = json.ui as Json;
		assert.equal(ui.page_url, "http://updater.test/");
		assert.equal(ui.refresh_seconds, 5);
		assert.equal(ui.error_banner, null);
	} finally {
		page.close();
	}
});

test("a failed poll is reported in the copy, not papered over", async () => {
	const page = await load(PAYLOAD);
	try {
		// Same shape as a poll that fails after one has succeeded: the banner is
		// up, and the containers under it are the last payload that arrived.
		page.win.fetch = (async () => ({
			ok: false,
			status: 500,
			text: async () => "boom",
		})) as unknown as typeof fetch;
		await (page.win.refresh as () => Promise<void>)();

		const { json } = page.copy();
		const ui = json.ui as Json;
		assert.match(String(ui.error_banner), /Failed to load container data: HTTP 500/);
		assert.deepEqual(json.containers, PAYLOAD.containers, "the stale payload is still reported");

		const before = bodyWithoutScript(page.html());
		const replay = await roundTrip(json);
		try {
			assert.equal(replay.markup, before, "the banner replays too");
		} finally {
			replay.close();
		}
	} finally {
		page.close();
	}
});

test("no derived value is stored beside the fact it is derived from", async () => {
	const page = await load(PAYLOAD);
	try {
		const { json } = page.copy();
		assert.deepEqual(
			Object.keys(json).sort(),
			["captured_at", "containers", "dry_run", "future_server_field", "generated_at", "interval",
				"label", "last_cycle", "next_cycle", "ui", "version"],
			"the copy is the payload plus ui and a timestamp — counts, headings and rendered cells are " +
			"computed by render(), and a second copy of them here is a thing that can go stale",
		);
		for (const c of json.containers as Json[]) {
			assert.ok(!("row" in c), `${String(c.name)} carries a rendered-row block`);
		}
	} finally {
		page.close();
	}
});

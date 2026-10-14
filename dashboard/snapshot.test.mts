// The gate for the "copy JSON" button.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";

// Frozen so both renders in a round trip resolve "9m ago" identically.
const NOW = Date.UTC(2026, 7, 8, 19, 9, 0);

// A payload shaped like a real one: monitored and unmonitored containers,
// online and offline, a configuration warning, a failed check, a held-back
// update. A container Docker could not inspect (null restarts). A container in
// a crash loop, one nothing can check, and a compose drift held back, blocked
// and unknown.
const PAYLOAD = {
	generated_at: "2026-08-08T19:09:05.713435114Z",
	interval: "10m0s",
	dry_run: false,
	label: "docker-updater.enable",
	version: "3d93f61589ca1c34e673d08be7fc189f851156c3",
	last_cycle: "2026-08-08T18:59:27.472951275Z",
	next_cycle: "2026-08-08T19:09:27.472951275Z",
	// Not in ApiResponse: stands in for a field the server grows later.
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
			commit: "3d93f61589ca1c34e673d08be7fc189f851156c3",
			commit_url: "https://github.com/wow-look-at-my/webhook-runner/commit/3d93f61589ca1c34e673d08be7fc189f851156c3",
			available_commit: "aa11bb22cc33dd44ee55ff6600112233445566778",
			available_commit_url: "https://github.com/wow-look-at-my/webhook-runner/commit/aa11bb22cc33dd44ee55ff6600112233445566778",
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
			name: "log-streamer-server",
			image: "log-streamer-server:local",
			image_id: "f4edd950028b",
			state: "restarting",
			status: "Restarting (255) 46 seconds ago",
			health: "",
			created: 1786874419,
			restarts: 964,
			auto_update: true,
			mode: "build",
			update_available: false,
			log_excerpt: "exec /usr/local/bin/log-streamer-server: exec format error",
		},
		{
			name: "s3",
			image: "sha256:4a721d2e7ba5dd476b10c9b7f84b954ed9bf0bd455050332a7b1e73d202688da",
			image_id: "4a721d2e7ba5",
			state: "running",
			status: "Up 16 hours",
			health: "",
			created: 1786723890,
			restarts: 0,
			auto_update: true,
			mode: "image",
			update_available: false,
			unmonitorable: "no registry repository to poll: the container runs a bare image ID and its image carries no repo digest",
			stuck_cycles: 800,
			stuck_since: "2026-08-05T19:00:00Z",
		},
		{
			name: "gsm-rolling",
			image: "ghcr.io/wow-look-at-my/github-state-mirror:latest",
			image_id: "59f03d035986",
			state: "running",
			status: "Up 2 days",
			health: "",
			created: 1786620105,
			restarts: 0,
			auto_update: true,
			mode: "image",
			update_available: false,
			current_ref: "c83fd7303795",
			skipped: true,
			skip_reason: "pre-update endpoint answered 503",
			recreate_queued: "2026-08-08T19:04:00Z",
			compose_drift: {
				state: "drift",
				service: "github-state-mirror",
				config_files: ["/srv/mirror/compose.yml"],
				running_hash: "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888",
				file_hash: "9999cccc8888dddd7777eeee6666ffff5555aaaa4444bbbb3333cccc2222dddd",
				command: "docker compose -f /srv/mirror/compose.yml --project-directory /srv/mirror -p mirror up -d --no-deps github-state-mirror",
				differences: [
					{ field: "mount /data", file: "tmpfs", container: "bind /srv/mirror/data" },
				],
			},
		},
		{
			name: "media",
			image: "ghcr.io/example/media:latest",
			image_id: "5a5a5a5a5a5a",
			state: "running",
			status: "Up 4 days",
			health: "",
			created: 1786420105,
			restarts: 0,
			auto_update: true,
			mode: "image",
			update_available: false,
			current_ref: "6b6b6b6b6b6b",
			compose_drift: {
				state: "drift",
				service: "media",
				config_files: ["/srv/media/compose.yml"],
				running_hash: "1212121212121212121212121212121212121212121212121212121212121212",
				file_hash: "3434343434343434343434343434343434343434343434343434343434343434",
				command: "docker compose -f /srv/media/compose.yml --project-directory /srv/media -p media up -d --no-deps media",
				differences: [
					{ field: "image", file: "ghcr.io/example/media:2", container: "ghcr.io/example/media:latest" },
					{ field: "environment TOKEN", file: "set (value hidden)", container: "a different value" },
				],
			},
		},
		{
			name: "docker-updater",
			image: "ghcr.io/wow-look-at-my/docker-updater:latest",
			image_id: "0a1b2c3d4e5f",
			state: "running",
			status: "Up 3 days",
			health: "",
			created: 1786520105,
			restarts: 0,
			auto_update: true,
			mode: "image",
			update_available: false,
			current_ref: "77aa88bb99cc",
			compose_drift: {
				state: "drift",
				service: "updater",
				config_files: ["/srv/updater/compose.yml"],
				running_hash: "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff",
				file_hash: "ffffeeeeddddccccbbbbaaaa0000999988887777666655554444333322221111",
				command: "docker compose -f /srv/updater/compose.yml --project-directory /srv/updater -p updater up -d --no-deps updater",
				blocked: "docker-updater cannot recreate its own container through compose, because compose stops this process before it starts the replacement",
			},
		},
		{
			name: "git-site",
			image: "nginx:latest",
			image_id: "1234abcd5678",
			state: "running",
			status: "Up 2 days",
			health: "",
			created: 1786620105,
			restarts: 0,
			auto_update: true,
			mode: "git",
			update_available: false,
			compose_drift: {
				state: "unknown",
				service: "site",
				config_files: ["/srv/site/compose.yml"],
				command: "docker compose -f /srv/site/compose.yml -p site up -d --no-deps site",
				error: "compose file /srv/site/compose.yml is not visible to docker-updater",
			},
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

	// A <script> element, not window.eval: the compiled bundle is emitted in strict mode.
	const tag = dom.window.document.createElement("script");
	tag.textContent = js;
	dom.window.document.body.appendChild(tag);

	// copyText is a top-level function declaration, so it is a property of the window the script's own calls resolve through.
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

// bodyWithoutScript drops the injected <script> so pages are compared on
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
		// up. The containers under it are the last payload that arrived.
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

// A container Docker reports as "restarting" is in that state for the whole
// life of a crash loop. Filing it under the online rows put a container that
// has never once started beside the healthy ones.
test("a crash-looping container is offline and red", async () => {
	const page = await load(PAYLOAD);
	try {
		const online = page.doc.getElementById("group-managed-online-rows")!;
		const offline = page.doc.getElementById("group-managed-offline-rows")!;
		assert.ok(!online.textContent!.includes("log-streamer-server"), "a crash loop is not online");
		assert.ok(offline.textContent!.includes("log-streamer-server"));
		assert.ok(offline.innerHTML.includes("dot-red"), "the state dot reports a failure, not an idle container");
		assert.equal(
			page.doc.getElementById("group-managed-offline-summary")!.textContent,
			"Managed · offline (1)",
		);
	} finally {
		page.close();
	}
});

// The button applies an update past its gates, so it must ask first, and a
// "no" must send nothing.
test("force update asks first, then posts the container", async () => {
	const page = await load(PAYLOAD);
	try {
		const buttons = [...page.doc.querySelectorAll<HTMLButtonElement>(".force-btn:not(.force-btn-drift)")];
		assert.equal(buttons.length, 1, "only the held-back update gets the update button");
		assert.equal(buttons[0].textContent, "force update");
		assert.ok(buttons[0].closest("tr")!.textContent!.includes("webhook-runner"));

		const posts: { url: string; init: RequestInit }[] = [];
		page.win.fetch = (async (url: string, init: RequestInit) => {
			posts.push({ url, init });
			return { ok: true, status: 202, text: async () => "queued" };
		}) as unknown as typeof fetch;

		let asked = "";
		page.win.confirm = (msg?: string) => {
			asked = String(msg);
			return false;
		};
		buttons[0].click();
		await new Promise((r) => setTimeout(r, 0));
		assert.match(asked, /Force-update webhook-runner/);
		assert.match(asked, /pre-update endpoint answered 503/, "the question names what holds the update back");
		assert.equal(posts.length, 0, "a declined confirmation sends nothing");

		page.win.confirm = () => true;
		buttons[0].click();
		await new Promise((r) => setTimeout(r, 0));
		assert.equal(posts.length, 1);
		assert.equal(posts[0].url, "api/force-update?container=webhook-runner");
		assert.equal(posts[0].init.method, "POST");
		assert.equal((posts[0].init.headers as Record<string, string>)["X-Docker-Updater-Action"], "force-update");

		const after = page.doc.querySelector<HTMLButtonElement>(".force-btn:not(.force-btn-drift)")!;
		assert.equal(after.textContent, "queued");
		assert.ok(after.disabled, "a queued request cannot be sent twice");

		const before = bodyWithoutScript(page.html());
		const replay = await roundTrip(page.copy().json);
		try {
			assert.equal(replay.markup, before, "the queued button replays too");
		} finally {
			replay.close();
		}
	} finally {
		page.close();
	}
});

// The container carries the enable label, reports no error and offers no
// update. This is indistinguishable from an up-to-date one until the row says
// otherwise.
test("a container nothing can check says so and counts as an error", async () => {
	const page = await load(PAYLOAD);
	try {
		const rows = page.doc.getElementById("group-managed-online-rows")!;
		assert.ok(rows.textContent!.includes("cannot update"));
		assert.ok(rows.textContent!.includes("no registry repository to poll"));
		// buildhost's failed pull, s3, which no cycle will ever retry, the unknown drift, and the drift only the operator can apply.
		assert.equal(page.doc.getElementById("stat-errors")!.textContent, "4");
	} finally {
		page.close();
	}
});

// A drifted container looks healthy and up to date. The row must name the
// drift and the files. A drift only the operator can apply carries the
// command, and an unknown result must say unknown rather than nothing.
test("compose drift is on the row", async () => {
	const page = await load(PAYLOAD);
	try {
		const lines = [...page.doc.querySelectorAll(".drift")].map((n) => n.textContent || "");
		assert.equal(lines.length, 4);
		const drift = lines.find((l) => l.includes("github-state-mirror"))!;
		assert.match(drift, /drifted from compose file: service github-state-mirror in \/srv\/mirror\/compose\.yml/);
		assert.match(drift, /hashes to 9999cccc8888, the container runs aaaa1111bbbb/);
		assert.ok(drift.endsWith("Queue a recreate to apply the fileshow 1 difference"), drift);
		const blocked = lines.find((l) => l.includes("service updater"))!;
		assert.match(blocked, /docker-updater cannot recreate its own container through compose/);
		assert.ok(blocked.endsWith("Apply the file: docker compose -f /srv/updater/compose.yml --project-directory /srv/updater -p updater up -d --no-deps updatershow details"), blocked);
		const unknown = lines.find((l) => l.includes("service site"))!;
		assert.match(unknown, /compose drift unknown for service site in \/srv\/site\/compose\.yml: compose file \/srv\/site\/compose\.yml is not visible/);

		const report = (page.win.errorReport as (d: unknown) => string)(page.copy().json);
		assert.match(report, /docker-updater {2}\[ghcr\.io\/wow-look-at-my\/docker-updater:latest\][\s\S]*cannot recreate its own container/);
		assert.ok(!report.includes("gsm-rolling"), "a drift a recreate can apply is pending work, not an error");
		assert.ok(!report.includes("media"));
	} finally {
		page.close();
	}
});

// requestLog answers the dashboard's own requests: the poll gets the payload,
// and every other request is recorded and accepted.
function requestLog(page: Harness): { url: string; init: RequestInit }[] {
	const sent: { url: string; init: RequestInit }[] = [];
	page.win.fetch = (async (url: string, init: RequestInit) => {
		if (url === "api/containers") {
			return { ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(PAYLOAD)) };
		}
		sent.push({ url, init });
		return { ok: true, status: 202, text: async () => "ok" };
	}) as unknown as typeof fetch;
	return sent;
}

function rowOf(page: Harness, name: string): HTMLTableRowElement {
	const rows = [...page.doc.querySelectorAll<HTMLTableRowElement>("tbody tr")];
	return rows.find((r) => r.querySelector(".cname")!.textContent === name)!;
}

// Out of sync with nothing queued: the only action is to queue a recreate,
// and the question lists what the file changes.
test("an out-of-sync container offers a recreate that queues", async () => {
	const page = await load(PAYLOAD);
	try {
		const tr = rowOf(page, "media");
		assert.ok(tr.classList.contains("row-drift"));
		assert.equal(tr.querySelector(".up-drift")!.textContent, "out of sync");
		assert.match(tr.textContent!, /compose file changed · not applied until a recreate is queued/);
		assert.equal(tr.querySelectorAll(".force-btn:not(.recreate-btn)").length, 0, "nothing to force before a recreate is queued");
		assert.equal(tr.querySelectorAll(".cancel-btn").length, 0);
		const btn = tr.querySelector<HTMLButtonElement>(".recreate-btn")!;
		assert.equal(btn.textContent, "recreate");
		assert.equal(page.doc.getElementById("stat-updates")!.textContent, "3", "both out-of-sync rows count as pending");

		const sent = requestLog(page);
		let asked = "";
		page.win.confirm = (msg?: string) => {
			asked = String(msg);
			return true;
		};
		btn.click();
		await new Promise((r) => setTimeout(r, 0));

		assert.match(asked, /^Queue a recreate of media from its compose file\?/);
		assert.match(asked, /image: ghcr\.io\/example\/media:latest → ghcr\.io\/example\/media:2/);
		assert.match(asked, /environment TOKEN: a different value → set \(value hidden\)/);
		assert.match(asked, /It runs at the next check/);
		assert.equal(sent.length, 1);
		assert.equal(sent[0].url, "api/recreate?container=media");
		assert.equal(sent[0].init.method, "POST");
		assert.equal((sent[0].init.headers as Record<string, string>)["X-Docker-Updater-Action"], "recreate");
	} finally {
		page.close();
	}
});

// A queued recreate shows what holds it, a force button that skips the
// gates, and a cancel button that drops it.
test("a queued recreate offers force recreate and cancel", async () => {
	const page = await load(PAYLOAD);
	try {
		const tr = rowOf(page, "gsm-rolling");
		assert.equal(tr.querySelector(".up-drift")!.textContent, "recreate queued");
		assert.match(tr.textContent!, /queued 5m ago · skipped: pre-update endpoint answered 503/);
		assert.equal(tr.querySelectorAll(".recreate-btn").length, 0);
		const force = tr.querySelector<HTMLButtonElement>(".force-btn-drift")!;
		assert.equal(force.textContent, "force recreate");
		const cancel = tr.querySelector<HTMLButtonElement>(".cancel-btn")!;
		assert.equal(cancel.textContent, "cancel");

		const sent = requestLog(page);
		let asked = "";
		page.win.confirm = (msg?: string) => {
			asked = String(msg);
			return true;
		};
		force.click();
		await new Promise((r) => setTimeout(r, 0));
		assert.match(asked, /^Recreate gsm-rolling from its compose file now\?/);
		assert.match(asked, /mount \/data: bind \/srv\/mirror\/data → tmpfs/);
		assert.match(asked, /Held back by: skipped: pre-update endpoint answered 503/);
		assert.match(asked, /compose cannot restore the old one/);
		assert.equal(sent[0].url, "api/force-update?container=gsm-rolling");
		assert.equal(rowOf(page, "gsm-rolling").querySelector<HTMLButtonElement>(".force-btn-drift")!.textContent, "queued");

		rowOf(page, "gsm-rolling").querySelector<HTMLButtonElement>(".cancel-btn")!.click();
		await new Promise((r) => setTimeout(r, 0));
		assert.equal(sent.length, 2);
		assert.equal(sent[1].url, "api/recreate?container=gsm-rolling");
		assert.equal(sent[1].init.method, "DELETE");
	} finally {
		page.close();
	}
});

// The differences open under the drift line, and the open list is part of the state a copy replays.
test("the differences open under the drift line and replay", async () => {
	const page = await load(PAYLOAD);
	try {
		assert.equal(page.doc.querySelectorAll(".drift-diff").length, 0, "closed by default");
		const toggle = rowOf(page, "media").querySelector<HTMLButtonElement>(".diff-toggle")!;
		assert.equal(toggle.textContent, "show 2 differences");
		toggle.click();

		const list = rowOf(page, "media").querySelector(".drift-diff")!;
		const items = [...list.querySelectorAll("li")].map((n) => n.textContent);
		assert.deepEqual(items, [
			"container → file",
			"image: ghcr.io/example/media:latest → ghcr.io/example/media:2",
			"environment TOKEN: a different value → set (value hidden)",
		]);
		assert.equal(rowOf(page, "media").querySelector(".diff-toggle")!.textContent, "hide differences");

		const before = bodyWithoutScript(page.html());
		const { json } = page.copy();
		assert.deepEqual((json.ui as Json).drift_open, { media: true });
		const replay = await roundTrip(json);
		try {
			assert.equal(replay.markup, before, "an open list replays open");
		} finally {
			replay.close();
		}
	} finally {
		page.close();
	}
});

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The web client has no JS test harness, so exercise its sync error classification by loading
// app.js into a node vm with the browser globals stubbed. Skipped where node is not installed.
const syncStatusHarness = `
const vm = require('vm'), fs = require('fs'), assert = require('assert');
const el = () => new Proxy(function () {}, { get: () => el(), set: () => true, apply: () => el() });
const ctx = vm.createContext({
  document: { getElementById: el, querySelectorAll: () => [] },
  indexedDB: { open: () => ({}) },
  navigator: {}, console, setInterval() {}, addEventListener() {},
});
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);
const status = (fetchImpl, after) => vm.runInContext(
  '(async () => { fetch = ' + fetchImpl + '; try { await netFetch("/x"); ' + after + ' } catch (e) { return syncStatus(e); } })()', ctx);
(async () => {
  assert.strictEqual(await status('async () => { throw new TypeError("any localized text") }', ''), 'Offline — changes will sync later');
  assert.strictEqual(await status('async () => ({})', 'throw new Error("HTTP 500");'), 'Sync error: HTTP 500');
  assert.strictEqual(await status('async () => ({})', 'throw new TypeError("x is undefined");'), 'Sync error: x is undefined');
  assert.match(await status('async () => ({})', 'JSON.parse("<html>");'), /^Sync error: /);
})().catch((e) => { console.error(e); process.exit(1); });
`

func TestSyncErrorStatus(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	h := filepath.Join(t.TempDir(), "harness.js")
	if err := os.WriteFile(h, []byte(syncStatusHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, h, filepath.Join("web", "app.js")).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// smartAdd must hand back null (= add the note as typed) on every kind of LLM trouble.
const smartAddHarness = `
const vm = require('vm'), fs = require('fs'), assert = require('assert');
const el = () => new Proxy(function () {}, { get: () => el(), set: () => true, apply: () => el() });
const ctx = vm.createContext({
  document: { getElementById: el, querySelectorAll: () => [] },
  indexedDB: { open: () => ({}) },
  navigator: {}, console, setInterval() {}, addEventListener() {}, AbortController, clearTimeout, sessionStorage: { getItem: () => null, setItem() {} }, Intl,
  setTimeout: (f) => { if (globalThis.fireTimers) f(); return 0; },
});
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);
const run = (fetchImpl) => vm.runInContext('(async () => { fetch = ' + fetchImpl + '; return smartAdd("milk, call mum"); })()', ctx);
const ok = (body) => 'async () => ({ ok: true, json: async () => (' + JSON.stringify(body) + ') })';
(async () => {
  assert.strictEqual(await run('async () => { throw new TypeError("offline") }'), null);
  assert.strictEqual(await run('async () => ({ ok: false, status: 503 })'), null);
  assert.strictEqual(await run(ok({ items: null, fallback: 'down' })), null);
  assert.strictEqual(await run(ok({ items: [] })), null);
  assert.strictEqual(await run('async () => ({ ok: true, json: async () => { throw new SyntaxError("x") } })'), null);
  const items = await run(ok({ items: [{ title: 'milk', tab: 'shopping', target_date: '' }] }));
  assert.strictEqual(items.length, 1);
  assert.strictEqual(items[0].title, 'milk');
  assert.strictEqual(vm.runInContext('addedMessage([{}, {}, {}], true)', ctx), '✨ 3 items sorted by smart add');
  assert.strictEqual(vm.runInContext('addedMessage([{}], true)', ctx), '✨ 1 item sorted by smart add');
  assert.match(vm.runInContext('addedMessage(null, true)', ctx), /^Added as typed/);
  assert.strictEqual(vm.runInContext('addedMessage(null, false)', ctx), ''); // LLM asleep: the placeholder already says so
  // Chat: asleep vs unavailable, and "+ " lines become tap-to-add suggestions.
  assert.strictEqual(vm.runInContext('chatStatusText({ enabled: true, available: true }, 21)', ctx), '');
  assert.strictEqual(vm.runInContext('chatStatusText({ enabled: true, available: false }, 21)', ctx), 'Assistant is asleep (back at 07:00)');
  assert.strictEqual(vm.runInContext('chatStatusText({ enabled: true, available: false }, 6)', ctx), 'Assistant is asleep (back at 07:00)');
  assert.strictEqual(vm.runInContext('chatStatusText({ enabled: true, available: false }, 12)', ctx), 'Assistant unavailable right now');
  assert.strictEqual(vm.runInContext('chatStatusText({ enabled: false, available: false }, 12)', ctx), 'The assistant is switched off.');
  assert.deepStrictEqual(JSON.parse(JSON.stringify(vm.runInContext('parseSuggestions("You could add:\\n+ eggs for tomorrow\\n  + appeler le plombier\\n1 + 1 = 2\\n+\\n")', ctx))), ['eggs for tomorrow', 'appeler le plombier']);
  globalThis.fireTimers = true; // the 6 s client timeout fires: a hanging request is abandoned
  assert.strictEqual(await run('(u, o) => new Promise((res, rej) => o.signal.aborted ? rej(new Error("aborted")) : o.signal.addEventListener("abort", () => rej(new Error("aborted"))))'), null);
})().catch((e) => { console.error(e); process.exit(1); });
`

func TestSmartAddFallback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	h := filepath.Join(t.TempDir(), "harness.js")
	if err := os.WriteFile(h, []byte(smartAddHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, h, filepath.Join("web", "app.js")).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// Suggestions already on the Shopping list are hidden; the profile view says who wrote it.
const learnHarness = `
const vm = require('vm'), fs = require('fs'), assert = require('assert');
const el = () => new Proxy(function () {}, { get: () => el(), set: () => true, apply: () => el() });
const ctx = vm.createContext({
  document: { getElementById: el, querySelectorAll: () => [] },
  indexedDB: { open: () => ({}) },
  navigator: {}, console, setInterval() {}, addEventListener() {}, sessionStorage: { getItem: () => null, setItem() {} }, Intl,
});
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);
const js = (code) => JSON.parse(JSON.stringify(vm.runInContext(code, ctx)));
const sugs = '[{name:"Milk",reason:"r"},{name:"Eggs",reason:"r"},{name:"Skyr",reason:"r"}]';
const its = '[{title:"milk ",tab:"shopping",done:false},{title:"Eggs",tab:"shopping",done:true},{title:"Skyr",tab:"todos",done:false}]';
assert.deepStrictEqual(js('visibleSuggestions(' + sugs + ', ' + its + ').map((s) => s.name)'), ['Eggs', 'Skyr']);
assert.match(js('profileMeta({profile:"",updated_at:0,edited:false})'), /^Nothing learned yet/);
assert.match(js('profileMeta({profile:"x",updated_at:1,edited:false})'), /^Learned by houss · /);
assert.match(js('profileMeta({profile:"x",updated_at:1,edited:true})'), /^Written by you · /);
`

func TestLearnUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	h := filepath.Join(t.TempDir(), "harness.js")
	if err := os.WriteFile(h, []byte(learnHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, h, filepath.Join("web", "app.js")).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

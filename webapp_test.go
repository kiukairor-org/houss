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

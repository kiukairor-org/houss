'use strict';
// Offline-first client: items live in IndexedDB; edits are queued there and replayed via /api/sync.
// Last write wins per item by updated_at (client clock, ms).
const TABS = { shopping: 'Shopping', todos: 'Todos', midterm: 'Mid-term' };
const $ = (id) => document.getElementById(id);
let items = [];
let tab = 'all';
let addTab = 'shopping'; // list new items go to; follows the last non-'all' tab
let tagFilter = null;
let syncing = false;
let prices = {}; // item id -> {status, ...} from /api/prices (last known; never blocks the list)
let priceTotal = null;

// ---- IndexedDB ----
const dbp = new Promise((resolve, reject) => {
  const req = indexedDB.open('houss', 1);
  req.onupgradeneeded = () => {
    req.result.createObjectStore('items', { keyPath: 'id' });
    req.result.createObjectStore('queue', { autoIncrement: true });
  };
  req.onsuccess = () => resolve(req.result);
  req.onerror = () => reject(req.error);
});
async function tx(store, mode, fn) {
  const db = await dbp;
  return new Promise((resolve, reject) => {
    const t = db.transaction(store, mode);
    const out = fn(t.objectStore(store));
    t.oncomplete = () => resolve(out && out.result);
    t.onerror = () => reject(t.error);
  });
}
const idbAll = (s) => tx(s, 'readonly', (o) => o.getAll());
const idbKeys = (s) => tx(s, 'readonly', (o) => o.getAllKeys());
const idbReplaceItems = (list) => tx('items', 'readwrite', (o) => { o.clear(); list.forEach((i) => o.put(i)); });

// ---- state ----
async function queued() {
  const [keys, vals] = await Promise.all([idbKeys('queue'), idbAll('queue')]);
  return keys.map((k, i) => ({ key: k, change: vals[i] }));
}
// Server list with still-queued local changes laid over it.
function overlay(server, q) {
  const m = new Map(server.map((i) => [i.id, i]));
  q.forEach(({ change }) => {
    const cur = m.get(change.id);
    if (cur && cur.updated_at > change.updated_at) return;
    if (change.deleted) m.delete(change.id); else m.set(change.id, change);
  });
  return [...m.values()];
}

async function mutate(change) {
  change.updated_at = Date.now();
  await tx('queue', 'readwrite', (o) => o.add(change));
  if (change.deleted) await tx('items', 'readwrite', (o) => o.delete(change.id));
  else await tx('items', 'readwrite', (o) => o.put(change));
  items = await idbAll('items');
  render();
  sync();
}

async function sync() {
  if (syncing) return;
  syncing = true;
  try {
    const q = await queued();
    const res = q.length
      ? await fetch('/api/sync', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ changes: q.map((x) => x.change) }) })
      : await fetch('/api/items');
    if (res.status === 401) return showLogin();
    if (res.status >= 400 && res.status < 500 && q.length) {
      // A change the server rejects would block the queue forever: drop it.
      await tx('queue', 'readwrite', (o) => q.forEach((x) => o.delete(x.key)));
      return;
    }
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = await res.json();
    const list = Array.isArray(data) ? data : data.items;
    await tx('queue', 'readwrite', (o) => q.forEach((x) => o.delete(x.key)));
    items = overlay(list, await queued());
    await idbReplaceItems(items);
    setStatus('');
    render();
    loadPrices();
  } catch (e) {
    setStatus('Offline — changes will sync later');
  } finally {
    syncing = false;
  }
}

function setStatus(t) { $('status').textContent = t; }

// ---- rendering ----
const today = () => new Date().toLocaleDateString('sv'); // YYYY-MM-DD in local time
function dueClass(it) {
  if (it.done || !it.target_date) return '';
  const t = today();
  if (it.target_date < t) return 'overdue';
  const soon = new Date(); soon.setDate(soon.getDate() + 3);
  return it.target_date <= soon.toLocaleDateString('sv') ? 'soon' : '';
}

function render() {
  document.querySelectorAll('#tabs button').forEach((b) => b.classList.toggle('active', b.dataset.tab === tab));
  const tags = $('tags');
  tags.hidden = tab !== 'all';
  tags.replaceChildren();
  if (tab === 'all') {
    Object.entries(TABS).forEach(([k, label]) => {
      const c = document.createElement('button');
      c.type = 'button';
      c.className = 'chip' + (tagFilter === k ? ' active' : '');
      c.textContent = label;
      c.onclick = () => { tagFilter = tagFilter === k ? null : k; render(); };
      tags.append(c);
    });
  }
  const shown = items
    .filter((i) => (tab === 'all' ? !tagFilter || i.tab === tagFilter : i.tab === tab))
    .sort((a, b) => a.done - b.done || (a.target_date || '9999').localeCompare(b.target_date || '9999') || b.updated_at - a.updated_at);
  document.querySelectorAll('#tabs button').forEach((b) => {
    b.querySelector('.count').textContent = b.dataset.tab === 'all' ? items.length : items.filter((i) => i.tab === b.dataset.tab).length;
  });
  $('empty').hidden = shown.length > 0;
  const ul = $('list');
  ul.replaceChildren();
  shown.forEach((it) => {
    const li = document.createElement('li');
    li.className = [it.done ? 'done' : '', dueClass(it)].join(' ').trim();
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = it.done;
    cb.setAttribute('aria-label', 'Done');
    cb.onchange = () => mutate({ ...it, done: cb.checked });
    const title = document.createElement('span');
    title.className = 'title';
    title.append(it.title);
    if (it.tab === 'shopping') title.append(priceLine(prices[it.id]));
    li.append(cb, title);
    if (tab === 'all') {
      const tag = document.createElement('span');
      tag.className = 'tag';
      tag.textContent = TABS[it.tab];
      li.append(tag);
    }
    if (it.target_date) {
      const d = document.createElement('span');
      d.className = 'date';
      d.textContent = it.target_date;
      li.append(d);
    }
    const edit = document.createElement('button');
    edit.type = 'button';
    edit.textContent = 'Edit';
    edit.className = 'ghost';
    edit.onclick = () => {
      const t = prompt('Title', it.title);
      if (t === null || !t.trim()) return;
      const d = prompt('Target date (YYYY-MM-DD, blank for none)', it.target_date || '');
      if (d === null || (d && !/^\d{4}-\d{2}-\d{2}$/.test(d))) return;
      mutate({ ...it, title: t.trim(), target_date: d });
    };
    const del = document.createElement('button');
    del.type = 'button';
    del.textContent = '✕';
    del.setAttribute('aria-label', 'Delete');
    del.onclick = () => mutate({ ...it, deleted: true });
    li.append(edit, del);
    ul.append(li);
  });
  const tot = $('total');
  tot.hidden = !(tab === 'shopping' && priceTotal !== null);
  if (!tot.hidden) tot.textContent = 'Total: £' + priceTotal.toFixed(2);
}

function age(ms) {
  const m = Math.max(0, Math.round((Date.now() - ms) / 60000));
  return m < 60 ? m + 'm ago' : Math.round(m / 60) + 'h ago';
}
function priceLine(p) {
  const el = document.createElement('span');
  el.className = 'price';
  if (!p) return el;
  if (p.status === 'pending') { el.textContent = 'checking price…'; return el; }
  if (p.status !== 'ok') { el.textContent = 'no price'; return el; }
  const a = document.createElement('a');
  a.href = p.url;
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  a.textContent = p.product;
  el.append('£' + p.price.toFixed(2) + ' ' + p.store + ' · ', a, ' · ' + age(Date.parse(p.fetched_at)));
  return el;
}

// Prices load after the list has rendered; poll quickly while any lookup is pending.
let priceTimer = null;
async function loadPrices() {
  clearTimeout(priceTimer);
  let pending = false;
  try {
    const res = await fetch('/api/prices');
    if (res.ok) {
      const d = await res.json();
      prices = d.prices || {};
      priceTotal = d.priced ? d.total : null;
      pending = Object.values(prices).some((p) => p.status === 'pending');
      render();
    }
  } catch (e) { /* offline: keep showing the last known prices */ }
  priceTimer = setTimeout(loadPrices, pending ? 5000 : 300000);
}

// ---- screens ----
function showLogin() { $('app').hidden = true; $('login').hidden = false; }
function showApp() { $('login').hidden = true; $('app').hidden = false; }

function newId() {
  const b = new Uint8Array(8);
  crypto.getRandomValues(b);
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}

$('login-form').onsubmit = async (e) => {
  e.preventDefault();
  $('login-error').textContent = '';
  try {
    const res = await fetch('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: $('password').value }) });
    if (res.ok) { $('password').value = ''; showApp(); sync(); return; }
    $('login-error').textContent = res.status === 429 ? 'Too many attempts, wait a minute.' : 'Wrong password.';
  } catch (err) {
    $('login-error').textContent = 'Offline.';
  }
};
$('logout').onclick = async () => {
  await fetch('/api/logout', { method: 'POST' }).catch(() => {});
  items = [];
  await idbReplaceItems([]);
  showLogin();
};
$('tabs').onclick = (e) => {
  const btn = e.target.closest('button[data-tab]');
  if (!btn) return;
  tab = btn.dataset.tab;
  if (tab !== 'all') addTab = tab;
  render();
};
$('add-form').onsubmit = (e) => {
  e.preventDefault();
  const title = $('title').value.trim();
  if (!title) return;
  mutate({ id: newId(), title, tab: addTab, done: false, target_date: $('date').value });
  $('title').value = '';
  $('date').value = '';
};

(async function init() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
  items = await idbAll('items');
  // Offline: show the cached list. Online: the session check below decides login vs app.
  let authed = true;
  try {
    const r = await fetch('/api/session');
    authed = r.ok;
  } catch (e) { /* offline: trust the cache */ }
  if (!authed) { showLogin(); return; }
  showApp();
  render();
  sync();
  addEventListener('online', sync);
  setInterval(sync, 30000);
})();

'use strict';
// Offline-first client: items live in IndexedDB; edits are queued there and replayed via /api/sync.
// Last write wins per item by updated_at (client clock, ms).
const TABS = { shopping: 'Shopping', todos: 'Todos', midterm: 'Mid-term' };
const $ = (id) => document.getElementById(id);
let items = [];
let tab = 'shopping';
let addTab = 'shopping'; // list new items go to; follows the last non-'all' tab
let tagFilter = null;
let syncing = false;
let prices = {}; // item id -> {status, ...} from /api/prices (last known; never blocks the list)
let priceTotal = null;
let smart = { enabled: false, available: false }; // smart add (shared LLM), from /api/llm
let adding = false;

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

async function mutate(...changes) {
  for (const change of changes) {
    change.updated_at = Date.now();
    await tx('queue', 'readwrite', (o) => o.add(change));
    if (change.deleted) await tx('items', 'readwrite', (o) => o.delete(change.id));
    else await tx('items', 'readwrite', (o) => o.put(change));
  }
  items = await idbAll('items');
  render();
  sync();
}

// Only a rejected fetch() (no response at all) counts as being offline; anything else is a real error.
class NetworkError extends Error {}
async function netFetch(url, opts) {
  try {
    return await fetch(url, opts);
  } catch (e) {
    throw new NetworkError(e && e.message);
  }
}
function syncStatus(e) {
  return e instanceof NetworkError ? 'Offline — changes will sync later' : 'Sync error: ' + (e && e.message ? e.message : e);
}

async function sync() {
  if (syncing) return;
  syncing = true;
  try {
    const q = await queued();
    const res = q.length
      ? await netFetch('/api/sync', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ changes: q.map((x) => x.change) }) })
      : await netFetch('/api/items');
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
    loadSmart();
    loadSuggestions();
  } catch (e) {
    console.error('sync failed', e);
    setStatus(syncStatus(e));
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
    edit.className = 'icon-btn';
    edit.innerHTML = ICON_EDIT;
    edit.setAttribute('aria-label', 'Edit');
    edit.onclick = () => {
      const t = prompt('Title', it.title);
      if (t === null || !t.trim()) return;
      const d = prompt('Target date (YYYY-MM-DD, blank for none)', it.target_date || '');
      if (d === null || (d && !/^\d{4}-\d{2}-\d{2}$/.test(d))) return;
      mutate({ ...it, title: t.trim(), target_date: d });
    };
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'icon-btn danger';
    del.innerHTML = ICON_DELETE;
    del.setAttribute('aria-label', 'Delete');
    del.onclick = () => mutate({ ...it, deleted: true });
    li.append(edit, del);
    ul.append(li);
  });
  renderSuggestions();
  const tot = $('total');
  tot.hidden = !(tab === 'shopping' && priceTotal !== null);
  if (!tot.hidden) tot.textContent = 'Total: £' + priceTotal.toFixed(2);
}

// Static inline SVGs (no user input) for the item action buttons.
const ICON_EDIT = '<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>';
const ICON_DELETE = '<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M6 6l1 14h10l1-14"/><path d="M10 10v6M14 10v6"/></svg>';

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

// ---- smart add ----
// The shared LLM is off every night and may vanish at any time: everything here falls back to
// adding the note exactly as typed, and the server sorts such notes later (needs_sort).
async function loadSmart() {
  try {
    const res = await fetch('/api/llm');
    if (res.ok) smart = await res.json();
  } catch (e) { /* offline: keep the last known state */ }
  if (chatting) renderChat();
  $('title').placeholder = !smart.enabled ? 'Add item' : smart.available ? 'Add items, e.g. milk and eggs for tomorrow' : 'Add item (smart add unavailable)';
}
// Returns [{title, tab, target_date}] or null (= add the note as typed). Never waits more than 6 s.
async function smartAdd(text) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 6000);
  try {
    const res = await fetch('/api/quickadd', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text }), signal: ctl.signal });
    if (!res.ok) return null;
    const d = await res.json();
    return Array.isArray(d.items) && d.items.length ? d.items : null;
  } catch (e) {
    return null;
  } finally {
    clearTimeout(timer);
  }
}

// The cue after Add: what smart add did, or why the note went in as typed.
function addedMessage(parsed, tried) {
  if (parsed) return '✨ ' + parsed.length + (parsed.length === 1 ? ' item' : ' items') + ' sorted by smart add';
  return tried ? 'Added as typed (smart add couldn’t read this one)' : '';
}
let toastTimer = null;
function showToast(text) {
  if (!text) return;
  $('toast').textContent = text;
  $('toast').hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { $('toast').hidden = true; }, 4000);
}

// ---- suggestions, learned from what the household ticks in Shopping ----
// The server works them out (statistics, plus the shared LLM's picks when it is awake). They are
// only shown: ➕ adds one like a typed item, ✕ dismisses it so houss learns to wait longer.
let suggestions = [];
function visibleSuggestions(list, its) {
  const open = new Set(its.filter((i) => i.tab === 'shopping' && !i.done).map((i) => i.title.trim().toLowerCase()));
  return list.filter((s) => !open.has(s.name.trim().toLowerCase()));
}
async function loadSuggestions() {
  try {
    const res = await fetch('/api/suggestions');
    if (res.ok) { suggestions = (await res.json()).suggestions || []; renderSuggestions(); }
  } catch (e) { /* offline: keep the last ones */ }
}
function renderSuggestions() {
  const shown = tab === 'shopping' ? visibleSuggestions(suggestions, items) : [];
  $('suggest').hidden = shown.length === 0;
  const ul = $('suggest-list');
  ul.replaceChildren();
  shown.forEach((s) => {
    const li = document.createElement('li');
    const t = document.createElement('span');
    t.className = 'title';
    const meta = document.createElement('small');
    meta.textContent = s.reason;
    t.append(s.name, meta);
    const add = document.createElement('button');
    add.type = 'button';
    add.textContent = '➕';
    add.setAttribute('aria-label', 'Add ' + s.name);
    add.onclick = async () => {
      suggestions = suggestions.filter((x) => x !== s);
      await mutate({ id: newId(), title: s.name, tab: 'shopping', done: false, target_date: '' });
      showToast('Added ' + s.name);
    };
    const no = document.createElement('button');
    no.type = 'button';
    no.textContent = '✕';
    no.setAttribute('aria-label', 'Not now: ' + s.name);
    no.onclick = () => {
      suggestions = suggestions.filter((x) => x !== s);
      renderSuggestions();
      fetch('/api/suggestions/dismiss', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: s.name }) }).catch(() => {});
    };
    li.append(t, add, no);
    ul.append(li);
  });
}

// "What houss knows about you": the learned profile, which the household can read, edit or clear.
function profileMeta(d) {
  if (!d.profile) return 'Nothing learned yet. houss learns from what you tick in Shopping; you can also write here yourself.';
  return (d.edited ? 'Written by you' : 'Learned by houss') + ' · ' + new Date(d.updated_at).toLocaleString();
}
async function loadProfile() {
  try {
    const res = await fetch('/api/profile');
    if (!res.ok) return;
    const d = await res.json();
    if (document.activeElement !== $('profile-text')) $('profile-text').value = d.profile;
    $('profile-meta').textContent = profileMeta(d);
  } catch (e) { $('profile-meta').textContent = 'Offline.'; }
}
async function saveProfile(text) {
  const res = await fetch('/api/profile', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ profile: text }) }).catch(() => null);
  if (!res || !res.ok) { $('profile-meta').textContent = res && res.status === 400 ? 'Too long: at most 12 lines of 200 characters.' : 'Could not save.'; return; }
  const d = await res.json();
  $('profile-text').value = d.profile;
  $('profile-meta').textContent = profileMeta(d);
}
$('profile-save').onclick = () => saveProfile($('profile-text').value);
$('profile-clear').onclick = () => { if (confirm('Forget everything houss has learned about you?')) saveProfile(''); };
$('profile-refresh').onclick = async () => {
  const res = await fetch('/api/learn/refresh', { method: 'POST' }).catch(() => null);
  $('profile-meta').textContent = res && res.ok ? 'Re-learning… this takes about a minute.' : 'The assistant is asleep; houss re-learns every morning.';
  if (res && res.ok) setTimeout(() => { loadProfile(); loadSuggestions(); }, 45000);
};

// ---- assistant chat ----
// One streamed answer per message from /api/chat; the conversation lives in this tab only
// (sessionStorage). The assistant never changes data: "+ " lines become buttons that add through addNote.
let chatLog = loadChat();
let chatting = false;
let chatBusy = false;
function loadChat() {
  try { return JSON.parse(sessionStorage.getItem('houss-chat')) || []; } catch (e) { return []; }
}
function saveChat() {
  try { sessionStorage.setItem('houss-chat', JSON.stringify(chatLog)); } catch (e) { /* private mode: memory only */ }
}
function chatStatusText(s, hour) {
  if (!s.enabled) return 'The assistant is switched off.';
  if (s.available) return '';
  return hour >= 19 || hour < 7 ? 'Assistant is asleep (back at 07:00)' : 'Assistant unavailable right now';
}
function londonHour() {
  return Number(new Intl.DateTimeFormat('en-GB', { timeZone: 'Europe/London', hour: 'numeric', hourCycle: 'h23' }).format(new Date()));
}
function parseSuggestions(text) {
  return text.split('\n').filter((l) => /^\s*\+\s+\S/.test(l)).map((l) => l.replace(/^\s*\+\s+/, '').trim()).filter((l) => l.length <= 200);
}
function renderChat() {
  $('app').classList.toggle('chatting', chatting);
  const st = chatStatusText(smart, londonHour());
  $('chat-status').textContent = st;
  $('chat-input').disabled = !!st || chatBusy;
  const log = $('chat-log');
  log.replaceChildren();
  chatLog.forEach((m) => {
    const d = document.createElement('div');
    d.className = 'msg ' + m.role + (m.error ? ' error' : '');
    const p = document.createElement('p');
    p.textContent = m.content || '…';
    d.append(p);
    if (m.role === 'assistant' && !m.pending && !m.error) {
      parseSuggestions(m.content).forEach((sug) => {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'suggest';
        b.textContent = '➕ ' + sug;
        b.onclick = async () => { b.disabled = true; await addNote(sug, ''); b.textContent = '✓ ' + sug; };
        d.append(b);
      });
    }
    log.append(d);
  });
  log.scrollTop = log.scrollHeight;
}
async function sendChat(text) {
  chatLog.push({ role: 'user', content: text });
  const history = chatLog.filter((m) => !m.error && m.content).slice(-20).map(({ role, content }) => ({ role, content: content.slice(0, 1000) }));
  const reply = { role: 'assistant', content: '', pending: true };
  chatLog.push(reply);
  chatBusy = true;
  renderChat();
  try {
    const res = await fetch('/api/chat', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ messages: history }) });
    if (!res.ok) {
      const d = await res.json().catch(() => ({}));
      reply.error = true;
      if (res.status === 503) { smart = { enabled: d.error !== 'disabled', available: false }; reply.content = chatStatusText(smart, londonHour()); }
      else if (res.status === 429) reply.content = d.error || 'Too many messages, wait a minute.';
      else reply.content = 'The assistant could not answer (' + (d.error || 'HTTP ' + res.status) + ').';
    } else {
      const rd = res.body.getReader();
      const dec = new TextDecoder();
      for (;;) {
        const { done, value } = await rd.read();
        if (done) break;
        reply.content += dec.decode(value, { stream: true });
        renderChat();
      }
    }
  } catch (e) {
    if (!reply.content) { reply.content = 'Offline — the assistant needs a connection.'; reply.error = true; }
  }
  reply.pending = false;
  chatBusy = false;
  saveChat();
  renderChat();
}
$('chat-open').onclick = () => { chatting = true; renderChat(); loadSmart(); loadProfile(); };
// Music button: one random French rap quote from /api/quote in a closable popup; clicking again replaces it.
async function showQuote() {
  try {
    const res = await fetch('/api/quote');
    if (!res.ok) return;
    const q = await res.json();
    $('quote-text').textContent = '« ' + q.text + ' »';
    $('quote-by').textContent = q.artist + ' — ' + q.track;
    $('quote').hidden = false;
  } catch (e) { /* offline: leave the popup as it was */ }
}
$('quote-open').onclick = showQuote;
$('quote-close').onclick = () => { $('quote').hidden = true; };
$('chat-back').onclick = () => { chatting = false; renderChat(); };
$('chat-new').onclick = () => { chatLog = []; saveChat(); renderChat(); };
$('chat-form').onsubmit = (e) => {
  e.preventDefault();
  const text = $('chat-input').value.trim();
  if (!text || chatBusy) return;
  $('chat-input').value = '';
  sendChat(text);
};

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
  chatLog = [];
  saveChat();
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
// addNote adds one note through smart add when it is available, else exactly as typed.
async function addNote(title, date) {
  let parsed = null;
  const tried = smart.available;
  if (tried) {
    adding = true;
    $('add-form').classList.add('busy');
    parsed = await smartAdd(title);
    $('add-form').classList.remove('busy');
    adding = false;
  }
  if (parsed) {
    await mutate(...parsed.map((p) => ({ id: newId(), title: p.title, tab: p.tab, done: false, target_date: date || p.target_date })));
  } else {
    const it = { id: newId(), title, tab: addTab, done: false, target_date: date };
    if (smart.enabled) it.needs_sort = true;
    await mutate(it);
  }
  showToast(addedMessage(parsed, tried));
}
$('add-form').onsubmit = async (e) => {
  e.preventDefault();
  const title = $('title').value.trim();
  if (!title || adding) return;
  await addNote(title, $('date').value); // a date picked by hand always wins over the LLM's
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

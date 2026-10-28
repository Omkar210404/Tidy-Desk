'use strict';
const $ = (id) => document.getElementById(id);
const TOKEN = document.querySelector('meta[name="tidydesk-token"]').content;
const S = { folders: [], result: null, files: new Map(), groups: new Map(), tab: 'exact', selected: new Set(), shown: 40, distance: 8, scanId: 0, trash: null };

// ---- helpers ----
function load(k, d) { try { const v = localStorage.getItem('tidydesk.' + k); return v == null ? d : JSON.parse(v); } catch { return d; } }
function store(k, v) { try { localStorage.setItem('tidydesk.' + k, JSON.stringify(v)); } catch { /* storage unavailable */ } }

async function api(path, body) {
  const opt = { headers: { 'X-TidyDesk-Token': TOKEN } };
  if (body !== undefined) { opt.method = 'POST'; opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Something went wrong (${res.status}).`);
  return data;
}

function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB']; let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + ' ' + u[i];
}
const fmtNum = (n) => n.toLocaleString();
const fmtDate = (s) => new Date(s).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
const plural = (n, w, ws) => `${fmtNum(n)} ${n === 1 ? w : (ws || w + 's')}`;
const baseName = (p) => p.split(/[\\/]/).pop();
const dirName = (p) => { const i = Math.max(p.lastIndexOf('\\'), p.lastIndexOf('/')); return i > 0 ? p.slice(0, i) : p; };
function fmtEta(s) {
  if (s < 0) return 'estimating…';
  if (s < 60) return 'under a minute';
  const m = Math.round(s / 60);
  return m < 60 ? `about ${m} min` : `about ${(s / 3600).toFixed(1)} h`;
}

function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v === true) e.setAttribute(k, '');
    else if (v !== false && v != null) e.setAttribute(k, v);
  }
  for (const c of kids.flat()) if (c != null && c !== false) e.append(c);
  return e;
}

function toast(msg, opts = {}) {
  const t = el('div', { class: 'toast' + (opts.err ? ' err' : ''), role: 'status' }, el('span', {}, msg));
  if (opts.action) t.append(el('button', { class: 'secondary', onclick: () => { t.remove(); opts.action.fn(); } }, opts.action.label));
  $('toasts').append(t);
  setTimeout(() => t.remove(), opts.err ? 10000 : 7000);
}

function confirmBox({ title, body, ok, danger, check }) {
  const dlg = $('confirmDlg'), okBtn = $('dlgOk');
  $('dlgTitle').textContent = title;
  $('dlgBody').textContent = body;
  okBtn.textContent = ok;
  okBtn.classList.toggle('danger', !!danger);
  $('dlgCheckWrap').classList.toggle('hidden', !check);
  $('dlgCheck').checked = false;
  okBtn.disabled = !!check;
  dlg.showModal();
  $('dlgCancel').focus();
  return new Promise((resolve) => {
    const finish = (v) => {
      okBtn.onclick = $('dlgCancel').onclick = dlg.onclose = null;
      if (dlg.open) dlg.close();
      resolve(v);
    };
    okBtn.onclick = () => finish(true);
    $('dlgCancel').onclick = () => finish(false);
    dlg.onclose = () => finish(false);
  });
}

// ---- setup ----
function saveSetup() {
  store('setup', { folders: S.folders, kind: $('kind').value, minSize: $('minSize').value, exclude: $('exclude').value, similar: $('similar').checked });
}

function renderFolders() {
  const ul = $('folders');
  ul.replaceChildren();
  if (!S.folders.length) ul.append(el('li', { class: 'hint' }, 'No folders yet. Add one above to get started.'));
  S.folders.forEach((f, i) => ul.append(el('li', { title: f }, el('span', {}, f),
    el('button', { 'aria-label': 'Remove ' + f, title: 'Remove from the list', onclick: () => { S.folders.splice(i, 1); saveSetup(); renderFolders(); } }, '×'))));
  $('scanBtn').disabled = !S.folders.length;
}

function addFolder(p) {
  p = p.trim().replace(/^"|"$/g, '');
  if (!p) return;
  if (!S.folders.includes(p)) S.folders.push(p);
  saveSetup();
  renderFolders();
}

async function browse() {
  const b = $('browseBtn');
  b.disabled = true;
  b.textContent = 'Folder window open…';
  try {
    const { path } = await api('/api/pick', {});
    if (path) addFolder(path);
  } catch (e) { toast(e.message, { err: true }); }
  b.disabled = false;
  b.textContent = 'Browse…';
}

// ---- scanning ----
const PHASES = {
  walk: 'Finding files…', quick: 'Comparing files of the same size…',
  full: 'Checking the full contents of likely duplicates…', photos: 'Looking at photos…', done: 'Finishing up…',
};

async function startScan() {
  saveSetup();
  const opts = {
    roots: S.folders, kind: $('kind').value, minSize: +$('minSize').value, similar: $('similar').checked,
    exclude: $('exclude').value.split('\n').map((s) => s.trim()).filter(Boolean),
  };
  try { await api('/api/scan', opts); } catch (e) { toast(e.message, { err: true }); return; }
  S.selected.clear();
  watchScan();
}

function watchScan() {
  $('progress').classList.remove('hidden');
  $('results').classList.add('hidden');
  $('actionBar').classList.add('hidden');
  $('scanBtn').disabled = true;
  const es = new EventSource('/api/events?t=' + TOKEN);
  es.onmessage = (ev) => {
    const st = JSON.parse(ev.data);
    updateProgress(st.progress);
    if (st.state !== 'scanning') { es.close(); scanEnded(st); }
  };
  es.onerror = () => { es.close(); setTimeout(refreshState, 1500); };
}

function updateProgress(p) {
  if (!p) return;
  $('pStep').textContent = p.step <= 4 ? `Step ${p.step} of 4` : 'Done';
  $('pPhase').textContent = PHASES[p.phase] || 'Working…';
  $('pFiles').textContent = fmtNum(p.files);
  $('pBytes').textContent = fmtBytes(p.hashed);
  $('pGroups').textContent = fmtNum(p.groups);
  $('pEta').textContent = p.total > 0 ? fmtEta(p.eta) : (p.phase === 'walk' ? 'still counting…' : 'estimating…');
  const bar = $('barFill');
  bar.classList.toggle('indeterminate', !(p.total > 0));
  if (p.total > 0) bar.style.width = Math.min(100, (100 * p.done) / p.total) + '%';
  $('pErrors').textContent = p.errors ? `${plural(p.errors, 'item')} couldn't be read and will be skipped.` : '';
}

function scanEnded(st) {
  $('progress').classList.add('hidden');
  $('scanBtn').disabled = !S.folders.length;
  if (st.state === 'done') { loadResult(true); return; }
  toast(st.message || 'The scan stopped.', { err: st.state === 'error' });
  if (S.result) $('results').classList.remove('hidden');
}

async function refreshState() {
  try {
    const st = await api('/api/state');
    renderTrash(st.trash);
    if (st.status.state === 'scanning') watchScan();
    else if (!$('progress').classList.contains('hidden')) scanEnded(st.status);
    else if (st.hasResult && !S.result) loadResult(true);
  } catch {
    toast('TidyDesk lost contact with its server. Is the TidyDesk window still open?', { err: true });
  }
}

// ---- results ----
async function loadResult(reset) {
  let r;
  try { r = await api('/api/result?d=' + S.distance); } catch { return; }
  if (reset || r.scanId !== S.scanId) { S.selected.clear(); S.shown = 40; }
  S.result = r;
  S.scanId = r.scanId;
  S.files = new Map(r.files.map((f) => [f.id, f]));
  S.groups = new Map([...r.exact, ...r.similar].map((g) => [g.id, g]));
  for (const id of [...S.selected]) if (!S.files.has(id)) S.selected.delete(id);
  renderResults();
}

function renderResults() {
  const r = S.result;
  const s = r.summary;
  $('results').classList.remove('hidden');
  $('freeBig').textContent = fmtBytes(s.exactBytes);
  let sub = s.exactGroups
    ? `by removing ${plural(s.exactFiles, 'duplicate file')} in ${plural(s.exactGroups, 'group')}, keeping one copy of each.`
    : 'No exact duplicates found.';
  if (s.similarGroups) sub += ` Similar photos: ${plural(s.similarFiles, 'more photo')} in ${plural(s.similarGroups, 'group')} could free up to ${fmtBytes(s.similarBytes)}, if you decide you don't need them.`;
  $('freeSub').textContent = sub;
  const where = r.roots.length === 1 ? r.roots[0] : plural(r.roots.length, 'folder');
  $('scanMeta').textContent = `Looked at ${plural(r.filesScanned, 'file')} in ${where} in ${r.elapsed.toFixed(1)} s.`;
  $('errBox').classList.toggle('hidden', !r.errorCount);
  $('errSummary').textContent = `${plural(r.errorCount, 'item')} couldn't be read and ${r.errorCount === 1 ? 'was' : 'were'} skipped`;
  $('errList').replaceChildren(...(r.errors || []).map((e) => el('li', {}, e)),
    ...(r.errorCount > (r.errors || []).length ? [el('li', {}, `…and ${fmtNum(r.errorCount - r.errors.length)} more`)] : []));
  $('cExact').textContent = r.exact.length;
  $('cSimilar').textContent = r.similar.length;
  $('tabExact').setAttribute('aria-selected', S.tab === 'exact');
  $('tabSimilar').setAttribute('aria-selected', S.tab === 'similar');
  $('simBar').classList.toggle('hidden', S.tab !== 'similar');
  renderGroups();
}

function visibleGroups() {
  const list = S.tab === 'exact' ? S.result.exact : S.result.similar;
  const q = $('search').value.trim().toLowerCase();
  const type = $('typeFilter').value;
  const filesOf = (g) => g.files.map((id) => S.files.get(id));
  const biggest = (g) => Math.max(...filesOf(g).map((f) => f.size));
  const sorters = {
    wasted: (a, b) => b.wasted - a.wasted,
    size: (a, b) => biggest(b) - biggest(a),
    count: (a, b) => b.files.length - a.files.length,
    path: (a, b) => S.files.get(a.keep).path.localeCompare(S.files.get(b.keep).path),
  };
  return list.filter((g) => {
    const fs = filesOf(g);
    return (!type || fs.some((f) => f.kind === type)) && (!q || fs.some((f) => f.path.toLowerCase().includes(q)));
  }).sort(sorters[$('sort').value]);
}

function renderGroups() {
  const gs = visibleGroups();
  $('groups').replaceChildren(...gs.slice(0, S.shown).map(groupCard));
  const left = gs.length - S.shown;
  $('moreBtn').classList.toggle('hidden', left <= 0);
  $('moreBtn').textContent = `Show more groups (${fmtNum(left)} left)`;
  const all = S.tab === 'exact' ? S.result.exact : S.result.similar;
  let msg = '';
  if (!all.length) msg = S.tab === 'exact' ? 'No exact duplicates here. Nice and tidy!' : "No similar photos found. Try moving the slider towards 'Loosely similar', or tick 'Also look for similar photos' in Options and scan again.";
  else if (!gs.length) msg = 'No groups match your filter.';
  $('emptyMsg').textContent = msg;
  $('emptyMsg').classList.toggle('hidden', !msg);
  updateSelection();
}

function groupCard(g) {
  const fs = g.files.map((id) => S.files.get(id));
  const title = g.similar
    ? [el('strong', {}, `${fs.length} similar photos`),
       el('span', { class: 'badge sim', title: 'These photos look alike but are different files' }, `Not identical · ${g.distance <= 2 ? 'very close' : g.distance <= 6 ? 'close' : 'loose'} match`)]
    : [el('strong', {}, `${fs.length} identical copies · ${fmtBytes(fs[0].size)} each`)];
  return el('article', { class: 'group', 'data-id': g.id },
    el('header', {}, el('div', { class: 'row' }, title), el('span', { class: 'muted small' }, `${fmtBytes(g.wasted)} can be freed`)),
    el('p', { class: 'blockmsg hidden' }, 'Every file in this group is ticked. TidyDesk always keeps at least one, so untick the one you want to keep.'),
    el('div', { class: g.similar ? 'tiles' : 'list' }, fs.map((f) => fileRow(f, g))));
}

function fileRow(f, g) {
  const name = baseName(f.path);
  const cb = el('input', { type: 'checkbox', 'aria-label': `Tick ${name} to move it to TidyDesk Trash`, checked: S.selected.has(f.id) });
  cb.addEventListener('change', () => {
    if (cb.checked) S.selected.add(f.id); else S.selected.delete(f.id);
    cb.closest('.file').classList.toggle('sel', cb.checked);
    updateSelection();
  });
  const thumb = f.thumb
    ? el('button', { class: 'thumb', title: 'View larger', 'aria-label': 'View ' + name, onclick: () => view(f) },
        el('img', { src: `/api/thumb?id=${f.id}&t=${TOKEN}`, alt: '', loading: 'lazy' }))
    : el('div', { class: 'thumb', 'aria-hidden': 'true' }, (name.includes('.') ? name.split('.').pop() : 'file').slice(0, 4));
  const info = [fmtBytes(f.size), 'modified ' + fmtDate(f.modTime)];
  if (f.width) info.push(`${f.width}×${f.height}`);
  return el('div', { class: 'file' + (S.selected.has(f.id) ? ' sel' : '') },
    cb, thumb,
    el('div', { class: 'meta' },
      el('div', { class: 'name' }, name, f.id === g.keep ? el('span', { class: 'badge keep', title: 'Suggested because ' + g.reason }, 'Suggested keep') : null),
      el('div', { class: 'path', title: f.path }, el('bdi', {}, dirName(f.path))),
      el('div', { class: 'info' }, info.join(' · '), ' · ',
        el('button', { class: 'link', title: 'Open the folder that holds this file', onclick: () => api('/api/reveal', { id: f.id }).catch((e) => toast(e.message, { err: true })) }, 'Show in folder'))));
}

function view(f) {
  $('viewImg').src = `/api/thumb?id=${f.id}&s=large&t=${TOKEN}`;
  $('viewImg').alt = baseName(f.path);
  $('viewCap').textContent = `${f.path} · ${fmtBytes(f.size)}${f.width ? ` · ${f.width}×${f.height}` : ''}`;
  $('viewDlg').showModal();
}

function updateSelection() {
  if (!S.result) return;
  let bytes = 0;
  for (const id of S.selected) bytes += S.files.get(id)?.size || 0;
  const full = (g) => g.files.every((id) => S.selected.has(id));
  const blocked = [...S.groups.values()].filter(full).length;
  document.querySelectorAll('#groups .group').forEach((card) => {
    const g = S.groups.get(card.dataset.id);
    const isFull = !!g && full(g);
    card.classList.toggle('blocked', isFull);
    card.querySelector('.blockmsg').classList.toggle('hidden', !isFull);
  });
  $('actionBar').classList.toggle('hidden', S.selected.size === 0);
  $('selText').textContent = `${plural(S.selected.size, 'file')} ticked · ${fmtBytes(bytes)}`;
  $('selWarn').textContent = blocked ? `${plural(blocked, 'group')} would lose every copy` : '';
  $('moveBtn').disabled = blocked > 0;
}

function tickSuggested() {
  for (const g of visibleGroups()) for (const id of g.files) if (id !== g.keep) S.selected.add(id);
  renderGroups();
  toast('Ticked every file except the suggested keep. Nothing moves until you confirm.');
}

// ---- actions ----
async function moveSelected() {
  const ids = [...S.selected];
  const bytes = ids.reduce((n, id) => n + (S.files.get(id)?.size || 0), 0);
  const ok = await confirmBox({
    title: `Move ${plural(ids.length, 'file')}, ${fmtBytes(bytes)}, to TidyDesk Trash?`,
    body: 'They go into a "TidyDesk Trash" folder inside the folder you scanned. Nothing is deleted: you can undo this any time until you empty TidyDesk Trash.',
    ok: 'Move to TidyDesk Trash',
  });
  if (!ok) return;
  $('moveBtn').disabled = true;
  try {
    const { report, trash } = await api('/api/remove', { scanId: S.scanId, ids, distance: S.distance });
    S.selected.clear();
    renderTrash(trash);
    let msg = `Moved ${plural(report.moved, 'file')} (${fmtBytes(report.bytes)}) to TidyDesk Trash.`;
    if (report.failed.length) msg += ` ${plural(report.failed.length, 'file')} skipped, e.g. ${report.failed[0]}`;
    toast(msg, { err: report.moved === 0, action: report.moved ? { label: 'Undo', fn: undo } : null });
    await loadResult();
  } catch (e) {
    toast(e.message, { err: true });
    updateSelection();
  }
}

async function undo() {
  try {
    const { report, trash } = await api('/api/undo', {});
    renderTrash(trash);
    let msg = `Put back ${plural(report.restored, 'file')} where ${report.restored === 1 ? 'it was' : 'they were'}.`;
    if (report.failed.length) msg += ` ${plural(report.failed.length, 'file')} couldn't be restored, e.g. ${report.failed[0]}`;
    toast(msg, { err: report.restored === 0 });
    if (S.result) await loadResult();
  } catch (e) { toast(e.message, { err: true }); }
}

async function emptyTrash() {
  const t = S.trash;
  const ok = await confirmBox({
    title: `Permanently delete ${plural(t.files, 'file')} (${fmtBytes(t.bytes)})?`,
    body: 'This empties TidyDesk Trash. The files will be gone for good and Undo can no longer bring them back.',
    ok: 'Delete for good', danger: true, check: true,
  });
  if (!ok) return;
  try {
    const { report, trash } = await api('/api/empty', { confirm: 'EMPTY' });
    renderTrash(trash);
    toast(`Deleted ${plural(report.deleted, 'file')} and freed ${fmtBytes(report.bytes)}.` + (report.failed.length ? ` ${report.failed.length} couldn't be deleted.` : ''), { err: report.failed.length > 0 });
  } catch (e) { toast(e.message, { err: true }); }
}

function renderTrash(t) {
  S.trash = t;
  $('trash').classList.toggle('full', t.files > 0);
  $('trashText').textContent = t.files ? `TidyDesk Trash: ${plural(t.files, 'file')} · ${fmtBytes(t.bytes)}` : 'TidyDesk Trash is empty';
  $('trashText').title = t.folders.length ? 'Stored in:\n' + t.folders.join('\n') : '';
  $('undoBtn').disabled = !t.lastFiles;
  $('undoBtn').textContent = t.lastFiles ? `Undo last move (${plural(t.lastFiles, 'file')})` : 'Undo last move';
  $('emptyBtn').disabled = !t.files;
}

// ---- wiring ----
function setTheme(t) { document.documentElement.dataset.theme = t; store('theme', t); }

function init() {
  setTheme(load('theme', 'dark'));
  const setup = load('setup', {});
  S.folders = Array.isArray(setup.folders) ? setup.folders : [];
  if (setup.kind) $('kind').value = setup.kind;
  $('minSize').value = setup.minSize || '1024';
  $('exclude').value = setup.exclude || '';
  if (setup.similar === false) $('similar').checked = false;
  renderFolders();

  $('addForm').addEventListener('submit', (e) => { e.preventDefault(); addFolder($('folderInput').value); $('folderInput').value = ''; });
  $('browseBtn').addEventListener('click', browse);
  for (const id of ['kind', 'minSize', 'exclude', 'similar']) $(id).addEventListener('change', saveSetup);
  $('scanBtn').addEventListener('click', startScan);
  $('cancelBtn').addEventListener('click', () => api('/api/cancel', {}).catch(() => {}));
  $('themeBtn').addEventListener('click', () => setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark'));
  $('undoBtn').addEventListener('click', undo);
  $('emptyBtn').addEventListener('click', emptyTrash);
  $('moveBtn').addEventListener('click', moveSelected);
  $('pickSuggested').addEventListener('click', tickSuggested);
  $('clearSel').addEventListener('click', () => { S.selected.clear(); renderGroups(); });
  $('moreBtn').addEventListener('click', () => { S.shown += 40; renderGroups(); });
  $('dlgCheck').addEventListener('change', () => { $('dlgOk').disabled = !$('dlgCheck').checked; });
  $('viewClose').addEventListener('click', () => $('viewDlg').close());
  for (const id of ['search', 'typeFilter', 'sort']) $(id).addEventListener('input', () => { S.shown = 40; if (S.result) renderGroups(); });
  for (const [id, tab] of [['tabExact', 'exact'], ['tabSimilar', 'similar']]) {
    $(id).addEventListener('click', () => { S.tab = tab; S.shown = 40; renderResults(); });
  }
  let simTimer;
  $('sim').addEventListener('input', () => {
    clearTimeout(simTimer);
    simTimer = setTimeout(() => { S.distance = +$('sim').value; loadResult(); }, 250);
  });
  document.addEventListener('keydown', (e) => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
    if (e.key === '/' && !typing && S.result) { e.preventDefault(); $('search').focus(); }
  });
  refreshState();
}

init();

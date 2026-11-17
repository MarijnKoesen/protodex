// SPDX-License-Identifier: Apache-2.0
// Protodex web UI. Talks only to the public Connect API (JSON protocol).

const MODULE = '/buf.registry.module.v1.';
const TOKEN_KEY = 'protodex.token';

export const token = {
  get: () => sessionStorage.getItem(TOKEN_KEY) || '',
  set: (v) => sessionStorage.setItem(TOKEN_KEY, v),
  clear: () => sessionStorage.removeItem(TOKEN_KEY),
};

export class ApiError extends Error {
  constructor(code, message) { super(message); this.code = code; }
}

export async function rpc(service, method, body = {}) {
  const headers = { 'Content-Type': 'application/json', 'Connect-Protocol-Version': '1' };
  if (token.get()) headers.Authorization = 'Bearer ' + token.get();
  const res = await fetch(`/${service}/${method}`, { method: 'POST', headers, body: JSON.stringify(body) });
  const text = await res.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { /* non-JSON error body */ }
  if (!res.ok) throw new ApiError(data.code || String(res.status), data.message || res.statusText);
  return data;
}
const mod = (m, b) => rpc(MODULE.slice(1) + 'ModuleService', m, b);
const commitSvc = (m, b) => rpc(MODULE.slice(1) + 'CommitService', m, b);
const labelSvc = (m, b) => rpc(MODULE.slice(1) + 'LabelService', m, b);
const downloadSvc = (m, b) => rpc(MODULE.slice(1) + 'DownloadService', m, b);
const tokenSvc = (m, b) => rpc('buf.registry.iam.v1.TokenService', m, b);
const userSvc = (m, b) => rpc('buf.registry.owner.v1.UserService', m, b);
const ownerSvc = (m, b) => rpc('buf.registry.owner.v1.OwnerService', m, b);

// el builds DOM with textContent only, so API data is never parsed as HTML.
export function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === false || v == null) continue;
    if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const k of kids.flat()) e.append(k instanceof Node ? k : document.createTextNode(k ?? ''));
  return e;
}
const fmtTime = (t) => (t ? new Date(t).toISOString().replace('T', ' ').replace(/\.\d+Z$/, 'Z') : '—');
const enc = encodeURIComponent;
const short = (id) => (id || '').slice(0, 12);

function table(headers, rows, caption) {
  return el('table', {},
    el('caption', { class: 'muted', style: false }, caption || ''),
    el('thead', {}, el('tr', {}, headers.map((h) => el('th', { scope: 'col' }, h)))),
    el('tbody', {}, rows.map((r) => el('tr', {}, r.map((c) => el('td', {}, c))))));
}
const pager = (next, onMore) => next ? el('p', {}, el('button', { type: 'button', onclick: onMore }, 'Load more')) : '';

// ---- views ----
// Each view receives the <main> container and route params.

async function modulesView(main) {
  main.replaceChildren(el('h1', {}, 'Modules'));
  const filter = el('input', { id: 'owner-filter', placeholder: 'e.g. acme' });
  const list = el('div');
  const more = el('div');
  let pageToken = '';
  async function load(reset) {
    if (reset) { pageToken = ''; list.replaceChildren(); }
    const body = { pageSize: 50, pageToken };
    if (filter.value.trim()) body.ownerRefs = [{ name: filter.value.trim() }];
    const res = await mod('ListModules', body);
    pageToken = res.nextPageToken || '';
    const ownerNames = await ownerNamesFor((res.modules || []).map((m) => m.ownerId));
    const rows = (res.modules || []).map((m) => {
      const o = ownerNames.get(m.ownerId) || m.ownerId;
      return [el('a', { href: `#/m/${enc(o)}/${enc(m.name)}` }, `${o}/${m.name}`),
        m.description || '', visibilityChip(m), fmtTime(m.updateTime)];
    });
    if (reset || !list.firstChild) list.replaceChildren(table(['Module', 'Description', 'Visibility', 'Updated'], rows, 'Modules'));
    else list.querySelector('tbody').append(...table([], rows).querySelectorAll('tbody tr'));
    if (reset && !rows.length) list.replaceChildren(el('p', { class: 'muted' }, 'No modules found.'));
    more.replaceChildren(pager(pageToken, () => guard(main, () => load(false))));
  }
  main.append(el('form', { class: 'inline', onsubmit: (e) => { e.preventDefault(); guard(main, () => load(true)); } },
    el('label', { for: 'owner-filter' }, 'Filter by owner'), filter, el('button', { type: 'submit' }, 'Filter')), list, more);
  await load(true);
}
const visibilityChip = (m) => el('span', { class: 'chip' }, (m.visibility || '').replace('MODULE_VISIBILITY_', '').toLowerCase());

const ownerCache = new Map();
async function ownerNamesFor(ids) {
  const missing = [...new Set(ids)].filter((id) => id && !ownerCache.has(id));
  if (missing.length) {
    const res = await ownerSvc('GetOwners', { ownerRefs: missing.map((id) => ({ id })) });
    for (const o of res.owners || []) {
      const v = o.user || o.organization;
      if (v) ownerCache.set(v.id, v.name);
    }
  }
  return ownerCache;
}

function moduleHeader(owner, name, active, m) {
  const base = `#/m/${enc(owner)}/${enc(name)}`;
  const tab = (path, label) => el('a', { href: base + path, 'aria-current': active === path ? 'page' : false }, label);
  return [el('h1', {}, `${owner}/${name}`),
    m ? el('p', {}, m.description || '', ' ', m.url ? el('a', { href: safeUrl(m.url), rel: 'noopener noreferrer' }, m.url) : '') : '',
    el('nav', { class: 'tabs', 'aria-label': 'Module' }, tab('', 'Overview'), tab('/commits', 'Commits'), tab('/labels', 'Labels'), tab('/files', 'Files'))];
}
const safeUrl = (u) => (/^https?:\/\//i.test(u) ? u : '#');
const nameRef = (owner, module, labelName) => ({ name: { owner, module, ...(labelName ? { labelName } : {}) } });

async function moduleView(main, [owner, name]) {
  const { modules } = await mod('GetModules', { moduleRefs: [{ name: { owner, module: name } }] });
  const m = modules[0];
  const [commits, labels] = await Promise.all([
    commitSvc('ListCommits', { pageSize: 5, resourceRef: nameRef(owner, name), order: 'ORDER_CREATE_TIME_DESC' }),
    labelSvc('ListLabels', { pageSize: 100, resourceRef: nameRef(owner, name) })]);
  const last = (commits.commits || [])[0];
  main.replaceChildren(...moduleHeader(owner, name, '', m),
    el('dl', {},
      el('dt', {}, 'Visibility'), el('dd', {}, visibilityChip(m)),
      el('dt', {}, 'State'), el('dd', {}, (m.state || '').replace('MODULE_STATE_', '').toLowerCase()),
      el('dt', {}, 'Default label'), el('dd', {}, m.defaultLabelName),
      el('dt', {}, 'Created'), el('dd', {}, fmtTime(m.createTime))),
    provenance(last),
    el('h2', {}, 'Labels'), labelsTable(owner, name, labels.labels || []),
    el('h2', {}, 'Recent commits'), commitsTable(owner, name, commits.commits || []));
}

// Mirror provenance: mirrored commits carry the upstream location in
// source_control_url; show it (and nothing when absent).
function provenance(commit) {
  if (!commit?.sourceControlUrl) return '';
  return el('p', { id: 'provenance' }, el('span', { class: 'chip' }, 'source'), ' ',
    el('a', { href: safeUrl(commit.sourceControlUrl), rel: 'noopener noreferrer' }, commit.sourceControlUrl));
}

function commitsTable(owner, name, commits) {
  if (!commits.length) return el('p', { class: 'muted' }, 'No commits.');
  return table(['Commit', 'Created', 'Digest', 'Source'], commits.map((c) => [
    el('a', { class: 'mono', href: `#/m/${enc(owner)}/${enc(name)}/files/${enc(c.id)}` }, short(c.id)),
    fmtTime(c.createTime),
    el('span', { class: 'mono' }, `${(c.digest?.type || '').replace('DIGEST_TYPE_', '').toLowerCase()}:${short(c.digest?.value)}`),
    c.sourceControlUrl ? el('a', { href: safeUrl(c.sourceControlUrl), rel: 'noopener noreferrer' }, 'source') : '']), 'Commits');
}

function labelsTable(owner, name, labels) {
  if (!labels.length) return el('p', { class: 'muted' }, 'No labels.');
  return table(['Label', 'Commit', 'Updated', 'State'], labels.map((l) => [
    el('a', { href: `#/m/${enc(owner)}/${enc(name)}/files/${enc(l.name)}` }, l.name),
    el('span', { class: 'mono' }, short(l.commitId)), fmtTime(l.updateTime),
    l.archiveTime ? el('span', { class: 'chip' }, 'archived') : '']), 'Labels');
}

async function commitsView(main, [owner, name]) {
  main.replaceChildren(...moduleHeader(owner, name, '/commits'));
  const out = el('div'); const more = el('div'); main.append(out, more);
  let pageToken = ''; let shown = [];
  async function load() {
    const res = await commitSvc('ListCommits', { pageSize: 50, pageToken, resourceRef: nameRef(owner, name), order: 'ORDER_CREATE_TIME_DESC' });
    shown = shown.concat(res.commits || []); pageToken = res.nextPageToken || '';
    out.replaceChildren(commitsTable(owner, name, shown));
    more.replaceChildren(pager(pageToken, () => guard(main, load)));
  }
  await load();
}

async function labelsView(main, [owner, name]) {
  main.replaceChildren(...moduleHeader(owner, name, '/labels'));
  const out = el('div'); const more = el('div'); main.append(out, more);
  let pageToken = ''; let shown = [];
  async function load() {
    const res = await labelSvc('ListLabels', { pageSize: 50, pageToken, resourceRef: nameRef(owner, name) });
    shown = shown.concat(res.labels || []); pageToken = res.nextPageToken || '';
    out.replaceChildren(labelsTable(owner, name, shown));
    more.replaceChildren(pager(pageToken, () => guard(main, load)));
  }
  await load();
}

async function filesView(main, [owner, name, ref, path]) {
  main.replaceChildren(...moduleHeader(owner, name, '/files'));
  // Without an explicit ref show the newest commit (the default label may not exist yet).
  if (!ref) {
    const latest = await commitSvc('ListCommits', { pageSize: 1, resourceRef: nameRef(owner, name), order: 'ORDER_CREATE_TIME_DESC' });
    ref = latest.commits?.[0]?.id;
    if (!ref) { main.append(el('p', { class: 'muted' }, 'No commits yet.')); return; }
  }
  const resourceRef = { name: { owner, module: name, ref } };
  const res = await downloadSvc('Download', { values: [{ resourceRef }] });
  const content = res.contents[0];
  const files = (content.files || []).slice().sort((a, b) => a.path.localeCompare(b.path));
  const current = files.find((f) => f.path === path) || files.find((f) => f.path.endsWith('.proto')) || files[0];
  const base = `#/m/${enc(owner)}/${enc(name)}/files/${enc(ref)}`;
  main.append(el('p', { class: 'muted' }, 'Commit ', el('code', {}, content.commit.id), ' · ', `${files.length} files`),
    provenance(content.commit),
    el('div', { class: 'files' },
      el('nav', { 'aria-label': 'Files' }, el('ul', {}, files.map((f) => el('li', {},
        el('a', { href: `${base}/${enc(f.path)}`, 'aria-current': f === current ? 'page' : false }, f.path))))),
      current ? el('section', { 'aria-label': current.path }, el('h2', { class: 'mono' }, current.path),
        el('pre', { tabindex: '0' }, el('code', {}, new TextDecoder().decode(Uint8Array.from(atob(current.content || ''), (c) => c.charCodeAt(0)))))) : el('p', {}, 'No files.')));
}

async function tokensView(main) {
  main.replaceChildren(el('h1', {}, 'Access tokens'));
  const secretBox = el('div', { 'aria-live': 'polite' });
  const list = el('div');
  const nameIn = el('input', { id: 'tok-name', minlength: '3', maxlength: '32', required: true });
  const expIn = el('input', { id: 'tok-exp', type: 'number', min: '1', placeholder: 'never' });
  async function refresh() {
    const res = await tokenSvc('ListTokens', { pageSize: 100 });
    const toks = res.tokens || [];
    list.replaceChildren(toks.length ? table(['Name', 'Created', 'Expires', 'Last used', ''], toks.map((t) => [
      t.name || short(t.id), fmtTime(t.createTime), fmtTime(t.expireTime), fmtTime(t.lastUsedTime),
      el('button', { type: 'button', 'aria-label': `Delete token ${t.name || t.id}`, onclick: () => guard(main, async () => {
        if (!confirm(`Delete token "${t.name || t.id}"?`)) return;
        await tokenSvc('DeleteTokens', { tokenRefs: [{ id: t.id }] });
        await refresh();
      }) }, 'Delete')]), 'Tokens') : el('p', { class: 'muted' }, 'No tokens.'));
  }
  main.append(el('form', { class: 'inline', onsubmit: (e) => {
    e.preventDefault();
    guard(main, async () => {
      const value = { name: nameIn.value };
      if (expIn.value) value.expireTime = new Date(Date.now() + Number(expIn.value) * 86400000).toISOString();
      const res = await tokenSvc('CreateTokens', { values: [value] });
      secretBox.replaceChildren(el('h2', {}, 'New token'), el('p', {}, 'Copy it now; it will not be shown again.'),
        el('p', { class: 'secret mono', id: 'new-secret' }, res.values[0].secret));
      nameIn.value = '';
      await refresh();
    });
  } }, el('label', { for: 'tok-name' }, 'Name'), nameIn, el('label', { for: 'tok-exp' }, 'Expires in (days)'), expIn,
  el('button', { type: 'submit', class: 'primary' }, 'Create token')), secretBox, list);
  await refresh();
}

function loginView(main) {
  const input = el('input', { id: 'login-token', type: 'password', autocomplete: 'off', required: true });
  main.replaceChildren(el('h1', {}, 'Sign in'),
    el('p', {}, 'Paste a personal access token. Tokens are kept for this browser tab only.'),
    el('form', { class: 'inline', onsubmit: (e) => {
      e.preventDefault();
      token.set(input.value.trim());
      guard(main, async () => { await userSvc('GetCurrentUser'); location.hash = '#/'; renderNav(); }, () => { token.clear(); });
    } }, el('label', { for: 'login-token' }, 'Token'), input, el('button', { type: 'submit', class: 'primary' }, 'Sign in')),
    el('p', {}, 'Or ', el('a', { href: '/auth/login' }, 'sign in with OIDC'), ' (if enabled) to get a token, then paste it above.'));
}

// ---- routing ----
const routes = [
  [/^\/?$/, modulesView],
  [/^\/m\/([^/]+)\/([^/]+)$/, moduleView],
  [/^\/m\/([^/]+)\/([^/]+)\/commits$/, commitsView],
  [/^\/m\/([^/]+)\/([^/]+)\/labels$/, labelsView],
  [/^\/m\/([^/]+)\/([^/]+)\/files(?:\/([^/]+)(?:\/(.+))?)?$/, filesView],
  [/^\/tokens$/, tokensView],
  [/^\/login$/, loginView],
];

// guard runs fn and renders errors (unauthenticated → login) instead of throwing.
async function guard(main, fn, onError) {
  try { await fn(); } catch (err) {
    onError?.(err);
    if (err instanceof ApiError && err.code === 'unauthenticated' && !onError) { location.hash = '#/login'; return; }
    main.querySelector('.error')?.remove();
    main.prepend(el('p', { class: 'error', role: 'alert' }, err instanceof ApiError ? `${err.code}: ${err.message}` : `Error: ${err.message}`));
  }
}

async function render() {
  const main = document.getElementById('main');
  const path = location.hash.replace(/^#/, '') || '/';
  for (const [re, view] of routes) {
    const m = re.exec(path);
    if (m) {
      const params = m.slice(1).map((p) => (p === undefined ? p : decodeURIComponent(p)));
      document.title = 'Protodex';
      await guard(main, () => view(main, params));
      main.focus({ preventScroll: true });
      return;
    }
  }
  main.replaceChildren(el('h1', {}, 'Not found'), el('a', { href: '#/' }, 'Back to modules'));
}

function renderNav() {
  const nav = document.getElementById('nav');
  const theme = el('button', { type: 'button', 'aria-label': 'Toggle light/dark theme', onclick: () => {
    const root = document.documentElement;
    const dark = (root.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')) === 'dark';
    root.dataset.theme = dark ? 'light' : 'dark';
    localStorage.setItem('protodex.theme', root.dataset.theme);
  } }, 'Theme');
  nav.replaceChildren(el('a', { href: '#/' }, 'Modules'), el('a', { href: '#/tokens' }, 'Tokens'),
    token.get() ? el('button', { type: 'button', onclick: () => { token.clear(); renderNav(); location.hash = '#/login'; } }, 'Sign out')
      : el('a', { href: '#/login' }, 'Sign in'), theme);
}

if (typeof document !== 'undefined') {
  const saved = localStorage.getItem('protodex.theme');
  if (saved) document.documentElement.dataset.theme = saved;
  renderNav();
  addEventListener('hashchange', render);
  render();
}

// lwd-web app shell — an Alpine.js single-page dashboard.
//
// Everything lives in one Alpine component (dashboard()) registered below.
// The component talks to the browser-facing JSON API under /api (see
// internal/web/api.go) and to /api/apps/{name}/logs over Server-Sent Events.
// There is no build step: this file is served as-is at /static/app.js.

document.addEventListener('alpine:init', () => {
  Alpine.data('dashboard', dashboard);
});

/**
 * apiFetch wraps fetch() for the /api surface:
 *  - throws a typed Error (with .status and .body) on non-2xx responses
 *  - redirects to /login on 401 (session expired / never logged in)
 *  - lets callers distinguish "daemon unreachable" (502) from other errors
 */
async function apiFetch(path, opts) {
  const res = await fetch(path, opts);

  if (res.status === 401) {
    window.location.assign('/login');
    // Throw so callers' promise chains stop; the redirect above makes the
    // rejection moot in practice.
    throw new ApiError(401, 'unauthorized', null);
  }

  if (!res.ok) {
    let body = null;
    let message = res.statusText || ('HTTP ' + res.status);
    try {
      body = await res.json();
      if (body && body.error) message = body.error;
    } catch (e) {
      // body wasn't JSON (or was empty) — fall back to statusText.
    }
    throw new ApiError(res.status, message, body);
  }

  if (res.status === 204) return null;
  const ct = res.headers.get('content-type') || '';
  if (ct.includes('application/json')) return res.json();
  return res.text();
}

class ApiError extends Error {
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

// Status → visual vocabulary. Anything not "running"/"failed" reads as
// "retired" (covers apps the daemon has torn down and any future status
// values we don't know about yet, so the UI degrades gracefully).
function statusKind(status) {
  if (status === 'running') return 'running';
  if (status === 'failed') return 'failed';
  return 'retired';
}

// healthKind maps a reconciler.SurfaceState value to the pill/dot class
// suffix, the same way statusKind does for app status: it whitelists the
// known values and falls back to 'retired' (the neutral/idle vocabulary) for
// anything else, so an unrecognized state degrades gracefully to a plain
// grey pill instead of emitting a `pill-<state>`/`dot-<state>` class that
// matches no CSS rule at all.
function healthKind(state) {
  if (state === 'healthy' || state === 'degraded' || state === 'healing' || state === 'failed') return state;
  return 'retired';
}

function timeAgo(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  const seconds = Math.floor((Date.now() - d.getTime()) / 1000);
  if (seconds < 5) return 'just now';
  const units = [
    ['y', 31536000], ['mo', 2592000], ['d', 86400], ['h', 3600], ['m', 60],
  ];
  for (const [label, secs] of units) {
    const v = Math.floor(seconds / secs);
    if (v >= 1) return v + label + ' ago';
  }
  return seconds + 's ago';
}

function fullTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleString();
}

// shortImage trims a long image reference (registry/repo@sha256:digest) down
// to something that fits a card without hiding the meaningful tail.
function shortImage(image) {
  if (!image) return '—';
  const at = image.indexOf('@sha256:');
  if (at !== -1) {
    return image.slice(0, at) + '@' + image.slice(at + 1, at + 15) + '…';
  }
  return image;
}

// specToToml renders a spec.App JSON snapshot (as stored in a Deployment's
// Spec field) back into readable lwd.toml text. It's a best-effort renderer
// for display + "edit & apply", not a byte-exact round trip — comments and
// original formatting from the source file aren't preserved (they were
// never in the JSON snapshot to begin with).
function specToToml(json) {
  let spec;
  try {
    spec = JSON.parse(json);
  } catch (e) {
    return '# could not parse stored spec: ' + e.message;
  }

  const lines = [];
  const kv = (key, value) => {
    if (value === undefined || value === null || value === '') return;
    lines.push(`${key} = ${tomlString(value)}`);
  };

  kv('name', spec.Name);
  kv('image', spec.Image);
  kv('domain', spec.Domain);
  if (spec.Port) lines.push(`port = ${spec.Port}`);
  if (spec.Node && spec.Node !== 'local') kv('node', spec.Node);
  if (spec.Compose) kv('compose', spec.Compose);
  if (spec.Service) kv('service', spec.Service);

  if (spec.Secrets && spec.Secrets.length) {
    lines.push(`secrets = [${spec.Secrets.map(tomlString).join(', ')}]`);
  }

  if (spec.Env && Object.keys(spec.Env).length) {
    lines.push('');
    lines.push('[env]');
    for (const [k, v] of Object.entries(spec.Env)) {
      lines.push(`${k} = ${tomlString(v)}`);
    }
  }

  if (spec.Health && (spec.Health.Path || spec.Health.RawTimeout)) {
    lines.push('');
    lines.push('[health]');
    if (spec.Health.Path) lines.push(`path = ${tomlString(spec.Health.Path)}`);
    const timeout = spec.Health.RawTimeout || durationString(spec.Health.Timeout);
    if (timeout) lines.push(`timeout = ${tomlString(timeout)}`);
  }

  return lines.join('\n') + '\n';
}

function tomlString(v) {
  return JSON.stringify(String(v));
}

// ---------------------------------------------------------------------------
// Deploy-modal toml generation (From Git / Builder tabs)
//
// These build an lwd.toml document client-side from form state, entirely so
// the "From Git" and "Builder" tabs can POST to the existing /api/apply
// (raw-toml) endpoint without any new server surface. They're pure functions
// of the form objects so the live preview pane can call them on every
// keystroke, and so a throwaway `go run` can sanity-check the output against
// spec.Parse.
//
// TOML rule that matters here: root-level scalar keys (name/domain/port/env/
// secrets/image) must be emitted before any table header ([git], [build]) or
// array-of-tables header ([[services]]) — once a header is written, bare
// `key = value` lines belong to that table.

// envRowsToInline renders [{key,value}, ...] rows into an inline table
// (`{ "K" = "V", ... }`), skipping rows with a blank key. Returns '' if no
// rows have a usable key (the caller omits the `env = ...` line entirely).
//
// Both the key and the value are passed through tomlString (quoted TOML
// basic strings). Quoting the key is a security-relevant choice, not just
// style: a bare/unquoted key is emitted as raw TOML syntax, so a key
// containing e.g. a newline, `"`, `#`, or `}` could otherwise terminate the
// inline table early and inject arbitrary top-level TOML (including a new
// `[[services]]` block) into the generated document, which is then sent to
// the server's /api/apply endpoint. A quoted TOML basic string can't break
// out of its own quotes (tomlString uses JSON.stringify, which escapes `"`,
// `\`, and control characters), so a quoted key is always a single atomic
// token no matter what characters it contains.
function envRowsToInline(rows) {
  const entries = (rows || [])
    .filter((r) => r && r.key && r.key.trim() !== '')
    .map((r) => `${tomlString(r.key.trim())} = ${tomlString(r.value || '')}`);
  if (!entries.length) return '';
  return `{ ${entries.join(', ')} }`;
}

// namesToArray renders a list of plain strings into a toml string array
// (`["A", "B"]`), trimming and dropping blanks. Returns '' if nothing is left.
function namesToArray(names) {
  const list = (names || []).map((n) => (n || '').trim()).filter(Boolean);
  if (!list.length) return '';
  return `[${list.map(tomlString).join(', ')}]`;
}

// appendServiceTables pushes one `[[services]]` array-of-tables block per
// backing-service row onto `lines`. A row with neither a name nor an image
// is treated as an unfinished/blank row and skipped.
function appendServiceTables(lines, services) {
  for (const svc of services || []) {
    if (!svc) continue;
    const name = (svc.name || '').trim();
    const image = (svc.image || '').trim();
    if (!name && !image) continue;

    lines.push('');
    lines.push('[[services]]');
    lines.push(`name = ${tomlString(name)}`);
    lines.push(`image = ${tomlString(image)}`);
    const command = (svc.command || '').trim();
    if (command) lines.push(`command = ${tomlString(command)}`);
    const env = envRowsToInline(svc.env);
    if (env) lines.push(`env = ${env}`);
    const secrets = namesToArray(svc.secrets);
    if (secrets) lines.push(`secrets = ${secrets}`);
    const volume = (svc.volume || '').trim();
    if (volume) lines.push(`volume = ${tomlString(volume)}`);
  }
}

// buildGitToml renders the "From Git" form into an lwd.toml document:
// top-level app fields, a [git] block, a [build] block, then any declared
// [[services]].
function buildGitToml(f) {
  const lines = [];
  lines.push(`name = ${tomlString((f.name || '').trim())}`);
  lines.push(`domain = ${tomlString((f.domain || '').trim())}`);
  if (String(f.port || '').trim()) lines.push(`port = ${parseInt(f.port, 10)}`);
  if (f.node && f.node !== 'local') lines.push(`node = ${tomlString(f.node)}`);
  const env = envRowsToInline(f.env);
  if (env) lines.push(`env = ${env}`);
  const secrets = namesToArray(f.secrets);
  if (secrets) lines.push(`secrets = ${secrets}`);

  lines.push('');
  lines.push('[git]');
  lines.push(`url = ${tomlString((f.url || '').trim())}`);
  lines.push(`ref = ${tomlString((f.ref || '').trim() || 'main')}`);
  const subdir = (f.subdir || '').trim();
  if (subdir) lines.push(`path = ${tomlString(subdir)}`);

  lines.push('');
  lines.push('[build]');
  lines.push(`dockerfile = ${tomlString((f.dockerfile || '').trim() || 'Dockerfile')}`);

  appendServiceTables(lines, f.services);

  return lines.join('\n') + '\n';
}

// buildBuilderToml renders the "Builder" form (an image app, not a git build)
// into an lwd.toml document: top-level app fields (including `image`), then
// any declared [[services]].
function buildBuilderToml(f) {
  const lines = [];
  lines.push(`name = ${tomlString((f.name || '').trim())}`);
  lines.push(`image = ${tomlString((f.image || '').trim())}`);
  lines.push(`domain = ${tomlString((f.domain || '').trim())}`);
  if (String(f.port || '').trim()) lines.push(`port = ${parseInt(f.port, 10)}`);
  if (f.node && f.node !== 'local') lines.push(`node = ${tomlString(f.node)}`);
  const env = envRowsToInline(f.env);
  if (env) lines.push(`env = ${env}`);
  const secrets = namesToArray(f.secrets);
  if (secrets) lines.push(`secrets = ${secrets}`);

  appendServiceTables(lines, f.services);

  return lines.join('\n') + '\n';
}

// durationString converts a Go time.Duration (nanoseconds, as it appears in
// the JSON snapshot) into a compact Go-style duration string ("30s"), used
// as a fallback when a stored spec has no RawTimeout (e.g. it was defaulted
// rather than set explicitly in the source lwd.toml).
function durationString(ns) {
  if (!ns) return '';
  const seconds = ns / 1e9;
  if (seconds >= 1 && Number.isInteger(seconds)) return seconds + 's';
  return seconds + 's';
}

function dashboard() {
  return {
    // ---- chrome / theme ------------------------------------------------
    theme: localStorage.getItem('lwd-theme') || '',

    // ---- routing ---------------------------------------------------------
    view: 'overview', // 'overview' | 'detail' | 'nodes' | 'health'

    // ---- overview ----------------------------------------------------
    apps: [],
    appsLoading: true,
    daemonDown: false,
    loadError: '',
    _pollHandle: null,

    // ---- detail --------------------------------------------------------
    selected: null,
    detail: null,
    detailLoading: false,
    detailError: '',
    activeTab: 'logs',

    // ---- logs ------------------------------------------------------------
    logLines: [],
    logFollow: true,
    logConnected: false,
    _es: null,

    // ---- secrets -----------------------------------------------------
    secretNames: [],
    secretsLoading: false,
    secretsError: '',
    newSecretName: '',
    newSecretValue: '',
    secretBusy: false,
    secretDeleteBusy: '',

    // ---- deploy modal --------------------------------------------------
    // deploy.tab selects the active source in 'create' mode: 'git' (From
    // Git), 'builder' (image app), or 'paste' (raw toml, the original flow).
    // 'edit' mode (from Config -> "Edit & apply") always uses the paste
    // textarea, pre-filled with the rendered current spec.
    deploy: {
      open: false,
      mode: 'create', // 'create' | 'edit'
      tab: 'git', // 'git' | 'builder' | 'paste'
      toml: '',
      error: '',
      busy: false,
      git: null,
      builder: null,
    },

    // ---- nodes -----------------------------------------------------------
    nodes: [],
    nodesLoading: false,
    nodesError: '',
    newNode: { name: '', sshHost: '', meshAddr: '', agentUrl: '' },
    nodeAddBusy: false,
    nodeRemoveBusy: '',

    // ---- health ------------------------------------------------------
    health: null,
    healthLoading: false,
    healthError: '',
    _healthPollHandle: null,

    // ---- danger zone ---------------------------------------------------
    deleteConfirm: false,
    deleteConfirmText: '',
    deleteBusy: false,
    rollbackBusyId: null,
    redeployBusy: false,

    // ---- toasts ------------------------------------------------------
    toasts: [],
    _toastSeq: 0,

    // ====================================================================
    // lifecycle
    // ====================================================================
    init() {
      this.applyTheme();
      this.loadApps();
      this.loadNodes(); // populates the deploy modal's node <select> even before visiting the Nodes view
      this._pollHandle = setInterval(() => this.loadApps({ silent: true }), 5000);
      window.addEventListener('beforeunload', () => { this.stopLogs(); this.stopHealthPoll(); });
    },

    // ====================================================================
    // theme
    // ====================================================================
    applyTheme() {
      if (this.theme) {
        document.documentElement.setAttribute('data-theme', this.theme);
      } else {
        document.documentElement.removeAttribute('data-theme');
      }
    },
    toggleTheme() {
      const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
      const current = this.theme || (prefersDark ? 'dark' : 'light');
      this.theme = current === 'dark' ? 'light' : 'dark';
      localStorage.setItem('lwd-theme', this.theme);
      this.applyTheme();
    },

    // ====================================================================
    // overview
    // ====================================================================
    async loadApps({ silent } = {}) {
      if (!silent) this.appsLoading = true;
      try {
        const apps = await apiFetch('/api/apps');
        this.apps = apps || [];
        this.daemonDown = false;
        this.loadError = '';
      } catch (e) {
        if (e.status === 502) {
          this.daemonDown = true;
        } else if (!silent) {
          this.loadError = e.message || 'Failed to load apps.';
        }
      } finally {
        this.appsLoading = false;
      }

      // Keep an open detail view in sync with the overview poll (status
      // pill, image tag) without re-fetching history/secrets every tick.
      if (this.view === 'detail' && this.selected && this.detail) {
        const fresh = this.apps.find((a) => a.name === this.selected);
        if (fresh) this.detail.status = fresh;
      }
    },

    statusKind,
    healthKind,
    shortImage,
    timeAgo,
    fullTime,

    // ====================================================================
    // navigation
    // ====================================================================
    async openApp(name) {
      this.stopHealthPoll();
      this.selected = name;
      this.view = 'detail';
      this.activeTab = 'logs';
      this.detail = null;
      this.detailError = '';
      this.logLines = [];
      await this.loadDetail();
      this.startLogs();
    },

    backToOverview() {
      this.stopLogs();
      this.stopHealthPoll();
      this.view = 'overview';
      this.selected = null;
      this.detail = null;
      this.deleteConfirm = false;
      this.deleteConfirmText = '';
      this.loadApps({ silent: true });
    },

    async openNodes() {
      this.stopLogs();
      this.stopHealthPoll();
      this.view = 'nodes';
      await this.loadNodes();
    },

    async setTab(tab) {
      const leavingLogs = this.activeTab === 'logs' && tab !== 'logs';
      this.activeTab = tab;
      if (leavingLogs) this.stopLogs();
      if (tab === 'logs' && !this.logConnected) this.startLogs();
      if (tab === 'secrets' && this.secretNames.length === 0) this.loadSecrets();
    },

    async loadDetail() {
      this.detailLoading = true;
      this.detailError = '';
      try {
        const d = await apiFetch(`/api/apps/${encodeURIComponent(this.selected)}`);
        this.detail = d;
      } catch (e) {
        this.detailError = e.status === 502
          ? 'Cannot reach the lwd daemon.'
          : (e.message || 'Failed to load app.');
      } finally {
        this.detailLoading = false;
      }
    },

    get currentSpecJson() {
      if (!this.detail || !this.detail.history || !this.detail.history.length) return null;
      return this.detail.history[0].Spec;
    },

    get currentSpecToml() {
      const json = this.currentSpecJson;
      return json ? specToToml(json) : '';
    },

    // ====================================================================
    // logs (SSE)
    // ====================================================================
    startLogs() {
      if (!this.selected || this._es) return;
      const es = new EventSource(`/api/apps/${encodeURIComponent(this.selected)}/logs`);
      this._es = es;
      es.onopen = () => { this.logConnected = true; };
      es.onmessage = (evt) => {
        this.logLines.push(evt.data);
        if (this.logLines.length > 4000) this.logLines.splice(0, this.logLines.length - 4000);
        if (this.logFollow) this.$nextTick(() => this.scrollLogsToEnd());
      };
      es.onerror = () => {
        this.logConnected = false;
        // EventSource retries on its own; if the tab/app changed meanwhile
        // stopLogs() will have already closed and nulled this instance.
      };
    },

    stopLogs() {
      if (this._es) {
        this._es.close();
        this._es = null;
      }
      this.logConnected = false;
    },

    scrollLogsToEnd() {
      const el = this.$refs.logPane;
      if (el) el.scrollTop = el.scrollHeight;
    },

    toggleFollow() {
      this.logFollow = !this.logFollow;
      if (this.logFollow) this.$nextTick(() => this.scrollLogsToEnd());
    },

    // ====================================================================
    // deployments (history / rollback / redeploy)
    // ====================================================================
    async rollback(dep) {
      if (!confirm(`Roll back ${this.selected} to ${shortImage(dep.Image)}?`)) return;
      this.rollbackBusyId = dep.ID;
      try {
        await apiFetch(`/api/apps/${encodeURIComponent(this.selected)}/rollback`, { method: 'POST' });
        this.notify('ok', `Rolled back ${this.selected}.`);
        await this.loadDetail();
        await this.loadApps({ silent: true });
      } catch (e) {
        this.notify('err', e.message || 'Rollback failed.');
      } finally {
        this.rollbackBusyId = null;
      }
    },

    async redeploy() {
      this.redeployBusy = true;
      try {
        await apiFetch(`/api/apps/${encodeURIComponent(this.selected)}/redeploy`, { method: 'POST' });
        this.notify('ok', `Redeployed ${this.selected}.`);
        await this.loadDetail();
        await this.loadApps({ silent: true });
      } catch (e) {
        this.notify('err', e.message || 'Redeploy failed.');
      } finally {
        this.redeployBusy = false;
      }
    },

    // ====================================================================
    // secrets
    // ====================================================================
    async loadSecrets() {
      this.secretsLoading = true;
      this.secretsError = '';
      try {
        this.secretNames = await apiFetch(`/api/apps/${encodeURIComponent(this.selected)}/secrets`) || [];
      } catch (e) {
        this.secretsError = e.message || 'Failed to load secrets.';
      } finally {
        this.secretsLoading = false;
      }
    },

    async addSecret() {
      const key = this.newSecretName.trim();
      if (!key) return;
      this.secretBusy = true;
      this.secretsError = '';
      try {
        await apiFetch(`/api/apps/${encodeURIComponent(this.selected)}/secrets`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ key, value: this.newSecretValue }),
        });
        this.newSecretName = '';
        this.newSecretValue = '';
        this.notify('ok', `Secret "${key}" saved.`);
        await this.loadSecrets();
      } catch (e) {
        this.secretsError = e.message || 'Failed to save secret.';
      } finally {
        this.secretBusy = false;
      }
    },

    async deleteSecret(key) {
      if (!confirm(`Delete secret "${key}"? This cannot be undone.`)) return;
      this.secretDeleteBusy = key;
      try {
        await apiFetch(`/api/apps/${encodeURIComponent(this.selected)}/secrets/${encodeURIComponent(key)}`, {
          method: 'DELETE',
        });
        this.notify('ok', `Secret "${key}" deleted.`);
        await this.loadSecrets();
      } catch (e) {
        this.notify('err', e.message || 'Failed to delete secret.');
      } finally {
        this.secretDeleteBusy = '';
      }
    },

    // ====================================================================
    // nodes
    // ====================================================================
    async loadNodes() {
      this.nodesLoading = true;
      this.nodesError = '';
      try {
        this.nodes = await apiFetch('/api/nodes') || [];
      } catch (e) {
        this.nodesError = e.message || 'Failed to load nodes.';
      } finally {
        this.nodesLoading = false;
      }
    },

    newNodeForm() {
      return { name: '', sshHost: '', meshAddr: '', agentUrl: '' };
    },

    async addNode() {
      const name = this.newNode.name.trim();
      const sshHost = this.newNode.sshHost.trim();
      const meshAddr = this.newNode.meshAddr.trim();
      if (!name || !sshHost || !meshAddr) return;
      this.nodeAddBusy = true;
      this.nodesError = '';
      try {
        await apiFetch('/api/nodes', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, ssh_host: sshHost, mesh_addr: meshAddr, agent_url: this.newNode.agentUrl.trim() }),
        });
        this.notify('ok', `Node "${name}" added.`);
        this.newNode = this.newNodeForm();
        await this.loadNodes();
      } catch (e) {
        this.nodesError = e.message || 'Failed to add node.';
      } finally {
        this.nodeAddBusy = false;
      }
    },

    async removeNode(name) {
      if (!confirm(`Remove node "${name}"? Apps already placed on it are not moved or removed.`)) return;
      this.nodeRemoveBusy = name;
      try {
        await apiFetch(`/api/nodes/${encodeURIComponent(name)}`, { method: 'DELETE' });
        this.notify('ok', `Node "${name}" removed.`);
        await this.loadNodes();
      } catch (e) {
        this.notify('err', e.message || 'Failed to remove node.');
      } finally {
        this.nodeRemoveBusy = '';
      }
    },

    // ====================================================================
    // health (Phase 10 continuous reconciler snapshot)
    // ====================================================================
    async openHealth() {
      this.stopLogs();
      this.view = 'health';
      await this.loadHealth();
      // The reconciler's own loop runs on LWD_RECONCILE_INTERVAL (15s by
      // default); polling a little faster than that keeps the panel feeling
      // live without hammering the daemon between passes.
      this.stopHealthPoll();
      this._healthPollHandle = setInterval(() => this.loadHealth({ silent: true }), 5000);
    },

    stopHealthPoll() {
      if (this._healthPollHandle) {
        clearInterval(this._healthPollHandle);
        this._healthPollHandle = null;
      }
    },

    async loadHealth({ silent } = {}) {
      if (!silent) {
        this.healthLoading = true;
        this.healthError = '';
      }
      try {
        this.health = await apiFetch('/api/health');
        this.healthError = '';
      } catch (e) {
        if (!silent) {
          this.healthError = e.status === 502
            ? 'Cannot reach the lwd daemon.'
            : (e.message || 'Failed to load health.');
        }
      } finally {
        this.healthLoading = false;
      }
    },

    // ====================================================================
    // danger zone
    // ====================================================================
    async deleteApp() {
      this.deleteBusy = true;
      try {
        await apiFetch(`/api/apps/${encodeURIComponent(this.selected)}`, { method: 'DELETE' });
        this.notify('ok', `${this.selected} deleted.`);
        this.backToOverview();
      } catch (e) {
        this.notify('err', e.message || 'Delete failed.');
      } finally {
        this.deleteBusy = false;
        this.deleteConfirm = false;
        this.deleteConfirmText = '';
      }
    },

    // ====================================================================
    // deploy modal (create / edit-and-apply)
    // ====================================================================
    newEnvRow() {
      return { key: '', value: '' };
    },

    newServiceRow() {
      return { name: '', image: '', command: '', volume: '', env: [], secrets: [] };
    },

    newGitForm() {
      return {
        url: '', ref: 'main', subdir: '', dockerfile: 'Dockerfile',
        name: '', domain: '', port: '', node: 'local',
        env: [], secrets: [], services: [],
      };
    },

    newBuilderForm() {
      return {
        image: '',
        name: '', domain: '', port: '', node: 'local',
        env: [], secrets: [], services: [],
      };
    },

    openDeployCreate() {
      this.deploy = {
        open: true, mode: 'create', tab: 'git', toml: '', error: '', busy: false,
        git: this.newGitForm(), builder: this.newBuilderForm(),
      };
      this.loadNodes(); // refresh the node <select> options in case one was just registered
    },

    openDeployEdit() {
      this.deploy = {
        open: true, mode: 'edit', tab: 'paste', toml: this.currentSpecToml, error: '', busy: false,
        git: this.newGitForm(), builder: this.newBuilderForm(),
      };
    },

    closeDeploy() {
      this.deploy.open = false;
    },

    // gitPreviewToml / builderPreviewToml are the live "transparency" preview
    // shown next to the From-Git / Builder forms, and also exactly what gets
    // POSTed to /api/apply on submit — the preview is never a lie.
    get gitPreviewToml() {
      return buildGitToml(this.deploy.git);
    },

    get builderPreviewToml() {
      return buildBuilderToml(this.deploy.builder);
    },

    get gitFormValid() {
      const f = this.deploy.git;
      return !!(f && f.url.trim() && f.name.trim() && f.domain.trim() && String(f.port).trim());
    },

    get builderFormValid() {
      const f = this.deploy.builder;
      return !!(f && f.image.trim() && f.name.trim() && f.domain.trim() && String(f.port).trim());
    },

    // deploySubmitDisabled gates the Apply button client-side (name/domain/
    // port required for From-Git and Builder; non-empty body for Paste and
    // Edit). The daemon validates fully regardless — this is just to avoid a
    // pointless round trip for an obviously-incomplete form.
    get deploySubmitDisabled() {
      if (this.deploy.busy) return true;
      if (this.deploy.mode === 'edit' || this.deploy.tab === 'paste') return !this.deploy.toml.trim();
      if (this.deploy.tab === 'git') return !this.gitFormValid;
      if (this.deploy.tab === 'builder') return !this.builderFormValid;
      return true;
    },

    async submitDeploy() {
      this.deploy.busy = true;
      this.deploy.error = '';
      const toml = (this.deploy.mode === 'edit' || this.deploy.tab === 'paste')
        ? this.deploy.toml
        : (this.deploy.tab === 'git' ? this.gitPreviewToml : this.builderPreviewToml);
      try {
        const dep = await apiFetch('/api/apply', {
          method: 'POST',
          headers: { 'Content-Type': 'text/plain' },
          body: toml,
        });
        this.notify('ok', `Applied ${dep && dep.App ? dep.App : 'app'}.`);
        this.deploy.open = false;
        await this.loadApps({ silent: true });
        if (this.view === 'detail' && dep && dep.App === this.selected) {
          await this.loadDetail();
        }
      } catch (e) {
        this.deploy.error = e.status === 502
          ? 'Cannot reach the lwd daemon.'
          : (e.message || 'Apply failed.');
      } finally {
        this.deploy.busy = false;
      }
    },

    // ====================================================================
    // toasts
    // ====================================================================
    notify(kind, message) {
      const id = ++this._toastSeq;
      this.toasts.push({ id, kind, message });
      setTimeout(() => {
        this.toasts = this.toasts.filter((t) => t.id !== id);
      }, 4500);
    },
  };
}

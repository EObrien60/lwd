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
    view: 'overview', // 'overview' | 'detail'

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
    deploy: {
      open: false,
      mode: 'create', // 'create' | 'edit'
      toml: '',
      error: '',
      busy: false,
    },

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
      this._pollHandle = setInterval(() => this.loadApps({ silent: true }), 5000);
      window.addEventListener('beforeunload', () => this.stopLogs());
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
    shortImage,
    timeAgo,
    fullTime,

    // ====================================================================
    // navigation
    // ====================================================================
    async openApp(name) {
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
      this.view = 'overview';
      this.selected = null;
      this.detail = null;
      this.deleteConfirm = false;
      this.deleteConfirmText = '';
      this.loadApps({ silent: true });
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
    openDeployCreate() {
      this.deploy = { open: true, mode: 'create', toml: '', error: '', busy: false };
    },

    openDeployEdit() {
      this.deploy = { open: true, mode: 'edit', toml: this.currentSpecToml, error: '', busy: false };
    },

    closeDeploy() {
      this.deploy.open = false;
    },

    async submitDeploy() {
      this.deploy.busy = true;
      this.deploy.error = '';
      try {
        const dep = await apiFetch('/api/apply', {
          method: 'POST',
          headers: { 'Content-Type': 'text/plain' },
          body: this.deploy.toml,
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

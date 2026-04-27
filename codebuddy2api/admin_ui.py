from __future__ import annotations


ADMIN_HTML = r"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>CodeBuddy2API Admin</title>
  <style>
    :root {
      color-scheme: light dark;
      --bg: #0f172a;
      --panel: #111827;
      --panel-2: #1f2937;
      --text: #e5e7eb;
      --muted: #9ca3af;
      --line: #374151;
      --ok: #22c55e;
      --bad: #ef4444;
      --warn: #f59e0b;
      --accent: #38bdf8;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: var(--bg);
      color: var(--text);
    }
    header {
      padding: 22px 24px;
      border-bottom: 1px solid var(--line);
      background: linear-gradient(135deg, #111827 0%, #0f172a 55%, #082f49 100%);
    }
    h1 { margin: 0; font-size: 22px; }
    h2 { margin: 0 0 12px; font-size: 16px; }
    main { padding: 20px 24px 40px; max-width: 1500px; margin: 0 auto; }
    section {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 12px;
      padding: 16px;
      margin-bottom: 16px;
      box-shadow: 0 12px 28px rgba(0,0,0,.18);
    }
    .grid { display: grid; gap: 12px; }
    .grid.stats { grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); }
    .grid.form { grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); align-items: end; }
    .card {
      background: var(--panel-2);
      border: 1px solid var(--line);
      border-radius: 10px;
      padding: 12px;
    }
    .label { color: var(--muted); font-size: 12px; text-transform: uppercase; letter-spacing: .04em; }
    .value { font-size: 20px; font-weight: 700; margin-top: 3px; }
    input, textarea, select, button {
      width: 100%;
      border-radius: 8px;
      border: 1px solid var(--line);
      background: #0b1220;
      color: var(--text);
      padding: 9px 10px;
      font: inherit;
    }
    textarea { min-height: 68px; resize: vertical; }
    button {
      cursor: pointer;
      background: #0ea5e9;
      border-color: #0284c7;
      color: white;
      font-weight: 700;
    }
    button.secondary { background: #334155; border-color: #475569; }
    button.ok { background: #16a34a; border-color: #15803d; }
    button.bad { background: #dc2626; border-color: #b91c1c; }
    button.warn { background: #d97706; border-color: #b45309; }
    button:disabled { opacity: .55; cursor: not-allowed; }
    .actions { display: flex; flex-wrap: wrap; gap: 8px; }
    .actions button { width: auto; min-width: 90px; }
    .muted { color: var(--muted); }
    .status {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      border-radius: 999px;
      padding: 2px 8px;
      background: #0b1220;
      border: 1px solid var(--line);
      font-size: 12px;
      white-space: nowrap;
    }
    .dot { width: 8px; height: 8px; border-radius: 999px; background: var(--muted); }
    .active .dot { background: var(--ok); }
    .disabled .dot { background: var(--bad); }
    .cooldown .dot { background: var(--warn); }
    .table-wrap { overflow: auto; border: 1px solid var(--line); border-radius: 10px; }
    table { width: 100%; border-collapse: collapse; min-width: 1180px; }
    th, td { padding: 9px 10px; border-bottom: 1px solid var(--line); vertical-align: top; }
    th { position: sticky; top: 0; background: #172033; text-align: left; z-index: 1; }
    tr:last-child td { border-bottom: 0; }
    td input { min-width: 80px; }
    td textarea { min-width: 170px; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; }
    .toast {
      position: fixed;
      right: 18px;
      bottom: 18px;
      max-width: 560px;
      padding: 12px 14px;
      border-radius: 10px;
      border: 1px solid var(--line);
      background: #020617;
      box-shadow: 0 12px 28px rgba(0,0,0,.35);
      display: none;
      white-space: pre-wrap;
      z-index: 10;
    }
    .toast.show { display: block; }
    .split { display: flex; gap: 10px; align-items: end; flex-wrap: wrap; }
    .split > label { flex: 1; min-width: 260px; }
    .small { font-size: 12px; }
    .nowrap { white-space: nowrap; }
  </style>
</head>
<body>
  <header>
    <h1>CodeBuddy2API Admin</h1>
    <div class="muted">Manage account pool, health, cooldown, and upstream usage. Full keys are never rendered.</div>
  </header>
  <main>
    <section>
      <h2>Admin key</h2>
      <div class="split">
        <label>
          <div class="label">Authorization Bearer</div>
          <input id="adminKey" type="password" autocomplete="off" placeholder="CODEBUDDY2API_ADMIN_KEY" />
        </label>
        <button id="saveKey" class="secondary" type="button">Save locally</button>
        <button id="refresh" type="button">Refresh</button>
      </div>
      <div class="muted small">Stored only in this browser localStorage and sent to Admin API as Authorization: Bearer.</div>
    </section>

    <section>
      <h2>Stats</h2>
      <div id="stats" class="grid stats"></div>
    </section>

    <section>
      <h2>Add account</h2>
      <form id="addForm" class="grid form">
        <label><div class="label">Name</div><input name="name" value="CodeBuddy account" required /></label>
        <label><div class="label">API key</div><input name="api_key" type="password" placeholder="ck_..." required /></label>
        <label><div class="label">Enabled</div><select name="enabled"><option value="true">true</option><option value="false">false</option></select></label>
        <label><div class="label">Priority</div><input name="priority" type="number" value="100" /></label>
        <label><div class="label">Weight</div><input name="weight" type="number" min="1" max="100" value="1" /></label>
        <label><div class="label">Concurrency</div><input name="concurrency" type="number" min="1" max="100" value="1" /></label>
        <label><div class="label">Proxy URL</div><input name="proxy_url" placeholder="http://... or socks5://..." /></label>
        <label><div class="label">Notes</div><input name="notes" placeholder="optional" /></label>
        <label style="grid-column: 1 / -1;"><div class="label">Header profile JSON</div><textarea name="header_profile" placeholder='{"user_agent":"CLI/1.0.8 CodeBuddy/1.0.8"}'></textarea></label>
        <button type="submit">Add account</button>
      </form>
    </section>

    <section>
      <h2>Accounts</h2>
      <div id="accounts"></div>
    </section>
  </main>
  <div id="toast" class="toast"></div>

  <script>
    const keyInput = document.getElementById("adminKey");
    const toast = document.getElementById("toast");
    keyInput.value = localStorage.getItem("codebuddy2api.adminKey") || "";

    document.getElementById("saveKey").addEventListener("click", () => {
      localStorage.setItem("codebuddy2api.adminKey", keyInput.value.trim());
      show("Admin key saved locally.");
    });
    document.getElementById("refresh").addEventListener("click", refreshAll);
    document.getElementById("addForm").addEventListener("submit", addAccount);

    function authHeaders() {
      const token = keyInput.value.trim();
      return token ? { "Authorization": `Bearer ${token}` } : {};
    }

    async function api(path, options = {}) {
      const headers = {
        "Content-Type": "application/json",
        ...authHeaders(),
        ...(options.headers || {}),
      };
      const res = await fetch(path, { ...options, headers });
      const text = await res.text();
      let body = null;
      try { body = text ? JSON.parse(text) : null; } catch (_) { body = text; }
      if (!res.ok) {
        const detail = body && body.detail ? body.detail : body && body.error ? body.error.message : text;
        throw new Error(`${res.status} ${res.statusText}: ${detail || "request failed"}`);
      }
      return body;
    }

    function parseOptionalJson(raw, fallback) {
      const value = (raw || "").trim();
      if (!value) return fallback;
      return JSON.parse(value);
    }

    function show(message, isError = false) {
      toast.textContent = message;
      toast.style.borderColor = isError ? "#ef4444" : "#22c55e";
      toast.classList.add("show");
      clearTimeout(window.__toastTimer);
      window.__toastTimer = setTimeout(() => toast.classList.remove("show"), 4500);
    }

    function fmt(value) {
      if (value === null || value === undefined || value === "") return "-";
      if (typeof value === "number") return Number.isInteger(value) ? String(value) : value.toFixed(6).replace(/0+$/, "").replace(/\.$/, "");
      return String(value);
    }

    function time(ts) {
      if (!ts) return "-";
      return new Date(ts * 1000).toLocaleString();
    }

    function escapeHtml(value) {
      return String(value ?? "").replace(/[&<>"']/g, ch => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
      }[ch]));
    }

    async function refreshAll() {
      try {
        localStorage.setItem("codebuddy2api.adminKey", keyInput.value.trim());
        const [stats, accounts] = await Promise.all([
          api("/admin/stats"),
          api("/admin/accounts"),
        ]);
        renderStats(stats);
        renderAccounts(accounts.accounts || []);
      } catch (err) {
        show(err.message, true);
      }
    }

    function renderStats(stats) {
      const items = [
        ["accounts", stats.accounts],
        ["enabled", stats.enabled_accounts],
        ["requests", stats.total_requests],
        ["success", stats.total_success],
        ["failures", stats.total_failures],
        ["credit", stats.total_credit],
        ["prompt tokens", stats.prompt_tokens],
        ["completion tokens", stats.completion_tokens],
        ["total tokens", stats.total_tokens],
      ];
      document.getElementById("stats").innerHTML = items.map(([label, value]) => `
        <div class="card"><div class="label">${escapeHtml(label)}</div><div class="value">${escapeHtml(fmt(value))}</div></div>
      `).join("");
    }

    function renderAccounts(accounts) {
      if (!accounts.length) {
        document.getElementById("accounts").innerHTML = '<div class="muted">No accounts yet.</div>';
        return;
      }
      document.getElementById("accounts").innerHTML = `
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>ID</th><th>Status</th><th>Name</th><th>Key</th><th>Priority</th><th>Weight</th><th>Concurrency</th><th>In-flight</th>
                <th>Usage</th><th>Failures</th><th>Proxy</th><th>Header profile</th><th>Notes</th><th>Last error</th><th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${accounts.map(accountRow).join("")}
            </tbody>
          </table>
        </div>`;
    }

    function accountRow(a) {
      const status = a.enabled ? a.status : "disabled";
      const profile = JSON.stringify(a.header_profile || {}, null, 0);
      return `
        <tr data-id="${a.id}">
          <td class="mono nowrap">${a.id}</td>
          <td><span class="status ${escapeHtml(status)}"><span class="dot"></span>${escapeHtml(status)}</span><div class="muted small">cooldown: ${escapeHtml(time(a.cooldown_until))}</div></td>
          <td><input data-field="name" value="${escapeHtml(a.name)}" /></td>
          <td><div class="mono nowrap">${escapeHtml(a.api_key_preview)}</div><input data-field="api_key" type="password" placeholder="new key only" /></td>
          <td><input data-field="priority" type="number" value="${escapeHtml(a.priority)}" /></td>
          <td><input data-field="weight" type="number" min="1" max="100" value="${escapeHtml(a.weight)}" /></td>
          <td><input data-field="concurrency" type="number" min="1" max="100" value="${escapeHtml(a.concurrency)}" /></td>
          <td class="mono">${escapeHtml(fmt(a.in_flight))}</td>
          <td class="small">
            credit: <span class="mono">${escapeHtml(fmt(a.total_credit))}</span><br>
            req/s/f: <span class="mono">${escapeHtml(fmt(a.total_requests))}/${escapeHtml(fmt(a.total_success))}/${escapeHtml(fmt(a.total_failures))}</span><br>
            tokens: <span class="mono">${escapeHtml(fmt(a.total_tokens))}</span><br>
            ok: ${escapeHtml(time(a.last_success_at))}<br>
            fail: ${escapeHtml(time(a.last_failure_at))}
          </td>
          <td><input data-field="proxy_url" value="${escapeHtml(a.proxy_url || "")}" /></td>
          <td><textarea data-field="header_profile">${escapeHtml(profile)}</textarea></td>
          <td><textarea data-field="notes">${escapeHtml(a.notes || "")}</textarea></td>
          <td class="small">${escapeHtml(a.last_error || "-")}</td>
          <td>
            <div class="actions">
              <button type="button" onclick="saveAccount(${a.id})">Save</button>
              <button type="button" class="${a.enabled ? "bad" : "ok"}" onclick="setEnabled(${a.id}, ${a.enabled ? "false" : "true"})">${a.enabled ? "Disable" : "Enable"}</button>
              <button type="button" class="secondary" onclick="probe(${a.id})">Probe</button>
              <button type="button" class="warn" onclick="resetFailures(${a.id})">Reset</button>
            </div>
          </td>
        </tr>`;
    }

    async function addAccount(event) {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      try {
        const payload = {
          name: form.get("name"),
          api_key: form.get("api_key"),
          enabled: form.get("enabled") === "true",
          priority: Number(form.get("priority") || 100),
          weight: Number(form.get("weight") || 1),
          concurrency: Number(form.get("concurrency") || 1),
          proxy_url: form.get("proxy_url") ? String(form.get("proxy_url")) : null,
          notes: form.get("notes") ? String(form.get("notes")) : null,
          header_profile: parseOptionalJson(form.get("header_profile"), {}),
        };
        await api("/admin/accounts", { method: "POST", body: JSON.stringify(payload) });
        event.currentTarget.reset();
        show("Account added.");
        await refreshAll();
      } catch (err) {
        show(err.message, true);
      }
    }

    async function saveAccount(id) {
      const row = document.querySelector(`tr[data-id="${id}"]`);
      const payload = {};
      for (const field of ["name", "priority", "weight", "concurrency", "proxy_url", "notes", "api_key"]) {
        const input = row.querySelector(`[data-field="${field}"]`);
        let value = input.value;
        if (field === "api_key" && !value.trim()) continue;
        if (["priority", "weight", "concurrency"].includes(field)) value = Number(value);
        if (["proxy_url", "notes"].includes(field)) value = value.trim() ? value : null;
        payload[field] = value;
      }
      try {
        payload.header_profile = parseOptionalJson(row.querySelector('[data-field="header_profile"]').value, {});
        await api(`/admin/accounts/${id}`, { method: "PATCH", body: JSON.stringify(payload) });
        show(`Account ${id} saved.`);
        await refreshAll();
      } catch (err) {
        show(err.message, true);
      }
    }

    async function setEnabled(id, enabled) {
      try {
        await api(`/admin/accounts/${id}/${enabled ? "enable" : "disable"}`, { method: "POST", body: "{}" });
        show(`Account ${id} ${enabled ? "enabled" : "disabled"}.`);
        await refreshAll();
      } catch (err) {
        show(err.message, true);
      }
    }

    async function probe(id) {
      try {
        const result = await api(`/admin/accounts/${id}/probe`, { method: "POST", body: "{}" });
        show(`Probe ${id} OK:\n${JSON.stringify(result, null, 2)}`);
        await refreshAll();
      } catch (err) {
        show(err.message, true);
      }
    }

    async function resetFailures(id) {
      try {
        await api(`/admin/accounts/${id}`, { method: "PATCH", body: JSON.stringify({ reset_failures: true }) });
        show(`Account ${id} failures reset.`);
        await refreshAll();
      } catch (err) {
        show(err.message, true);
      }
    }

    refreshAll();
  </script>
</body>
</html>
"""

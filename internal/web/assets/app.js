"use strict";
(() => {
  const TOKEN_KEY = "omni_notify_token";
  // localStorage can be unavailable (private mode, sandboxed iframes); degrade
  // gracefully — the token just won't persist across reloads.
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch {} },
    del(k) { try { localStorage.removeItem(k); } catch {} },
  };
  let token = store.get(TOKEN_KEY) || "";
  const autoTimers = {};

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  function el(tag, text, cls) {
    const e = document.createElement(tag);
    if (text != null) e.textContent = text;
    if (cls) e.className = cls;
    return e;
  }

  function toast(msg, type = "") {
    const t = $("#toast");
    t.textContent = msg;
    t.className = "toast " + type;
    t.hidden = false;
    clearTimeout(toast._t);
    toast._t = setTimeout(() => { t.hidden = true; }, 3800);
  }

  // guard wraps an async fn so any error surfaces as a toast instead of throwing.
  function guard(fn) {
    return async (...args) => {
      try { return await fn(...args); }
      catch (e) { toast(e.message || "request failed", "err"); }
    };
  }

  async function api(method, path, body) {
    if (!token) {
      throw new Error("Enter your bearer token in the top bar, then click Save.");
    }
    const headers = {};
    if (token) headers["Authorization"] = "Bearer " + token;
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await fetch(path, {
      method, headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    let data = null;
    if (text) { try { data = JSON.parse(text); } catch { data = text; } }
    if (!res.ok) {
      const msg = data && data.error ? data.error : "HTTP " + res.status;
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    return data;
  }

  // ---- rendering helpers (all text goes through textContent: no XSS) ----
  function badge(text, cls) { return el("span", text, "badge " + cls); }
  function sevBadge(s, td) { if (s) td.append(badge(s, "sev-" + s)); }
  function statusBadge(s, td) { if (s) td.append(badge(s, "st-" + s)); }
  function dlBadge(s, td) { if (s) td.append(badge(s, "dl-" + s)); }
  function fmtTime(s) { if (!s) return ""; const d = new Date(s); return isNaN(d.getTime()) ? s : d.toLocaleString(); }
  function short(s) { return s ? String(s).slice(0, 12) : ""; }

  function renderTable(tableEl, columns, rows, opts = {}) {
    tableEl.textContent = "";
    const head = el("tr");
    columns.forEach((c) => head.append(el("th", c.label)));
    tableEl.append(head);
    if (!rows || rows.length === 0) {
      const tr = el("tr"), td = el("td", "No results", "empty");
      td.colSpan = columns.length;
      tr.append(td); tableEl.append(tr);
      return;
    }
    rows.forEach((r) => {
      const tr = el("tr");
      if (opts.onRow) {
        tr.className = "clickable";
        tr.addEventListener("click", (e) => { if (!e.target.closest("button")) opts.onRow(r); });
      }
      columns.forEach((c) => {
        const td = el("td");
        if (c.wrap) td.className = "wrap";
        if (c.render) {
          const v = c.render(r, td);
          if (v != null && td.childNodes.length === 0) td.textContent = String(v);
        } else {
          const v = r[c.key];
          td.textContent = v == null ? "" : String(v);
        }
        tr.append(td);
      });
      tableEl.append(tr);
    });
  }

  function showModal(obj) {
    $("#modal-body").textContent = JSON.stringify(obj, null, 2);
    $("#modal").hidden = false;
  }

  function qs(obj) {
    const p = new URLSearchParams();
    Object.entries(obj).forEach(([k, v]) => { if (v !== "" && v != null) p.set(k, v); });
    const s = p.toString();
    return s ? "?" + s : "";
  }

  // ---------------- loaders ----------------
  const loadEvents = guard(async () => {
    const f = {};
    $$("#tab-events [data-filter]").forEach((i) => { if (i.value) f[i.dataset.filter] = i.value; });
    const data = await api("GET", "/api/v1/events" + qs(f));
    renderTable($("#events-table"), [
      { label: "id", key: "id" },
      { label: "time", render: (r) => fmtTime(r.timestamp) },
      { label: "source", key: "source" },
      { label: "type", key: "type" },
      { label: "status", render: (r, td) => statusBadge(r.status, td) },
      { label: "severity", render: (r, td) => sevBadge(r.severity, td) },
      { label: "title", key: "title", wrap: true },
      { label: "fingerprint", render: (r) => short(r.fingerprint) },
    ], data.events, { onRow: showModal });
  });

  const loadStates = guard(async () => {
    const active = $("#states-active").checked;
    const data = await api("GET", "/api/v1/states" + (active ? "?active=true" : ""));
    renderTable($("#states-table"), [
      { label: "active", render: (r, td) => td.append(badge(r.active ? "active" : "inactive", r.active ? "st-firing" : "sev-debug")) },
      { label: "status", render: (r, td) => statusBadge(r.status, td) },
      { label: "severity", render: (r, td) => sevBadge(r.severity, td) },
      { label: "title", key: "title", wrap: true },
      { label: "source", key: "source" },
      { label: "first seen", render: (r) => fmtTime(r.first_seen) },
      { label: "last seen", render: (r) => fmtTime(r.last_seen) },
      { label: "fingerprint", render: (r) => short(r.fingerprint) },
    ], data.states, { onRow: showModal });
  });

  const loadDeliveries = guard(async () => {
    const f = {};
    $$("#tab-deliveries [data-dfilter]").forEach((i) => { if (i.value) f[i.dataset.dfilter] = i.value; });
    const data = await api("GET", "/api/v1/deliveries" + qs(f));
    renderTable($("#deliveries-table"), [
      { label: "id", key: "id" },
      { label: "status", render: (r, td) => dlBadge(r.status, td) },
      { label: "provider", key: "provider" },
      { label: "route", key: "route" },
      { label: "attempts", render: (r) => r.attempt_count + "/" + r.max_attempts },
      { label: "last error", key: "last_error", wrap: true },
      { label: "updated", render: (r) => fmtTime(r.updated_at) },
      { label: "fingerprint", render: (r) => short(r.fingerprint) },
    ], data.deliveries, { onRow: showModal });
  });

  const loadProviders = guard(async () => {
    const data = await api("GET", "/api/v1/providers");
    renderTable($("#providers-table"), [
      { label: "name", key: "name" },
      { label: "kind", key: "kind" },
      { label: "enabled", render: (r, td) => td.append(badge(r.enabled ? "yes" : "no", r.enabled ? "dl-success" : "sev-debug")) },
      { label: "secret", render: (r) => (r.has_secret ? "••••" : "—") },
      { label: "managed", key: "managed_by" },
      { label: "", render: (r, td) => { const b = el("button", "Edit", "btn btn-sm"); b.onclick = () => openProvider(r); td.append(b); } },
    ], data.providers);
  });

  const loadRoutes = guard(async () => {
    const data = await api("GET", "/api/v1/routes");
    renderTable($("#routes-table"), [
      { label: "name", key: "name" },
      { label: "priority", key: "priority" },
      { label: "providers", render: (r) => (r.providers || []).join(", "), wrap: true },
      { label: "match", render: (r) => JSON.stringify(r.match || {}), wrap: true },
      { label: "flags", render: (r) => [r.is_default ? "default" : "", r.stop_processing ? "stop" : "", r.disabled ? "disabled" : ""].filter(Boolean).join(" ") },
      { label: "", render: (r, td) => { const b = el("button", "Edit", "btn btn-sm"); b.onclick = () => openRoute(r); td.append(b); } },
    ], data.routes);
  });

  const loaders = { events: loadEvents, states: loadStates, deliveries: loadDeliveries, providers: loadProviders, routes: loadRoutes, send: () => {} };

  // ---------------- provider form ----------------
  // Per-kind schema: the secret's meaning and the config fields differ by kind,
  // so the form renders typed inputs instead of a raw JSON blob. Field types:
  // text (default), number, select (options), textarea, json (parsed object).
  const PROVIDER_SCHEMA = {
    discord: { secret: "Webhook URL", fields: [{ key: "username" }, { key: "avatar_url" }] },
    slack: { secret: "Webhook URL", fields: [{ key: "username" }, { key: "icon_emoji" }] },
    webhook: {
      secret: "Target URL (or JSON {url,auth_header,auth_value})", fields: [
        { key: "method", placeholder: "POST" },
        { key: "content_type", placeholder: "application/json" },
        { key: "headers", type: "json", placeholder: '{"Authorization":"Bearer …"}' },
        { key: "template", type: "textarea", placeholder: "Go text/template; raw event JSON if blank" },
      ],
    },
    smtp: {
      secret: "Password", fields: [
        { key: "host", required: true }, { key: "port", type: "number", placeholder: "587" },
        { key: "username" }, { key: "from", required: true },
        { key: "to", required: true, placeholder: "comma-separated" },
        { key: "tls", type: "select", options: ["starttls", "tls", "none"] },
        { key: "subject_template", type: "textarea" },
      ],
    },
    telegram: {
      secret: "Bot token", fields: [
        { key: "chat_id", required: true },
        { key: "parse_mode", type: "select", options: ["", "Markdown", "HTML"] },
        { key: "api_base", advanced: true, placeholder: "https://api.telegram.org" },
      ],
    },
    ntfy: {
      secret: "Topic URL", secretPlaceholder: "https://ntfy.sh/your-topic", fields: [
        { key: "priority", type: "number", placeholder: "1-5" },
        { key: "tags", placeholder: "comma-separated" },
        { key: "token", placeholder: "optional bearer token" },
      ],
    },
    gotify: {
      secret: "App token", fields: [
        { key: "url", required: true, placeholder: "https://gotify.host" },
        { key: "priority", type: "number", placeholder: "5" },
      ],
    },
    pushover: {
      secret: "API token", fields: [
        { key: "user", required: true }, { key: "priority", type: "number" }, { key: "device" },
      ],
    },
    teams: { secret: "Webhook URL", fields: [] },
    matrix: {
      secret: "Access token", fields: [
        { key: "homeserver", required: true, placeholder: "https://matrix.org" },
        { key: "room_id", required: true, placeholder: "!abc:matrix.org" },
        { key: "msgtype", placeholder: "m.text" },
      ],
    },
    pagerduty: {
      secret: "Routing key", fields: [
        { key: "source" }, { key: "events_url", advanced: true },
      ],
    },
    opsgenie: { secret: "API key", fields: [{ key: "api_url", advanced: true }] },
    googlechat: { secret: "Webhook URL", fields: [] },
    twilio: {
      secret: "Auth token", fields: [
        { key: "account_sid", required: true },
        { key: "from", required: true, placeholder: "+1…" },
        { key: "to", required: true, placeholder: "+1…" },
        { key: "api_base", advanced: true },
      ],
    },
  };

  let providerMode = "create";
  const provForm = $("#provider-form");
  const fieldsBox = $("#provider-fields");
  $("#provider-new").onclick = () => openProvider(null);
  $("#provider-cancel").onclick = () => { provForm.hidden = true; };
  provForm.kind.addEventListener("change", () => {
    provForm._origConfig = {}; // switching kind discards the old kind's config
    renderProviderFields(provForm.kind.value, {});
  });

  // renderProviderFields builds typed inputs for a kind, populated from config.
  function renderProviderFields(kind, config) {
    config = config || {};
    const schema = PROVIDER_SCHEMA[kind] || { secret: "Secret", fields: [] };
    // Secret label + placeholder reflect this kind's meaning.
    $("#provider-secret-label").textContent = "Secret — " + schema.secret;
    provForm.secret.placeholder = providerMode === "edit"
      ? "(leave blank to keep existing)"
      : (schema.secretPlaceholder || schema.secret);

    fieldsBox.textContent = "";
    const advanced = [];
    schema.fields.forEach((f) => {
      const wrap = el("label");
      wrap.append(el("span", f.label || f.key + (f.required ? " *" : "")));
      let input;
      if (f.type === "select") {
        input = el("select");
        (f.options || []).forEach((o) => {
          const opt = el("option", o === "" ? "(none)" : o);
          opt.value = o;
          input.append(opt);
        });
      } else if (f.type === "textarea" || f.type === "json") {
        input = el("textarea");
        input.rows = f.type === "json" ? 3 : 4;
        input.spellcheck = false;
      } else {
        input = el("input");
        input.type = f.type === "number" ? "number" : "text";
      }
      input.dataset.key = f.key;
      input.dataset.ftype = f.type || "text";
      if (f.placeholder) input.placeholder = f.placeholder;
      // Populate from existing config.
      const v = config[f.key];
      if (v !== undefined && v !== null) {
        input.value = f.type === "json" ? JSON.stringify(v) : String(v);
      }
      wrap.append(input);
      if (f.advanced) advanced.push(wrap); else fieldsBox.append(wrap);
    });
    if (advanced.length) {
      const details = el("details");
      details.append(el("summary", "Advanced"));
      advanced.forEach((w) => details.append(w));
      fieldsBox.append(details);
    }
  }

  // readProviderConfig builds the config object from the rendered fields. With
  // PUT-replace semantics the form is authoritative: a filled field sets its key,
  // an empty one removes it. Unknown keys on the original (not in the schema) are
  // preserved so editing never silently drops them.
  function readProviderConfig(original) {
    const cfg = {};
    Object.keys(original || {}).forEach((k) => { cfg[k] = original[k]; });
    let bad = null;
    $$("#provider-fields [data-key]").forEach((input) => {
      const key = input.dataset.key, type = input.dataset.ftype;
      const raw = input.value.trim();
      if (raw === "") { delete cfg[key]; return; }
      if (type === "number") {
        const n = Number(raw);
        if (Number.isNaN(n)) { bad = key; return; }
        cfg[key] = n;
      } else if (type === "json") {
        try { cfg[key] = JSON.parse(raw); }
        catch { bad = key; }
      } else {
        cfg[key] = raw;
      }
    });
    if (bad) throw new Error(`invalid value for "${bad}"`);
    return cfg;
  }

  function openProvider(p) {
    providerMode = p ? "edit" : "create";
    provForm.hidden = false;
    provForm.reset();
    provForm.name.readOnly = !!p;
    provForm.kind.value = p ? p.kind : "discord";
    $("#provider-form-title").textContent = p ? "Edit provider: " + p.name : "New provider";
    $("#provider-submit").textContent = p ? "Save changes" : "Create";
    $("#provider-test").hidden = !p;
    if (p) {
      provForm.name.value = p.name;
      provForm.enabled.checked = !!p.enabled;
      provForm.secret.value = "";
    }
    // Remember the original config so edits preserve any keys not in the schema.
    provForm._origConfig = p && p.config ? p.config : {};
    renderProviderFields(provForm.kind.value, provForm._origConfig);
  }

  provForm.onsubmit = guard(async (e) => {
    e.preventDefault();
    let config;
    try { config = readProviderConfig(provForm._origConfig); }
    catch (err) { return toast(err.message, "err"); }
    const payload = { enabled: provForm.enabled.checked, kind: provForm.kind.value, config };
    if (provForm.secret.value) payload.secret = provForm.secret.value;
    if (providerMode === "create") {
      payload.name = provForm.name.value.trim();
      await api("POST", "/api/v1/providers", payload);
      toast("provider created", "ok");
    } else {
      // PUT (replace): the form is authoritative — cleared fields are removed.
      // Secret is preserved when blank.
      await api("PUT", "/api/v1/providers/" + encodeURIComponent(provForm.name.value), payload);
      toast("provider updated", "ok");
    }
    provForm.hidden = true;
    loadProviders();
  });

  $("#provider-test").onclick = guard(async () => {
    await api("POST", "/api/v1/test", { provider: provForm.name.value });
    toast("test notification sent", "ok");
  });

  // ---------------- route form ----------------
  let routeMode = "create";
  const routeForm = $("#route-form");
  $("#route-new").onclick = () => openRoute(null);
  $("#route-cancel").onclick = () => { routeForm.hidden = true; };

  function openRoute(r) {
    routeMode = r ? "edit" : "create";
    routeForm.hidden = false;
    routeForm.reset();
    routeForm.name.readOnly = !!r;
    $("#route-form-title").textContent = r ? "Edit route: " + r.name : "New route";
    $("#route-submit").textContent = r ? "Save changes" : "Create";
    if (r) {
      routeForm.name.value = r.name;
      routeForm.match.value = r.match ? JSON.stringify(r.match, null, 2) : "";
      routeForm.providers.value = (r.providers || []).join(", ");
      routeForm.priority.value = r.priority || 0;
      routeForm.dedup_window.value = r.dedup_window || "";
      routeForm.repeat_interval.value = r.repeat_interval || "";
      routeForm.is_default.checked = !!r.is_default;
      routeForm.stop_processing.checked = !!r.stop_processing;
      routeForm.disabled.checked = !!r.disabled;
    }
  }

  routeForm.onsubmit = guard(async (e) => {
    e.preventDefault();
    const payload = {
      priority: parseInt(routeForm.priority.value || "0", 10),
      is_default: routeForm.is_default.checked,
      stop_processing: routeForm.stop_processing.checked,
      disabled: routeForm.disabled.checked,
    };
    const matchRaw = routeForm.match.value.trim();
    if (matchRaw) {
      try { payload.match = JSON.parse(matchRaw); }
      catch { return toast("match must be valid JSON", "err"); }
    } else if (routeMode === "create") {
      payload.match = {};
    }
    const provs = routeForm.providers.value.split(",").map((s) => s.trim()).filter(Boolean);
    if (provs.length || routeMode === "create") payload.providers = provs;
    if (routeForm.dedup_window.value.trim()) payload.dedup_window = routeForm.dedup_window.value.trim();
    if (routeForm.repeat_interval.value.trim()) payload.repeat_interval = routeForm.repeat_interval.value.trim();

    if (routeMode === "create") {
      payload.name = routeForm.name.value.trim();
      await api("POST", "/api/v1/routes", payload);
      toast("route created", "ok");
    } else {
      // PUT (replace): the form is the authoritative route definition.
      await api("PUT", "/api/v1/routes/" + encodeURIComponent(routeForm.name.value), payload);
      toast("route updated", "ok");
    }
    routeForm.hidden = true;
    loadRoutes();
  });

  // ---------------- send event ----------------
  $("#event-form").onsubmit = guard(async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const ev = {
      event_id: fd.get("event_id"),
      type: fd.get("type"),
      source: fd.get("source"),
      title: fd.get("title"),
      timestamp: new Date().toISOString(),
    };
    if (fd.get("status")) ev.status = fd.get("status");
    if (fd.get("severity")) ev.severity = fd.get("severity");
    if (fd.get("summary")) ev.summary = fd.get("summary");
    const labels = (fd.get("labels") || "").trim();
    if (labels) {
      try { ev.labels = JSON.parse(labels); }
      catch { return toast("labels must be valid JSON", "err"); }
    }
    const res = await api("POST", "/api/v1/events", ev);
    const out = $("#event-result");
    out.hidden = false;
    out.textContent = JSON.stringify(res, null, 2);
    toast("event accepted", "ok");
  });

  // ---------------- tabs / chrome ----------------
  function currentTab() { const t = $(".tab.active"); return t ? t.dataset.tab : "events"; }
  function activate(name) {
    $$(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === name));
    $$(".panel").forEach((p) => p.classList.toggle("active", p.id === "tab-" + name));
    if (loaders[name]) loaders[name]();
  }
  $$(".tab").forEach((t) => (t.onclick = () => activate(t.dataset.tab)));

  function bindAuto(id, fn) {
    $(id).addEventListener("change", (e) => {
      clearInterval(autoTimers[id]);
      if (e.target.checked) autoTimers[id] = setInterval(fn, 5000);
    });
  }
  $("#events-refresh").onclick = loadEvents;
  $("#states-refresh").onclick = loadStates;
  $("#states-active").onchange = loadStates;
  $("#deliveries-refresh").onclick = loadDeliveries;
  $("#providers-refresh").onclick = loadProviders;
  $("#routes-refresh").onclick = loadRoutes;
  bindAuto("#events-auto", loadEvents);
  bindAuto("#states-auto", loadStates);
  bindAuto("#deliveries-auto", loadDeliveries);

  $("#modal-close").onclick = () => { $("#modal").hidden = true; };
  $("#modal").addEventListener("click", (e) => { if (e.target.id === "modal") $("#modal").hidden = true; });

  // ---------------- token + health ----------------
  $("#token").value = token;
  $("#token-save").onclick = () => {
    token = $("#token").value.trim();
    store.set(TOKEN_KEY, token);
    toast("token saved", "ok");
    activate(currentTab());
  };
  $("#token-clear").onclick = () => {
    token = "";
    store.del(TOKEN_KEY);
    $("#token").value = "";
    toast("token cleared");
  };
  // Enter in the token field applies it (same as clicking Save).
  $("#token").addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); $("#token-save").click(); }
  });
  // No token yet: focus the field so it's obvious where to start.
  if (!token) $("#token").focus();

  async function checkHealth() {
    const h = $("#health");
    try { const r = await fetch("/healthz"); h.className = "health " + (r.ok ? "ok" : "bad"); }
    catch { h.className = "health bad"; }
  }

  checkHealth();
  setInterval(checkHealth, 15000);
  activate("events");
})();

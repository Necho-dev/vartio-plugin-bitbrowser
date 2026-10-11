// @ts-check
// BitBrowser local API console, mounted by the probe-hub page (plugins.js).
import { API, state, esc } from "/modules/core.js";
import { icon } from "/modules/icons.js";

const KEY_STORE = "bitbrowserApiKey";

/**
 * @typedef {{ operationId: string, path: string, tag?: string, summary: string, required?: string[], confirm?: boolean, audit?: boolean, noAuth?: boolean, example?: any }} Op
 */

/** @param {HTMLElement} el @param {{ agent: string, embedded: boolean, base: string, api: (p: string, o?: any) => Promise<any>, open: (n: string, q?: Record<string, string>) => void }} ctx */
export async function mount(el, ctx) {
  const BASE = API + ctx.base;
  const agentPath = `${BASE}/agents/${encodeURIComponent(ctx.agent)}`;
  el.innerHTML = `
    <div class="pl-bar">
      <button class="btn sm" data-r="check">检测连通</button><span class="pl-meta" data-r="status"></span>
      <span class="pl-grow"></span>
      <input class="input" data-r="key" type="password" placeholder="Local API Token（留空用 Hub 默认）" autocomplete="off" style="width:240px">
      <a class="btn sm" href="${BASE}/openapi.json" target="_blank" rel="noopener">${icon("file-text")}openapi.json</a>
    </div>
    <div class="pl-split">
      <div><input class="input" data-r="filter" placeholder="搜索接口" style="width:100%;margin-bottom:8px"><div class="pl-list" data-r="ops"></div></div>
      <div>
        <div class="pl-sec"><h4 data-r="title">选择左侧接口</h4><div class="pl-meta" data-r="path"></div></div>
        <textarea class="input" data-r="body" spellcheck="false" rows="9" style="width:100%">{}</textarea>
        <div class="pl-bar" style="margin-top:8px"><button class="btn primary" data-r="send" disabled>${icon("send")}发送</button><span class="pl-meta" data-r="meta"></span><span class="pl-grow"></span><span data-r="extra"></span></div>
        <pre class="pl-pre faint" data-r="resp">尚无响应</pre>
        <details style="margin-top:10px"><summary class="faint">cURL（经 Hub 透传，可直接用于集成）</summary><pre class="pl-pre" data-r="curl" style="margin-top:6px"></pre></details>
      </div>
    </div>`;
  /** @param {string} r @returns {any} */
  const $ = (r) => el.querySelector(`[data-r="${r}"]`);
  $("key").value = localStorage.getItem(KEY_STORE) || "";

  /** @type {Op[]} */
  let ops = [];
  /** @type {Op | null} */
  let current = null;

  /** @param {Record<string, string>} [extra] */
  const headers = (extra = {}) => {
    /** @type {Record<string, string>} */
    const h = { "Content-Type": "application/json", ...extra };
    if (state.token) h.Authorization = "Bearer " + state.token;
    const k = $("key").value.trim();
    if (k) h["X-Api-Key"] = k;
    return h;
  };

  const check = async () => {
    const st = $("status");
    st.className = "pl-meta";
    st.textContent = "检测中…";
    try {
      const s = await ctx.api(`/agents/${encodeURIComponent(ctx.agent)}/status`);
      st.className = "pl-meta " + (s.reachable ? "ok" : "bad");
      st.textContent = s.reachable ? `可达 ${s.target} · ${s.latencyMs} ms` : `不可达：${s.message}`;
    } catch (e) {
      st.className = "pl-meta bad";
      st.textContent = e.message;
    }
  };

  /** @param {Op} op */
  const badges = (op) => (op.confirm ? '<span class="pl-pill warn">需确认</span>' : "") + (op.audit ? '<span class="pl-pill info">审计</span>' : "") + (op.noAuth ? '<span class="pl-pill">免 Token</span>' : "");

  const renderOps = () => {
    const q = $("filter").value.trim().toLowerCase();
    let tag = "", out = "";
    ops.forEach((op, i) => {
      if (q && !(op.operationId + op.path + op.summary).toLowerCase().includes(q)) return;
      if (op.tag !== tag) { tag = op.tag || ""; out += `<div class="group">${esc(tag)}</div>`; }
      out += `<div data-i="${i}" class="${current === op ? "on" : ""}"><code>${esc(op.path)}</code>${badges(op)}<small>${esc(op.summary)}</small></div>`;
    });
    $("ops").innerHTML = out || `<div class="group">没有匹配的接口</div>`;
  };
  $("ops").addEventListener("click", (/** @type {MouseEvent} */ e) => {
    const d = /** @type {HTMLElement | null} */ (/** @type {HTMLElement} */ (e.target).closest("[data-i]"));
    if (d) select(ops[Number(d.dataset.i)]);
  });

  /** @param {Op} op */
  const select = (op) => {
    current = op;
    $("title").innerHTML = esc(op.summary) + badges(op);
    $("path").textContent = `POST ${op.path} · operationId=${op.operationId}` + (op.required?.length ? ` · 必填 ${op.required.join(", ")}` : "");
    $("body").value = JSON.stringify(op.example || {}, null, 2);
    $("send").disabled = false;
    $("meta").textContent = "";
    $("extra").innerHTML = "";
    $("resp").className = "pl-pre faint";
    $("resp").textContent = "尚无响应";
    renderOps();
    renderCurl();
  };

  const renderCurl = () => {
    if (!current) return;
    let body = $("body").value;
    try { body = JSON.stringify(JSON.parse(body)); } catch { /* show as typed */ }
    const lines = [`curl -sS -X POST '${location.origin}${agentPath}/api${current.path}'`, `  -H 'Content-Type: application/json'`];
    if (state.token) lines.push(`  -H 'Authorization: Bearer $PROBE_HUB_ADMIN_TOKEN'`);
    if (!current.noAuth) lines.push(`  -H 'x-api-key: $BITBROWSER_API_KEY'`);
    if (current.confirm) lines.push(`  -H 'X-Confirm: true'`);
    lines.push(`  -d '${body.replace(/'/g, "'\\''")}'`);
    $("curl").textContent = lines.join(" \\\n");
  };

  const send = async () => {
    const op = current;
    if (!op) return;
    let body;
    try { body = JSON.stringify(JSON.parse($("body").value || "{}")); } catch (e) {
      $("meta").className = "pl-meta bad";
      $("meta").textContent = "请求体不是合法 JSON：" + e.message;
      return;
    }
    /** @type {Record<string, string>} */
    const extra = {};
    if (op.confirm) {
      if (!window.confirm(`${op.summary}\n${op.path}\n\n这是破坏性操作，确认执行？`)) return;
      extra["X-Confirm"] = "true";
    }
    $("send").disabled = true;
    $("meta").className = "pl-meta";
    $("meta").textContent = "请求中…";
    $("extra").innerHTML = "";
    const t0 = performance.now();
    try {
      const r = await fetch(`${agentPath}/call/${op.operationId}`, { method: "POST", headers: headers(extra), body });
      const text = await r.text();
      const ms = Math.round(performance.now() - t0);
      let pretty = text, ok = r.ok;
      /** @type {any} */
      let j = null;
      try { j = JSON.parse(text); pretty = JSON.stringify(j, null, 2); if (j && j.success === false) ok = false; } catch { /* not JSON */ }
      $("meta").className = "pl-meta " + (ok ? "ok" : "bad");
      $("meta").textContent = `HTTP ${r.status} · ${ms} ms`;
      $("resp").className = "pl-pre";
      $("resp").textContent = pretty;
      // An opened window reports its DevTools address; hand it to the CDP plugin.
      const http = ok && j && j.data && typeof j.data.http === "string" ? j.data.http : "";
      const port = http.split(":").pop();
      if (port && /^\d+$/.test(port)) {
        $("extra").innerHTML = `<button class="btn sm" data-r="toCdp">${icon("globe")}用 CDP 调试（端口 ${esc(port)}）</button>`;
        $("toCdp").addEventListener("click", () => ctx.open("cdp", { agent: ctx.agent, port }));
      }
    } catch (e) {
      $("meta").className = "pl-meta bad";
      $("meta").textContent = e.message;
    } finally {
      $("send").disabled = false;
    }
  };

  $("key").addEventListener("change", () => localStorage.setItem(KEY_STORE, $("key").value.trim()));
  $("filter").addEventListener("input", renderOps);
  $("body").addEventListener("input", renderCurl);
  $("check").addEventListener("click", check);
  $("send").addEventListener("click", send);

  try {
    const res = await ctx.api("/operations");
    ops = res.operations;
    if (res.apiKeyDefault) $("key").placeholder = "留空使用 Hub 配置的默认 Token";
    renderOps();
  } catch (e) {
    $("ops").innerHTML = `<div class="group">${esc(e.message)}</div>`;
  }
  check();
}

const $ = (id) => document.getElementById(id);
const view = $("view");
const dialog = $("dialog");
const agentDialog = $("agent-dialog");
const routes = { chat: ["Chat", "chat"], accounts: ["Accounts", "users"], ledger: ["Usage", "chart"], traffic: ["Traffic", "activity"], keys: ["API keys", "key"], settings: ["Settings", "settings"] };
const glyphs = {
  settings: '<path d="M4 6h16M4 12h16M4 18h16M8 4v4m8 2v4m-6 2v4"/>',
  chat: '<path d="M21 11.5a8.4 8.4 0 0 1-8.5 8.5H4l1.8-4A8.5 8.5 0 1 1 21 11.5Z"/><path d="M9 11.5h7"/>',
  users: '<circle cx="9" cy="8" r="3"/><path d="M2.5 20v-2a4 4 0 0 1 4-4h5a4 4 0 0 1 4 4v2M16 5.2a3 3 0 0 1 0 5.6m2 3.4a4 4 0 0 1 3.5 4V20"/>',
  user: '<circle cx="12" cy="8" r="3.5"/><path d="M5 21v-2a4 4 0 0 1 4-4h6a4 4 0 0 1 4 4v2"/>',
  chart: '<path d="M4 4v16h16M9 15v-4m5 4V7m5 8V4"/>',
  activity: '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
  key: '<circle cx="8" cy="15.5" r="4.5"/><path d="m11.2 12.3 8-8m-2 2 2.5 2.5m-5-0.1 2.5 2.5"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  arrow: '<path d="M5 12h14m-6-6 6 6-6 6"/>',
  send: '<path d="M12 19V5m-6 6 6-6 6 6"/>',
  Spark: '<path d="M12 2.5c1 6 3.5 8.5 9.5 9.5-6 1-8.5 3.5-9.5 9.5-1-6-3.5-8.5-9.5-9.5 6-1 8.5-3.5 9.5-9.5Z"/>',
  box: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
  refresh: '<path d="M20 11a8 8 0 1 0-2.3 6.7M20 5v6h-6"/>',
  edit: '<path d="m4 16 11-11 4 4L8 20H4v-4Zm9-9 4 4"/>',
  close: '<path d="m6 6 12 12M18 6 6 18"/>',
  copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 4H6a2 2 0 0 0-2 2v10"/>',
  search: '<circle cx="10.5" cy="10.5" r="5.5"/><path d="m15 15 4.5 4.5"/>',
  link: '<path d="M13 5h6v6m0-6-8 8m-1-8H6a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-4"/>',
  chevron: '<path d="m7 10 5 5 5-5"/>',
  write: '<path d="m12 19 7-7m-3-3 3-3 3 3-3 3M4 7h8M4 12h6M4 17h4"/>',
  code: '<path d="m8 6-6 6 6 6m8-12 6 6-6 6m-3-15-2 18"/>',
  trash: '<path d="M4 7h16m-10 4v5m4-5v5M9 7V4h6v3m3 0-1 13H7L6 7"/>',
  diagram: '<rect x="3" y="4" width="7" height="5" rx="1"/><rect x="14" y="15" width="7" height="5" rx="1"/><path d="M7 9v8h7"/>',
  table: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 10h18M3 14.5h18m-11-9.5v14"/>',
  checklist: '<path d="m3 7 2 2 3-4m-5 10 2 2 3-4m4-6h9m-9 8h9"/>',
  download: '<path d="M12 3v12m-4-4 4 4 4-4M4 16v3a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-3"/>',
  globe: '<circle cx="12" cy="12" r="8"/><path d="M4 12h16m-8-8a13 13 0 0 1 0 16 13 13 0 0 1 0-16Z"/>',
};
const icon = (name) => `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${glyphs[name]}</svg>`;
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const cap = (s) => s ? s.charAt(0).toUpperCase() + s.slice(1) : "";
const numberFormat = new Intl.NumberFormat();
const count = (n) => numberFormat.format(Number(n) || 0);
const ms = (n) => n >= 1000 ? `${(n / 1000).toFixed(1)} s` : `${Math.round(n || 0)} ms`;
const time = (n) => n ? new Date(n).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "—";
const label = (a) => a.label || a.display_name || a.email || a.id;
const accounts = () => state.account?.accounts || [];
const usable = (a) => !!a && a.enabled !== false && !a.expired;
const canChat = (id) => id ? usable(accounts().find((a) => a.id === id)) : accounts().some(usable);
const models = () => state.config?.models?.length ? state.config.models : ["smart", "fast", "balanced", "auto"];
const defaultModel = () => models().includes(state.config?.default_model) ? state.config.default_model : models()[0];
const state = { account: null, config: null, stats: null, keys: null, route: "", ready: false, search: "", loadError: "", loginWatch: false, loginTimer: null, watching: false, poll: false, accountVersion: 0, agentAccountId: "" };
let toastTimer;
let conversation;
const chatAccountKey = "awsquick.chat.account";
const chatModelKey = "awsquick.chat.model";
const chatThinkingKey = "awsquick.chat.thinking";
const thinkingOptions = [["", "Default"], ["off", "Off"], ["low", "Low"], ["medium", "Medium"], ["high", "High"]];
let lastChatAccount = "";
let lastChatModel = "";
let lastChatThinking = "";
try {
  lastChatAccount = localStorage.getItem(chatAccountKey) || "";
  lastChatModel = localStorage.getItem(chatModelKey) || "";
  const effort = localStorage.getItem(chatThinkingKey);
  if (thinkingOptions.some(([value]) => value === effort)) lastChatThinking = effort;
} catch {}
const preferredChatModel = () => models().includes(lastChatModel) ? lastChatModel : defaultModel();

function rememberChatAccount(id) {
  lastChatAccount = id;
  try { localStorage.setItem(chatAccountKey, id); } catch {}
}

async function api(path, method = "GET", data, signal) {
  const res = await fetch(path, { method, signal, ...(data === undefined ? {} : { headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }) });
  if (res.status === 401 && path.startsWith("/admin/")) {
    location.replace("/login");
    throw new Error("Dashboard login required");
  }
  if (res.ok && res.headers.get("content-type")?.includes("text/event-stream")) return res;
  const text = await res.text();
  let obj;
  try { obj = JSON.parse(text); } catch { obj = { error: text }; }
  if (!res.ok) throw new Error(typeof obj.error === "string" ? obj.error : res.statusText);
  return obj;
}

function toast(message, error = false) {
  const el = $("toast");
  el.textContent = message;
  el.classList.toggle("is-error", error);
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, error ? 6500 : 3200);
}

function button(text, action, style = "secondary", symbol = "", extra = "") {
  return `<button type="button" class="btn ${style}" data-action="${action}" ${extra}>${symbol ? `<span class="btn-icon" aria-hidden="true">${icon(symbol)}</span>` : ""}${esc(text)}</button>`;
}

async function withBusy(btn, work) {
  if (!btn) return work();
  if (btn.classList.contains("is-busy")) return;
  const wasDisabled = btn.disabled;
  btn.classList.add("is-busy");
  btn.setAttribute("aria-busy", "true");
  btn.disabled = true;
  try { return await work(); }
  finally {
    btn.classList.remove("is-busy");
    btn.removeAttribute("aria-busy");
    btn.disabled = wasDisabled;
  }
}

function session(a) {
  if (!a) return ["neutral", "No account"];
  if (a.enabled === false) return ["neutral", "Disabled"];
  if (a.expired) return ["bad", "Expired"];
  if (!a.expires_at && !a.expires_in) return ["neutral", "Unknown"];
  if (a.expires_in <= 0) return ["neutral", "Unknown"];
  return a.expires_in <= 300 ? ["warn", "Expiring soon"] : ["good", "Connected"];
}

function refreshLabel(a) {
  if (a.refresh?.running) return "Refreshing…";
  if (a.enabled === false) return "Paused";
  if (!a.has_refresh || !a.expires_at) return "Unavailable";
  return a.refresh?.last_failure ? "Retry scheduled" : "On";
}

function dateTime(value, milliseconds = false) {
  return value > 0 ? new Date(value * (milliseconds ? 1 : 1000)).toLocaleString() : "—";
}

function providerName(provider) {
  return ["social", "social_gateway"].includes(provider) ? "Builder ID / Social" : ["enterprise", "gateway"].includes(provider) ? "Enterprise" : cap(provider) || "—";
}

function expires(a) {
  if (a.expired) return "Expired";
  if (!(a.expires_in > 0)) return "—";
  const min = Math.ceil(a.expires_in / 60);
  if (min >= 60 * 24) return `${Math.floor(min / 1440)}d ${Math.floor(min / 60) % 24}h`;
  if (min >= 60) return `${Math.floor(min / 60)}h ${min % 60}m`;
  return `${min} min`;
}

function badge(kind, text, dot = false) {
  return `<span class="badge ${kind}${dot ? " dotted" : ""}">${esc(text)}</span>`;
}

function renderChrome() {
  $("nav").innerHTML = Object.entries(routes).map(([id, [title, glyph]]) => `<a href="#${id}" ${id === state.route ? 'aria-current="page"' : ""}>${icon(glyph)}<span>${title}</span></a>`).join("");
  $("page-title").textContent = routes[state.route][0];
  renderHeader();
  renderFooter();
}

function hasTrafficErrors() {
  return Number(state.stats?.errors) > 0 || (state.stats?.recent || []).some((r) => r.status && r.status !== "ok");
}

function renderHeader() {
  const login = state.account?.login;
  $("top-actions").innerHTML = state.route === "chat" ? button("New chat", "new", "secondary", "plus")
    : state.route === "accounts" ? `${button("Import", "import", "secondary", "plus")}${button("Export", "export-accounts", "secondary", "download", accounts().length ? "" : "disabled")}${login?.state === "waiting" ? button("Resume sign-in", "add", "primary", "arrow") : button("Add account", "add", "primary", "plus")}`
    : state.route === "traffic" ? button("Clear errors", "clear-errors", "secondary", "trash", hasTrafficErrors() ? "" : "disabled")
    : state.route === "keys" ? button("Create key", "mint", "primary", "plus") : "";
}

let panelAuthEnabled = false;
fetch("/auth/status", { credentials: "same-origin" }).then((r) => r.json()).then((st) => {
  panelAuthEnabled = !!st.enabled;
  renderFooter();
}).catch(() => {});

document.addEventListener("click", async (event) => {
  if (!event.target.closest?.("#logout")) return;
  try { await fetch("/auth/logout", { method: "POST", credentials: "same-origin" }); } catch {}
  location.replace("/login");
});

function renderFooter() {
  const n = accounts().filter(usable).length;
  const kind = n ? "good" : "neutral";
  const status = state.ready ? `${count(n)} available` : state.loadError ? "Offline" : "Connecting";
  const footer = $("sidebar-footer");
  const markup = `<a class="profile" href="#accounts"><span class="profile-avatar">${icon("users")}</span><span class="profile-text"><strong>Account pool</strong><small><i class="${kind}"></i>${esc(status)}</small></span>${icon("chevron")}</a>${panelAuthEnabled ? `<button type="button" class="btn quiet sidebar-logout" id="logout">Sign out</button>` : ""}`;
  if (footer._markup !== markup) { footer.innerHTML = markup; footer._markup = markup; }
}

function navigate() {
  const route = location.hash.slice(1);
  const next = Object.hasOwn(routes, route) ? route : "chat";
  if (next === state.route) return;
  const hadRoute = state.route;
  if (dialog.open) dialog.close();
  if (agentDialog.open) agentDialog.close();
  state.route = next;
  renderChrome();
  render();
  if (hadRoute) $("page-title").focus({ preventScroll: true });
  if (next === "keys") loadKeys();
}

function render() {
  view.classList.toggle("chat-view", state.route === "chat");
  view.scrollTop = 0;
  if (!state.ready) {
    view.innerHTML = state.loadError ? empty("activity", "Couldn’t load workspace", state.loadError, button("Try again", "reload", "primary", "refresh")) : '<p class="loading" role="status">Loading…</p>';
    return;
  }
  if (state.route === "chat") renderChat();
  else if (state.route === "accounts") renderAccounts();
  else if (state.route === "keys") renderKeys();
  else if (state.route === "settings") renderSettings();
  else renderStats();
}

function empty(symbol, title, text = "", action = "") {
  return `<div class="empty"><span class="empty-icon">${icon(symbol)}</span><h2>${esc(title)}</h2>${text ? `<p>${esc(text)}</p>` : ""}${action}</div>`;
}

function newChat(id = canChat(lastChatAccount) ? lastChatAccount : "", model = preferredChatModel()) {
  const nonce = globalThis.crypto?.randomUUID?.() || Math.random().toString(36).slice(2) + Date.now().toString(36);
  return { id: `thr-${nonce}`, accountId: id, model, thinking: lastChatThinking, messages: [], draft: "", controller: null, scroll: null };
}

function startChat(id) {
  conversation = newChat(id);
  if (state.route !== "chat") location.hash = "chat";
  else {
    renderChat();
    $("chat-input")?.focus();
  }
}

const prompts = [
  ["diagram", "Draw an SVG", "Draw an SVG diagram showing how a browser sends a request to a server and gets a response."],
  ["table", "Compare options", "Compare notebooks, spreadsheets, and task apps for planning a small project. Show the trade-offs in a table."],
  ["checklist", "Make a checklist", "Make a checklist for planning a weekend trip."],
  ["download", "Create a file", "Create a downloadable Markdown template for meeting notes."],
  ["write", "Draft an email", "Help me draft an email about "],
  ["Spark", "Brainstorm ideas", "Help me brainstorm ideas for "],
  ["code", "Explain code", "Explain what this code does:\n\n"],
];

function renderChat() {
  conversation ||= newChat();
  const t = conversation;
  view.innerHTML = `<div class="chat-space"><div class="chat-log" id="chat-log" tabindex="0" aria-label="Conversation"></div>
    <div class="composer-area"><form class="composer" id="chat-form">
      <textarea rows="1" id="chat-input" aria-label="Message" maxlength="100000"></textarea>
      <div class="composer-tools"><div class="selectors" id="chat-selectors"></div><div id="send-control"></div></div>
    </form><div class="composer-note"><span id="chat-status" role="status"></span><span class="keyboard">Enter to send <span>·</span> Shift + Enter for a new line</span></div></div></div>`;
  const input = $("chat-input");
  input.value = t.draft;
  input.addEventListener("input", () => { t.draft = input.value; sizeInput(); syncSend(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); if (!t.controller) $("chat-form").requestSubmit(); }
  });
  $("chat-form").addEventListener("submit", sendChat);
  $("chat-log").addEventListener("scroll", (e) => {
    const log = e.currentTarget;
    t.scroll = log.scrollHeight - log.clientHeight - log.scrollTop > 80 ? log.scrollTop : null;
  }, { passive: true });
  renderSelectors();
  paintChat();
  sizeInput();
}

function renderSelectors() {
  const t = conversation;
  if (!$("chat-selectors")) return;
  const list = accounts();
  const locked = t.messages.length > 0 || !!t.controller;
  if (!locked && t.accountId && !list.some((a) => a.id === t.accountId)) t.accountId = "";
  if (!locked && !models().includes(t.model)) t.model = preferredChatModel();
  const opts = list.map((a) => `<option value="${esc(a.id)}" ${t.accountId === a.id ? "selected" : ""} ${!usable(a) ? "disabled" : ""}>${esc(label(a))}</option>`).join("");
  const missing = t.accountId && !list.some((a) => a.id === t.accountId);
  $("chat-selectors").innerHTML = `<label class="selector" title="Account">${icon("user")}<select id="chat-account" aria-label="Chat account" ${locked || !list.length ? "disabled" : ""}>${missing ? '<option selected disabled>Account removed</option>' : `<option value="" ${!t.accountId ? "selected" : ""}>${list.length ? "Auto account" : "No accounts"}</option>`}${opts}</select>${icon("chevron")}</label>
    <label class="selector" title="Model">${icon("Spark")}<select id="chat-model" aria-label="Chat model" ${locked ? "disabled" : ""}>${!models().includes(t.model) ? `<option selected>${esc(t.model)}</option>` : ""}${models().map((model) => `<option value="${esc(model)}" ${model === t.model ? "selected" : ""}>${esc(cap(model))}</option>`).join("")}</select>${icon("chevron")}</label>
    <label class="selector selector-thinking" title="Thinking effort">${icon("settings")}<select id="chat-thinking" aria-label="Thinking effort">${thinkingOptions.map(([v, name]) => `<option value="${v}" ${v === t.thinking ? "selected" : ""}>Thinking: ${name}</option>`).join("")}</select>${icon("chevron")}</label>`;
  $("chat-account").addEventListener("change", (e) => {
    t.accountId = e.target.value;
    rememberChatAccount(t.accountId);
    syncSend();
  });
  $("chat-model").addEventListener("change", (e) => {
    t.model = lastChatModel = e.target.value;
    try { localStorage.setItem(chatModelKey, lastChatModel); } catch {}
  });
  $("chat-thinking").addEventListener("change", (e) => {
    t.thinking = lastChatThinking = e.target.value;
    try { localStorage.setItem(chatThinkingKey, lastChatThinking); } catch {}
  });
  $("send-control").innerHTML = '<button class="send" id="send-message" type="submit" title="Send message" aria-label="Send message">' + icon("send") + '</button>' + button("", "stop", "stop icon-button", "box", 'id="stop-message" aria-label="Stop response" title="Stop response"');
  syncSend();
}

function syncSend() {
  const t = conversation;
  const input = $("chat-input");
  if (!input) return;
  const a = accounts().find((a) => a.id === t.accountId);
  const blocked = !canChat(t.accountId);
  input.disabled = blocked;
  input.placeholder = !blocked ? "Message Quick…" : t.accountId ? "Account unavailable" : "Connect an account to chat";
  $("send-message").hidden = !!t.controller;
  $("send-message").disabled = blocked || !t.draft.trim();
  $("stop-message").hidden = !t.controller;
  const status = t.controller ? "Responding…" : a?.expired ? "Session expired — check Accounts" : "";
  if ($("chat-status").textContent !== status) $("chat-status").textContent = status;
}

function sizeInput() {
  const input = $("chat-input");
  input.style.height = "auto";
  input.style.height = `${Math.min(input.scrollHeight, 160)}px`;
}

function inlineMarkdown(text) {
  return text.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/g).map((part) => part.startsWith("`") && part.endsWith("`") && part.length > 2
    ? `<code>${esc(part.slice(1, -1))}</code>` : part.startsWith("**") && part.endsWith("**") && part.length > 4
      ? `<strong>${esc(part.slice(2, -2))}</strong>` : esc(part)).join("");
}

function markdown(text, pending = false) {
  const lines = text.split("\n");
  const html = [];
  let list = "";
  let paragraph = [];
  const flush = () => {
    if (list) { html.push(`</${list}>`); list = ""; }
    if (paragraph.length) { html.push(`<p>${paragraph.join("<br>")}</p>`); paragraph = []; }
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const fence = /^\s*```([\w+.-]*)\s*$/.exec(line);
    const item = /^\s*(?:([-*+])|\d+\.)\s+(.+)$/.exec(line);
    const heading = /^\s*(#{1,6})\s+(.+)$/.exec(line);
    if (fence) {
      flush();
      const code = [];
      while (++i < lines.length && !/^\s*```\s*$/.test(lines[i])) code.push(lines[i]);
      if (pending && fence[1].toLowerCase() === "svg") continue;
      html.push(`<div class="code-panel">${fence[1] ? `<div class="code-label">${esc(fence[1])}</div>` : ""}<pre><code>${esc(code.join("\n"))}</code></pre></div>`);
    } else if (item) {
      const tag = item[1] ? "ul" : "ol";
      if (list !== tag) { flush(); list = tag; html.push(`<${tag}>`); }
      html.push(`<li>${inlineMarkdown(item[2])}</li>`);
    } else if (heading) {
      flush();
      const level = Math.min(heading[1].length + 1, 6);
      html.push(`<h${level}>${inlineMarkdown(heading[2])}</h${level}>`);
    } else if (line.trim()) {
      if (list) flush();
      paragraph.push(inlineMarkdown(line));
    } else flush();
  }
  flush();
  return html.join("");
}

function chatWelcome() {
  const connected = accounts().some(usable);
  return `<div class="welcome">
    <h2>${connected ? "What can I help you with?" : "Connect an account to chat"}</h2>
    ${connected ? `<div class="prompts">${prompts.map(([symbol, title], i) => button(title, "prompt", "prompt", symbol, `data-id="${i}"`)).join("")}</div>` : `<a class="btn primary welcome-action" href="#accounts">${icon("users")} Manage accounts ${icon("arrow")}</a>`}</div>`;
}

function checklistProgress(artifact) {
  return `${artifact.checked?.filter(Boolean).length || 0} of ${artifact.items.length} complete`;
}

function artifactMarkup(artifact, messageIndex, artifactIndex) {
  const key = `${messageIndex}:${artifactIndex}`;
  const title = esc(artifact.title);
  const download = (label) => button(label, "download-artifact", "quiet artifact-download", "download", `data-id="${key}"`);
  switch (artifact.kind) {
    case "svg": {
      const src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(artifact.source)}`;
      const titleId = `artifact-title-${messageIndex}-${artifactIndex}`;
      return `<figure class="chat-artifact svg-artifact" aria-labelledby="${titleId}">
        <figcaption class="artifact-header"><strong id="${titleId}">${title}</strong>${download("Download SVG")}</figcaption>
        <div class="artifact-preview"><img src="${esc(src)}" alt="${title}"></div>
        <details class="artifact-source"><summary>SVG source</summary><pre><code>${esc(artifact.source)}</code></pre></details>
      </figure>`;
    }
    case "table":
      return `<div class="chat-artifact table-artifact" role="region" tabindex="0" aria-label="${title}"><table>
        <caption>${title}</caption><thead><tr>${artifact.columns.map((value) => `<th scope="col">${esc(value)}</th>`).join("")}</tr></thead>
        <tbody>${artifact.rows.map((row) => `<tr>${row.map((value) => `<td>${esc(value)}</td>`).join("")}</tr>`).join("")}</tbody>
      </table></div>`;
    case "checklist":
      return `<section class="chat-artifact checklist-artifact" aria-labelledby="artifact-title-${messageIndex}-${artifactIndex}">
        <div class="artifact-header"><strong id="artifact-title-${messageIndex}-${artifactIndex}">${title}</strong><span class="checklist-progress" role="status">${checklistProgress(artifact)}</span></div>
        <ul>${artifact.items.map((item, i) => `<li><label><input type="checkbox" data-checklist="${key}" data-item="${i}" ${artifact.checked?.[i] ? "checked" : ""}><span>${esc(item)}</span></label></li>`).join("")}</ul>
      </section>`;
    case "file":
      return `<div class="chat-artifact artifact-header file-artifact"><strong>${esc(artifact.filename)}</strong>${download("Download file")}</div>`;
    case "error":
      return `<p class="message-note err">${title}</p>`;
    default:
      return "";
  }
}

function chatArtifact(key) {
  const [messageIndex, artifactIndex] = key.split(":").map(Number);
  return conversation?.messages[messageIndex]?.artifacts?.[artifactIndex];
}

function downloadArtifact(key) {
  const artifact = chatArtifact(key);
  if (artifact?.kind !== "svg" && artifact?.kind !== "file") return;
  const anchor = document.createElement("a");
  const svg = artifact.kind === "svg";
  const filename = artifact.title?.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 80);
  const url = URL.createObjectURL(new Blob([artifact.source ?? ""], { type: svg ? "image/svg+xml" : "text/plain;charset=utf-8" }));
  anchor.href = url;
  anchor.download = svg ? `${filename || "diagram"}.svg` : artifact.filename;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

view.addEventListener("change", (event) => {
  const input = event.target;
  if (!input.matches("input[data-checklist]")) return;
  const artifact = chatArtifact(input.dataset.checklist);
  if (artifact?.kind !== "checklist") return;
  artifact.checked ||= Array(artifact.items.length).fill(false);
  artifact.checked[Number(input.dataset.item)] = input.checked;
  input.closest(".checklist-artifact").querySelector(".checklist-progress").textContent = checklistProgress(artifact);
});

function messageMarkup(m, i) {
  const you = m.role === "user";
  const hiddenSVG = m.pending && (/^\s*<svg\b/.test(m.content) || /^\s*```svg\s*$/i.test(m.content.split("\n", 1)[0]));
  const pending = m.pending && ((!m.content && !m.artifacts?.length) || hiddenSVG);
  const ui = m.usage;
  const cached = ui?.prompt_tokens_details?.cached_tokens || 0;
  const usage = ui ? `Billed ${count(Math.max(0, (ui.prompt_tokens || 0) - cached))} · Cached ${count(cached)} · Output ${count(ui.completion_tokens)}` : "";
  return `<article class="message ${you ? "user-message" : "assistant-message"}" data-message="${i}">
    ${you ? "" : `<span class="message-avatar">${icon("Spark")}</span>`}<div class="message-content">
    ${you ? `<p class="user-text">${esc(m.content)}</p>` : `<div class="response">${pending ? '<span class="typing" aria-label="Waiting for response"><i></i><i></i><i></i></span>' : markdown(m.content, m.pending)}</div>`}
    ${m.artifacts?.length ? `<div class="chat-artifacts">${m.artifacts.map((artifact, index) => artifactMarkup(artifact, i, index)).join("")}</div>` : ""}
    ${m.note ? `<p class="message-note ${m.failed ? "err" : ""}">${esc(m.note)}</p>` : ""}
    ${!you && !m.pending && (m.content || usage || m.failed) ? `<div class="message-tools">${m.content ? button("", "copy-message", "quiet icon-button", "copy", `data-id="${i}" title="Copy response" aria-label="Copy response"`) : ""}${m.failed && i === conversation.messages.length - 1 ? button("Retry", "retry", "quiet", "refresh") : ""}${usage ? `<span>${esc(usage)}</span>` : ""}</div>` : ""}
    </div></article>`;
}

function paintChat(index = null) {
  const log = $("chat-log");
  if (!log) return;
  const t = conversation;
  const row = index === null ? null : log.querySelector(`[data-message="${index}"]`);
  if (row) row.outerHTML = messageMarkup(t.messages[index], index);
  else log.innerHTML = t.messages.length ? `<div class="messages">${t.messages.map(messageMarkup).join("")}</div>` : chatWelcome();
  log.scrollTop = t.scroll === null ? log.scrollHeight : t.scroll;
}

async function readStream(res, message, update) {
  if (!(res instanceof Response) || !res.body) throw new Error("No response from Quick");
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  let complete = false;
  try {
    while (true) {
      const { value, done } = await reader.read();
      buf += decoder.decode(value, { stream: !done });
      const frames = buf.split(/\r?\n\r?\n/);
      buf = frames.pop();
      for (const frame of frames) {
        const data = frame.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
        if (!data || data === "[DONE]") continue;
        const ev = JSON.parse(data);
        if (ev.error) throw new Error(ev.error);
        if (ev.done) {
          complete = true;
          message.content = ev.text ?? message.content;
          message.usage = ev.usage;
          message.artifacts = ev.artifacts;
        } else if (ev.text) {
          message.content += ev.text;
          update();
        }
      }
      if (done) break;
    }
    if (!complete) throw new Error("The response ended early.");
    if (!message.content && !message.artifacts?.length) message.note = "No text returned.";
  } finally {
    if (!complete) await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

function sendChat(e) {
  e.preventDefault();
  const t = conversation;
  const text = $("chat-input").value.trim();
  if (text) runChat(t, text);
}

function chatHistory(messages) {
  const history = [];
  let artifactBudget = 48_000;
  for (let i = messages.length - 2; i >= 0; i -= 2) {
    const reply = messages[i + 1];
    if (!reply || reply.failed || reply.stopped) continue;
    const context = [reply.content];
    for (const artifact of reply.artifacts || []) {
      if (artifact.kind === "error") continue;
      const data = JSON.stringify(artifact);
      if (data.length <= artifactBudget) {
        context.push(`Previously displayed ${artifact.kind} artifact: ${data}`);
        artifactBudget -= data.length;
      } else {
        context.push(`Previously displayed ${artifact.kind} artifact: ${artifact.title || artifact.filename}`);
      }
    }
    const content = context.filter(Boolean).join("\n\n");
    if (content) history.unshift({ role: "user", content: messages[i].content }, { role: "assistant", content });
  }
  return history;
}

async function runChat(t, text, retry = false) {
  if (t.controller || !canChat(t.accountId)) return;
  if (retry) t.messages.splice(-2);
  else t.draft = "";
  const history = chatHistory(t.messages);
  const response = { role: "assistant", content: "", pending: true };
  t.messages.push({ role: "user", content: text }, response);
  t.scroll = null;
  t.controller = new AbortController();
  const active = () => conversation === t && state.route === "chat";
  const update = () => { if (active()) paintChat(t.messages.length - 1); };
  if (active()) {
    $("chat-input").value = t.draft;
    renderSelectors();
    sizeInput();
    paintChat();
  }
  try {
    const res = await api("/admin/chat", "POST", { account_id: t.accountId, model: t.model, thinking_effort: t.thinking, message: text, history, thread_id: t.id, stream: true, built_in_tools: true }, t.controller.signal);
    const routed = res.headers?.get("X-Account-Id");
    if (routed && !t.accountId) {
      t.accountId = routed;
      if (active()) renderSelectors();
    }
    await readStream(res, response, update);
  } catch (e) {
    response.stopped = t.controller.signal.aborted;
    response.failed = !response.stopped;
    response.note = response.stopped ? "Response stopped." : e.message;
  } finally {
    response.pending = false;
    t.controller = null;
    if (active()) {
      if (document.activeElement === $("stop-message")) $("chat-input").focus();
      syncSend();
      update();
      if (t.scroll === null && response.artifacts?.some((artifact) => artifact.kind !== "error")) {
        const log = $("chat-log");
        const row = log.querySelector(".message:last-child");
        if (row && row.offsetHeight > log.clientHeight) {
          log.scrollTop += row.getBoundingClientRect().top - log.getBoundingClientRect().top - 12;
        }
      }
    }
    loadStats().catch(() => {});
  }
}

function renderAccounts() {
  view.innerHTML = `<div class="page-body">
    <div class="section-head"><h2>All accounts <span id="account-count"></span></h2><label class="search" id="account-search-wrap">${icon("search")}<input id="account-search" type="search" placeholder="Find an account" aria-label="Find an account" value="${esc(state.search)}"></label></div>
    <div id="account-list"></div></div>`;
  $("account-search").addEventListener("input", (e) => { state.search = e.target.value; renderAccountList(); });
  renderAccountList();
}

function routeLabel(a) {
  return a.proxy || "Direct";
}

function renderAccountList() {
  const node = $("account-list");
  if (!node) return;
  const list = accounts();
  $("account-count").textContent = count(list.length);
  $("account-search-wrap").hidden = list.length < 2;
  if (node.contains(document.activeElement) || node.querySelector(".is-busy")) return;
  const search = state.search.trim().toLowerCase();
  const filtered = list.filter((a) => !search || list.length < 2 || [label(a), a.email, providerName(a.provider), a.region, a.aws_account, a.account_name].some((s) => s?.toLowerCase().includes(search)));
  const markup = !list.length ? empty("users", "No accounts yet", "", button("Add account", "add", "primary", "plus"))
    : !filtered.length ? empty("search", "No matching accounts")
    : `<div class="account-grid">${filtered.map(accountCard).join("")}</div>`;
  if (node._markup !== markup) { node.innerHTML = markup; node._markup = markup; }
}

function formatQuotaValue(value) {
  if (value == null || !Number.isFinite(Number(value))) return "0";
  return String(Number(Number(value).toFixed(1)));
}

function subscriptionLabel(type) {
  const raw = String(type || "").trim();
  if (!raw) return "";
  const map = {
    QUICK_LITE_FREE_TRIAL: "Free Trial",
    QUICK_FREE_TRIAL: "Free Trial",
    QUICK_LITE: "Quick Lite",
    QUICK_PLUS: "Quick Plus",
    QUICK_MAX: "Quick Max",
    QUICK_PRO: "Quick Pro",
    QUICK_ENTERPRISE: "Enterprise",
  };
  if (map[raw]) return map[raw];
  return raw.toLowerCase().split("_").filter(Boolean).map(cap).join(" ");
}

function quotaDate(ts) {
  return ts > 0 ? new Date(ts * 1000).toLocaleDateString([], { month: "short", day: "numeric" }) : "";
}

function quotaBar(period, meta, leftText, pct) {
  const clamped = Math.max(0, Math.min(100, Number(pct) || 0));
  const fillClass = clamped > 90 ? "critical" : clamped > 70 ? "high" : "";
  return `<div class="account-usage account-usage-${period}">
    <div class="usage-label"><span>${cap(period)} quota</span>${meta ? `<span>${esc(meta)}</span>` : ""}</div>
    <div class="usage-bar"><div class="usage-fill${fillClass ? ` ${fillClass}` : ""}" style="width:${clamped}%"></div></div>
    <div class="usage-text"><span>${esc(leftText)}</span><span>${clamped.toFixed(1)}%</span></div>
  </div>`;
}

function accountCard(a) {
  const [kind, status] = session(a);
  const name = label(a);
  const email = a.email;
  const attr = `data-id="${esc(a.id)}"`;
  const usage = a.usage || {};
  const q = a.quota;
  const subLabel = subscriptionLabel(q?.subscription_type);
  const remainingHtml = q && q.monthly_limit > 0
    ? `${esc(`${formatQuotaValue(q.monthly_available)} / ${formatQuotaValue(q.monthly_limit)}`)} <span class="account-detail-muted">(${esc(`${Math.max(0, 100 - (q.monthly_pct || 0)).toFixed(1)}% left`)})</span>`
    : "—";
  const monthlyBar = q && q.monthly_limit > 0
    ? quotaBar("monthly", q.monthly_resets_at > 0 ? `Resets ${quotaDate(q.monthly_resets_at)}` : "", `${formatQuotaValue(q.monthly_used)} / ${formatQuotaValue(q.monthly_limit)}`, q.monthly_pct)
    : "";
  const sessionBar = q && (q.session_pct != null || q.session_resume_minutes > 0)
    ? quotaBar("session", q.session_resume_minutes > 0 ? `Resumes in ${q.session_resume_minutes} min` : "", `${Math.max(0, 100 - (q.session_pct || 0)).toFixed(1)}% left`, q.session_pct)
    : "";
  return `<article class="account-card" ${attr}>
    <header class="account-top"><div class="account-name">
      <h3>${esc(name)}</h3>${email && email !== name ? `<p>${esc(email)}</p>` : ""}
      <div class="account-tags">${subLabel ? badge("neutral", subLabel) : ""}${badge("accent", providerName(a.provider))}${badge(kind, status, true)}${a.chat_agent_id ? badge("good", "Custom agent") : badge("neutral", "Default agent")}</div>
    </div><button type="button" class="account-switch" role="switch" aria-checked="${a.enabled !== false}" aria-label="Include ${esc(name)} in account pool" data-action="toggle-account" ${attr}><span>Enabled</span><span class="account-switch-track" aria-hidden="true"></span></button></header>
    <dl class="account-detail">
      ${detailItems([["Requests", count(usage.requests)], ["Errors", count(usage.errors), usage.errors ? "err" : ""], ["Last used", dateTime(usage.last_used, true)], ["Expires in", expires(a)], ["Region", a.region], ["Auto-refresh", refreshLabel(a)], ["Route", routeLabel(a), "account-detail-wide"], ["Tokens", `${count(usage.prompt_tokens)} in · ${count(usage.cache_read_tokens)} cached · ${count(usage.completion_tokens)} out`, "account-detail-wide"], ["Remaining", remainingHtml, "account-detail-wide-html"]])}
    </dl>
    ${sessionBar || monthlyBar ? `<div class="account-quotas">${sessionBar}${monthlyBar}</div>` : ""}
    <div class="account-actions">${button(a.refresh?.running ? "Refreshing…" : "Refresh", "refresh", "secondary", "refresh", attr + (a.has_refresh && !a.refresh?.running ? "" : " disabled"))}${button("Details", "details", "secondary", "user", attr)}${button("Edit", "edit", "secondary", "edit", attr)}${button("Chat", "account-chat", "secondary", "chat", attr + (usable(a) ? "" : " disabled"))}${button("Delete", "remove", "danger-quiet delete-account", "trash", attr)}</div>
    </article>`;
}

function detailItems(items) {
  return items.map(([title, value, style]) => {
    const wide = style === "account-detail-wide" || style === "account-detail-wide-html";
    const body = style === "account-detail-wide-html" ? (value || "—") : esc(value || "—");
    return `<div${wide ? ' class="account-detail-wide"' : ""}><dt>${esc(title)}</dt><dd${style === "err" ? ' class="err"' : ""}>${body}</dd></div>`;
  }).join("");
}

function showDetails(a) {
  if (!a) return;
  const [kind, status] = session(a);
  const { requests, errors, prompt_tokens, cache_read_tokens, completion_tokens, total_tokens, duration_ms, last_used } = a.usage || {};
  const q = a.quota;
  const section = (title, items) => `<section class="detail-section"><h3>${title}</h3><dl class="detail-grid">${detailItems(items)}</dl></section>`;
  const quotaSection = q ? section("Account quota", [
    ...(q.subscription_type ? [["Plan", subscriptionLabel(q.subscription_type)]] : []),
    ...(q.entitlement_status ? [["Entitlement", cap(q.entitlement_status.toLowerCase())]] : []),
    ...(q.monthly_limit > 0 ? [
      ["Monthly quota", `${formatQuotaValue(q.monthly_used)} / ${formatQuotaValue(q.monthly_limit)} (${(q.monthly_pct || 0).toFixed(1)}%)`],
      ["Remaining", `${formatQuotaValue(q.monthly_available)} / ${formatQuotaValue(q.monthly_limit)}`],
    ] : []),
    ...(q.monthly_resets_at > 0 ? [["Resets", dateTime(q.monthly_resets_at)]] : []),
    ["Session usage", q.session_resume_minutes > 0 ? `${(q.session_pct || 0).toFixed(1)}% (resumes in ${q.session_resume_minutes} min)` : `${(q.session_pct || 0).toFixed(1)}%`],
    ...(q.trial_status ? [["Trial", q.trial_expires_at > 0 ? `${cap(q.trial_status.toLowerCase())} · ends ${quotaDate(q.trial_expires_at)}` : cap(q.trial_status.toLowerCase())]] : []),
    ["Overage", q.overage_enabled ? "Enabled" : "Disabled"],
    ["Updated", dateTime(q.updated_at)],
  ]) : "";
  dialog.dataset.mode = "details";
  showDialog(label(a), `<div class="dialog-form">
    <div>${badge(kind, status, true)}</div>
    ${section("Identity", [["Email", a.email], ["AWS account", a.aws_account], ["Account name", a.account_name], ["Sign-in", providerName(a.provider)], ["Region", a.region], ["Local ID", a.id], ["Chat agent", a.chat_agent_id || "Default Quick agent"], ...(a.actor_arn ? [["Actor ARN", a.actor_arn]] : [])])}
    ${quotaSection}
    ${section("Session & routing", [["Expires", dateTime(a.expires_at)], ["Session obtained", dateTime(a.obtained_at)], ["Last refreshed", dateTime(a.refresh?.last_success)], ["Auto-refresh", refreshLabel(a)], ...(a.refresh?.last_failure ? [["Last refresh failure", dateTime(a.refresh.last_failure)], ["Next retry", dateTime(a.refresh.next_retry)]] : []), ["Route", routeLabel(a)], ["Upstream host", a.ws_host], ["Added", dateTime(a.created_at)]])}
    ${section("Proxy usage", [["Requests", count(requests)], ["Failures", count(errors)], ["Last used", dateTime(last_used, true)], ["Average latency", requests ? ms(duration_ms / requests) : "—"], ["Input tokens", count(prompt_tokens)], ["Cached input", count(cache_read_tokens)], ["Output tokens", count(completion_tokens)], ["Total tokens", count(total_tokens)]])}
    <div class="dialog-actions">${button("Edit account", "edit", "secondary", "edit", `data-id="${esc(a.id)}"`)}${button("Done", "close", "primary")}</div></div>`);
}

function showDialog(title, content) {
  dialog.classList.toggle("wide-dialog", dialog.dataset.mode === "details");
  dialog.innerHTML = `<div class="dialog-head"><h2 id="dialog-title">${esc(title)}</h2>${button("", "close", "quiet icon-button", "close", 'aria-label="Close"')}</div>${content}`;
  if (!dialog.open) dialog.showModal();
  dialog.querySelector("[autofocus]")?.focus();
}

function showAgentDialog(title, content) {
  agentDialog.innerHTML = `<div class="dialog-head"><h2 id="agent-dialog-title">${esc(title)}</h2>${button("", "close-agent", "quiet icon-button", "close", 'aria-label="Close agent manager"')}</div>${content}`;
  if (!agentDialog.open) agentDialog.showModal();
  agentDialog.querySelector("[autofocus]")?.focus();
}

const agentPath = "/admin/accounts/agent";
let agentLoad;
let agentPack;

async function openAgentManager(id, focusAgent = "") {
  const a = accounts().find((x) => x.id === id);
  if (!a) return;
  agentLoad?.abort();
  const load = new AbortController();
  agentLoad = load;
  state.agentAccountId = id;
  agentPack = null;
  showAgentDialog(`Agents · ${label(a)}`, '<div class="dialog-form"><p class="agent-loading" role="status"><span class="loading-spinner" aria-hidden="true"></span>Loading agents…</p></div>');
  try {
    const pack = await api(`${agentPath}?id=${encodeURIComponent(id)}`, "GET", undefined, load.signal);
    if (load.signal.aborted || !agentDialog.open) return;
    agentPack = pack;
    renderAgentManager(a, pack);
    if (focusAgent) [...agentDialog.querySelectorAll('[data-action="agent-edit"]')].find((b) => b.dataset.id === focusAgent)?.focus();
  } catch (e) {
    if (load.signal.aborted || !agentDialog.open) return;
    const attr = `data-id="${esc(id)}"`;
    showAgentDialog(`Agents · ${label(a)}`, `<div class="dialog-form"><p class="err" role="alert">${esc(e.message)}</p><div class="agent-actions">${button("Retry", "manage-agent", "secondary", "refresh", attr)}${button("Open by ID", "agent-open-id")}${button("New agent", "agent-new", "primary", "plus")}</div></div>`);
  }
}

function renderAgentManager(a, pack) {
  const selected = pack.selected_id;
  const row = (agent) => {
    const active = agent.id === selected;
    const attr = `data-id="${esc(agent.id)}"`;
    const selectable = agent.status === "ACTIVE" && agent.lifecycle === "PUBLISHED";
    return `<li class="agent-manager"><div class="agent-row-head"><strong>${esc(agent.name || "Custom agent")}</strong>${active ? badge("good", "Selected") : ""}${agent.status ? badge(selectable ? "neutral" : "warn", cap(agent.status.toLowerCase())) : ""}</div>
      ${agent.description ? `<p>${esc(agent.description)}</p>` : ""}<p class="agent-id">${esc(agent.id)}</p>
      ${agent.error ? `<p class="err">${esc(agent.error)}</p>` : `<div class="agent-actions">${!active && selectable ? button("Select", "agent-select", "primary", "", attr) : ""}${button("Edit", "agent-edit", "secondary", "edit", attr)}${button("Delete", "delete-agent", "danger-quiet", "trash", attr)}</div>`}</li>`;
  };
  showAgentDialog(`Agents · ${label(a)}`, `<div class="dialog-form">
    <p class="form-hint">The selected agent handles new chats for this account unless full AWS Quick context is enabled.</p>
    <div class="agent-actions">${button("New agent", "agent-new", "primary", "plus")}${button("Open by ID", "agent-open-id")}${button("Refresh", "manage-agent", "quiet", "refresh", `data-id="${esc(a.id)}"`)}</div>
    ${!pack.complete ? '<p class="form-hint">The upstream search is unavailable. Showing recently used agents, the selected agent, and agents managed here. Open other agents by ID.</p>' : ""}
    <ul class="agent-list"><li class="agent-manager"><div class="agent-row-head"><strong>Default agent</strong>${selected ? "" : badge("good", "Selected")}</div>${selected ? `<div>${button("Select default", "agent-select")}</div>` : ""}</li>
    ${pack.agents.map(row).join("")}</ul></div>`);
}

async function saveAgent(payload, success, accountID = state.agentAccountId) {
  const result = await api(agentPath, "POST", { id: accountID, ...payload });
  toast(success);
  if (["select", "delete"].includes(payload.action)) void reloadAccounts().catch((e) => toast(e.message, true));
  if (agentDialog.open && state.agentAccountId === accountID) await openAgentManager(accountID, result.agent_id);
}

function agentForm(agent = null) {
  const accountID = state.agentAccountId;
  const back = `data-id="${esc(accountID)}"`;
  const template = agentPack?.claude_instructions || "";
  showAgentDialog(agent ? `Edit · ${agent.name}` : "New agent", `<form class="dialog-form" id="agent-form">
    <label class="field"><span>Name</span><input name="name" maxlength="80" required autofocus value="${esc(agent?.name ?? "Claude")}"></label>
    <label class="field"><span>Description</span><input name="description" maxlength="500" value="${esc(agent?.description ?? "")}"></label>
    <label class="field"><span>Welcome message <small>Optional greeting</small></span><input name="welcome_message" maxlength="500" value="${esc(agent?.welcome_message ?? "")}"></label>
    <label class="field"><span>Agent instructions</span><textarea name="instructions" rows="8" maxlength="5000">${esc(agent?.instructions ?? template)}</textarea></label>
    ${template ? `<div>${button("Use Claude instructions", "agent-template", "quiet", "Spark")}</div>` : ""}${formError}
    <div class="dialog-actions">${button("Cancel", "manage-agent", "quiet", "", back)}${submit(agent ? "Save changes" : "Create agent")}</div></form>`);
  bindDialog("agent-form", async (data) => {
    data.name = data.name.trim();
    if (!data.name) throw new Error("Name is required.");
    await saveAgent({ ...data, action: agent ? "update" : "create", agent_id: agent?.id }, agent ? "Agent updated" : "Agent created. Select it to use it in chats.", accountID);
  });
}

async function editAgent(id, btn) {
  const accountID = state.agentAccountId;
  const load = agentLoad;
  const agent = await withBusy(btn, () => api(`${agentPath}?id=${encodeURIComponent(accountID)}&agent_id=${encodeURIComponent(id)}`));
  if (agentDialog.open && state.agentAccountId === accountID && agentLoad === load) agentForm(agent);
}

function openAgentByID() {
  const accountID = state.agentAccountId;
  showAgentDialog("Open agent by ID", `<form class="dialog-form" id="agent-by-id"><label class="field"><span>Agent ID</span><input name="agent_id" required maxlength="80" autofocus></label>${formError}<div class="dialog-actions">${button("Back", "manage-agent", "quiet", "", `data-id="${esc(accountID)}"`)}${submit("Open agent")}</div></form>`);
  bindDialog("agent-by-id", async ({ agent_id }) => {
    const id = agent_id.trim();
    if (!id) throw new Error("Agent ID is required.");
    const agent = await api(`${agentPath}?id=${encodeURIComponent(accountID)}&agent_id=${encodeURIComponent(id)}`);
    if (!agentDialog.open || state.agentAccountId !== accountID || !$("agent-by-id")) return;
    const attr = `data-id="${esc(agent.id)}"`;
    const canSelect = agent.status === "ACTIVE" && agent.lifecycle === "PUBLISHED";
    showAgentDialog(agent.name || "Custom agent", `<div class="dialog-form"><p class="agent-id">${esc(agent.id)}</p><p>${esc(agent.description)}</p><div class="agent-actions">${canSelect ? button("Select", "agent-select", "primary", "", attr) : ""}${button("Edit", "agent-edit", "secondary", "edit", attr)}${button("Delete", "delete-agent", "danger-quiet", "trash", attr)}${button("Back", "manage-agent", "quiet", "", `data-id="${esc(accountID)}"`)}</div></div>`);
  });
}

async function applyClaude(btn) {
  const form = $("settings-form");
  const results = $("claude-results");
  const instructions = form.elements.claude_instructions.value;
  const fullContext = form.elements.full_context;
  results.textContent = "Applying Claude instructions to accounts…";
  try {
    const pack = await withBusy(btn, () => api("/admin/accounts/agents/apply-claude", "POST", { instructions }));
    state.config = { ...state.config, claude_instructions: pack.claude_instructions, full_context: pack.full_context };
    if (form.isConnected) {
      if (form.elements.claude_instructions.value === instructions) form.elements.claude_instructions.value = pack.claude_instructions;
      fullContext.checked = false;
      results.innerHTML = `<ul class="claude-results">${pack.results.map((item) => `<li><strong>${esc(label(accounts().find((a) => a.id === item.id) || item))}</strong>: ${item.created} created · ${item.updated} updated · ${item.unchanged} unchanged${item.partial ? " · partial agent list" : ""}${(item.errors || []).map((e) => `<div class="err">${esc(e)}</div>`).join("")}</li>`).join("")}</ul>`;
    }
    const failed = pack.results.filter((item) => item.errors?.length).length;
    toast(failed ? `Finished with errors on ${failed} account(s)` : "Claude instructions applied", !!failed);
    void reloadAccounts().catch(() => {});
  } catch (e) { if (results.isConnected) results.textContent = e.message; throw e; }
}

const formError = '<p class="form-error err" role="alert" hidden></p>';
const cancel = () => button("Cancel", "close");
const submit = (text, style = "primary") => `<button class="btn ${style}" type="submit">${text}</button>`;
const formActions = (text, style) => `<div class="dialog-actions">${cancel()}${submit(text, style)}</div>`;

function bindDialog(formId, callback) {
  $(formId).addEventListener("submit", async (e) => {
    e.preventDefault();
    const form = e.currentTarget;
    const btn = form.querySelector('[type="submit"]');
    if (btn?.classList.contains("is-busy")) return;
    const error = form.querySelector(".form-error");
    if (error) error.hidden = true;
    try {
      await withBusy(btn, () => callback(Object.fromEntries(new FormData(form))));
    } catch (e) {
      if (error) { error.textContent = e.message; error.hidden = false; }
      else throw e;
    }
  });
}

function showAdd() {
  dialog.dataset.mode = "login";
  const login = state.account?.login || {};
  if (login.state === "waiting") { showLoginProgress(); return; }
  const addError = login.state === "failed" && login.error ? `<p class="form-error err" role="alert">${esc(login.error)}</p>` : formError;
  showDialog("Add account", `<form id="add-form" class="dialog-form"><fieldset><legend>Sign-in method</legend><div class="provider-options">
    <label><input type="radio" name="provider" value="social" checked><span>${icon("user")} Social</span></label>
    <label><input type="radio" name="provider" value="enterprise"><span>${icon("users")} Enterprise</span></label></div></fieldset>
    <label class="field"><span>Proxy <small>Optional</small></span><input name="proxy" placeholder="Proxy URL or host:port:user:pass" autocomplete="off" spellcheck="false"></label>
    ${addError}${formActions("Start sign-in")}</form>`);
  bindDialog("add-form", (data) => beginLogin({ ...data, proxy: data.proxy.trim() }));
}

async function beginLogin(data) {
  const st = await api("/admin/login/start", "POST", data);
  state.accountVersion++;
  state.account.login = st;
  renderHeader();
  watchLogin();
  if (dialog.open && dialog.dataset.mode === "login") showLoginProgress();
}

function parseKiroExport(raw) {
  let decoded;
  try { decoded = JSON.parse(raw); } catch { throw new Error("Choose a valid Kiro JSON export."); }
  const entries = Array.isArray(decoded) ? decoded : decoded?.accounts;
  if (!Array.isArray(entries)) throw new Error("Choose a Kiro export with an accounts list.");
  const identities = new Map();
  for (const row of entries) {
    const provider = row?.credentials?.provider || row?.idp || row?.provider;
    if (typeof provider !== "string" || provider.toLowerCase().replace(/[\s_-]/g, "") !== "builderid") continue;
    const email = typeof row.email === "string" ? row.email.trim() : "";
    if (!/^[^\s@]+@[^\s@]+$/.test(email) || email.length > 254) continue;
    const nickname = typeof row.nickname === "string" ? Array.from(row.nickname.trim()).slice(0, 50).join("") : "";
    identities.set(email.toLowerCase(), { email, nickname });
  }
  if (!identities.size) throw new Error("No Builder ID accounts with an email were found in this export.");
  return [...identities.values()];
}

function hasQuickCredentials(raw) {
  const text = String(raw || "").trim();
  if (!text) return false;
  try {
    const decoded = JSON.parse(text);
    const items = Array.isArray(decoded) ? decoded : (Array.isArray(decoded?.accounts) ? decoded.accounts : [decoded]);
    return items.some((row) => row && typeof row === "object" && (row.refresh_token || row.refreshToken || row.tokens?.refresh_token || row.upstream?.tokens?.refresh_token || row.access_token || row.accessToken || row.session?.jwt));
  } catch {
    const lines = text.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
    if (!lines.length) return false;
    try {
      const first = JSON.parse(lines[0]);
      return !!(first && typeof first === "object" && (first.refresh_token || first.refreshToken || first.tokens?.refresh_token || first.upstream?.tokens?.refresh_token));
    } catch {
      return false;
    }
  }
}

function showExportDialog() {
  dialog.dataset.mode = "export";
  showDialog("Export AWS Quick accounts", `<form id="export-form" class="dialog-form">
    <p class="form-hint">Export saved accounts in the unified <code>aws-quick/v1</code> format for backup, proxy sync, or re-import.</p>
    <div class="dialog-actions">
      ${button("Cancel", "close", "quiet")}
      ${button("Download JSONL", "download-export-jsonl", "secondary", "download")}
      ${button("Download JSON", "download-export-json", "primary", "download")}
    </div>
  </form>`);
}

function showImport() {
  dialog.dataset.mode = "import";
  showDialog("Import accounts", `<form id="import-kiro" class="dialog-form">
    <p class="form-hint">Upload or paste an AWS Quick export (<code>aws-quick/v1</code> JSON / JSONL / <code>upstream.json</code>) to import directly, or choose a Kiro Builder ID export to start a linked sign-in.</p>
    <div class="field"><label class="field-label" for="kiro-file">Export file <small>Optional</small></label><div class="file-picker">
      <input type="file" id="kiro-file" accept=".json,.jsonl,.txt,application/json" aria-describedby="kiro-filename">
      <span class="file-picker-button" aria-hidden="true">${icon("plus")} Choose JSON / JSONL</span>
      <span class="file-name" id="kiro-filename">No file selected</span>
    </div></div>
    <label class="field"><span>Or paste JSON / JSONL</span><textarea id="quick-import-raw" name="raw" rows="6" placeholder='{"format":"aws-quick/v1","email":"...","refresh_token":"...","client_id":"...","client_secret":"..."}'></textarea></label>
    <label class="field" id="kiro-identity-wrap" hidden><span>Builder ID account</span><span class="field-select"><select id="kiro-identity" name="identity"></select>${icon("chevron")}</span></label>
    ${formError}${formActions("Import")}</form>`);
  const form = $("import-kiro");
  const fileInput = $("kiro-file");
  const rawInput = $("quick-import-raw");
  const select = $("kiro-identity");
  const error = form.querySelector(".form-error");
  const submitButton = form.querySelector('[type="submit"]');
  let identities = [];
  let mode = "quick";
  submitButton.disabled = true;

  const inspectText = (rawText) => {
    identities = [];
    $("kiro-identity-wrap").hidden = true;
    error.hidden = true;
    const trimmed = String(rawText || "").trim();
    if (!trimmed) {
      submitButton.disabled = true;
      submitButton.textContent = "Import";
      return;
    }
    if (hasQuickCredentials(trimmed)) {
      mode = "quick";
      submitButton.disabled = false;
      submitButton.textContent = "Import AWS Quick";
      return;
    }
    try {
      const parsed = parseKiroExport(trimmed);
      mode = "kiro";
      identities = parsed;
      select.replaceChildren(...identities.map((item, i) => new Option(item.nickname ? `${item.nickname} — ${item.email}` : item.email, i)));
      $("kiro-identity-wrap").hidden = false;
      submitButton.disabled = false;
      submitButton.textContent = "Sign in to Quick";
    } catch (e) {
      mode = "quick";
      submitButton.disabled = false;
      submitButton.textContent = "Import";
      error.textContent = e.message;
      error.hidden = false;
    }
  };

  fileInput.addEventListener("change", async () => {
    const file = fileInput.files[0];
    $("kiro-filename").textContent = file ? file.name : "No file selected";
    $("kiro-filename").title = file?.name || "";
    if (!file) return;
    if (file.size > 10_000_000) {
      error.textContent = "The export file is larger than 10 MB.";
      error.hidden = false;
      return;
    }
    const text = await file.text();
    if (!form.isConnected || fileInput.files[0] !== file) return;
    rawInput.value = text;
    inspectText(text);
  });
  rawInput.addEventListener("input", () => inspectText(rawInput.value));

  bindDialog("import-kiro", async ({ raw, identity }) => {
    const payloadText = String(raw || "").trim();
    if (!payloadText) throw new Error("Choose a file or paste an AWS Quick / Kiro export.");
    if (mode === "kiro") {
      const item = identities[Number(identity)];
      if (!item) throw new Error("Select a Builder ID account.");
      dialog.dataset.mode = "login";
      try { await beginLogin({ provider: "social", expected_email: item.email, nickname: item.nickname }); }
      catch (e) { if (form.isConnected) dialog.dataset.mode = "import"; throw e; }
      return;
    }
    const res = await api("/admin/accounts/import", "POST", { raw: payloadText });
    applyAccounts(res);
    if (dialog.open && dialog.dataset.mode === "import") dialog.close();
    const s = res.summary || {};
    toast(`Imported ${s.uploaded || 0} account(s)${s.failed ? ` (${s.failed} failed)` : ""}`, !!s.failed && !s.uploaded);
  });
}

async function reconnect(a) {
  if (!a) return;
  if (state.account?.login?.state === "waiting") { showAdd(); return; }
  dialog.dataset.mode = "login";
  await beginLogin({ provider: a.provider, account_id: a.id });
}

function safeLoginURL(raw) {
  try {
    const url = new URL(raw);
    return ["http:", "https:"].includes(url.protocol) ? url.href : "";
  } catch { return ""; }
}

function showLoginProgress() {
  const login = state.account.login;
  const url = safeLoginURL(login.authorize_url || login.url);
  const next = !!login.authorize_url;
  const name = next ? "Resume AWS sign-in" : "Open sign-in";
  const heading = !url ? "Preparing sign-in…" : next ? "Finish signing in on AWS" : "Sign in to AWS Quick";
  const hint = next
    ? "Resume on AWS. If the final page won’t open, paste its full address here."
    : "If the sign-in tab lands on a local page that won’t open, paste its full address here. AWS may then ask you to finish authorization.";
  showDialog("Connect account", `<div class="dialog-form"><div class="login-progress"><span class="empty-icon">${icon("link")}</span><div><strong role="status">${heading}</strong>${url ? `<p>${hint}</p>` : ""}</div></div>
    ${url ? `<div class="login-links"><a id="login-link" class="btn primary" href="${esc(url)}" target="_blank" rel="noreferrer">${esc(name)} ${icon("link")}</a>${button("Copy link", "copy-login", "secondary", "copy")}</div>
    <form id="login-callback-form" class="login-callback-form"><label class="field"><span>Callback URL</span><input name="callback_url" type="url" placeholder="Paste the full callback URL" autocomplete="off" spellcheck="false" required maxlength="16384"></label>
    ${formError}<div class="dialog-actions">${button("Cancel sign-in", "cancel-login", "quiet")}${submit("Submit callback")}</div></form>` : `<div class="dialog-actions">${button("Cancel sign-in", "cancel-login", "quiet")}</div>`}</div>`);
  if (!url) return;
  bindDialog("login-callback-form", async ({ callback_url }) => {
    const form = $("login-callback-form");
    const result = await api("/admin/login/callback", "POST", { callback_url: callback_url.trim() });
    if (!dialog.open || dialog.dataset.mode !== "login" || form !== $("login-callback-form")) return;
    if (result.authorize_url) {
      const authorize = safeLoginURL(result.authorize_url);
      if (!authorize.startsWith("https://")) throw new Error("AWS did not return a valid sign-in link");
      state.accountVersion++;
      state.account.login.authorize_url = authorize;
      showLoginProgress();
      $("login-link")?.focus();
    } else {
      form.elements.callback_url.value = "";
      dialog.querySelector(".login-progress strong").textContent = "Finishing sign-in…";
      form.hidden = true;
    }
  });
}

function watchLogin() {
  if (state.loginTimer || state.watching) return;
  state.loginWatch = true;
  state.loginTimer = setTimeout(async () => {
    state.loginTimer = null;
    state.watching = true;
    const version = state.accountVersion;
    try {
      const st = await api("/admin/login/status");
      if (version !== state.accountVersion) return;
      const previousURL = state.account.login?.authorize_url || state.account.login?.url;
      state.account.login = st;
      if (st.state === "ok" && st.account_id) {
        state.loginWatch = false;
        await reloadAccounts();
        if (dialog.open && dialog.dataset.mode === "login") dialog.close();
        toast("Account connected");
      } else if (st.state === "failed" || st.state === "idle") {
        state.loginWatch = false;
        renderHeader();
        if (dialog.open && dialog.dataset.mode === "login") showAdd();
        if (st.state === "failed") toast(st.error || "Sign-in failed", true);
      } else if (dialog.open && dialog.dataset.mode === "login" && previousURL !== (st.authorize_url || st.url) &&
          !$("login-callback-form")?.querySelector('[type="submit"]')?.disabled) showLoginProgress();
    } catch (e) { toast(e.message, true); }
    finally {
      state.watching = false;
      if (state.loginWatch) watchLogin();
    }
  }, 1300);
}

function editAccount(a) {
  dialog.dataset.mode = "edit";
  showDialog("Edit account", `<form class="dialog-form" id="edit-account"><label class="field"><span>Display label</span><input name="label" value="${esc(a.label)}" placeholder="${esc(a.display_name || a.email || "Account name")}" autofocus></label>
    <label class="field"><span>Proxy <small>Leave blank for direct</small></span><input name="proxy" value="${esc(a.proxy)}" placeholder="Proxy URL or host:port:user:pass" autocomplete="off" spellcheck="false"></label>
    <label class="pool-toggle"><input type="checkbox" name="enabled" ${a.enabled === false ? "" : "checked"}> Include in account pool</label>
    <div>${button("Agents", "manage-agent", "secondary", "Spark", `data-id="${esc(a.id)}"`)}</div>
    ${a.email && ["social", "enterprise"].includes(a.provider) ? `<div>${button("Reconnect", "reconnect", "quiet", "link", `data-id="${esc(a.id)}"`)}</div>` : ""}
    ${formError}${formActions("Save changes")}</form>`);
  bindDialog("edit-account", async ({ label, proxy, enabled }) => {
    applyAccounts(await api("/admin/accounts/proxy", "POST", { id: a.id, label: label.trim() || a.display_name || a.email || a.id, proxy: proxy.trim(), enabled: enabled === "on" }));
    if (dialog.open && dialog.dataset.mode === "edit") dialog.close();
    toast("Account updated");
  });
}

function confirmAction(title, description, text, callback) {
  dialog.dataset.mode = "confirm";
  showDialog(title, `<form class="dialog-form" id="confirm-form"><p>${esc(description)}</p>${formError}<div class="dialog-actions">${button("Cancel", "close", "quiet")}<button class="btn danger" type="submit" autofocus>${esc(text)}</button></div></form>`);
  bindDialog("confirm-form", async () => {
    await callback();
    if (dialog.open && dialog.dataset.mode === "confirm") dialog.close();
  });
}

function table(headers, rows, name) {
  return `<div class="table-wrap" tabindex="0" role="region" aria-label="${esc(name)}"><table><thead><tr>${headers.map((h) => `<th scope="col">${esc(h)}</th>`).join("")}</tr></thead><tbody>${rows.map((row) => `<tr>${row.map((cell) => `<td>${cell}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`;
}

function chartMax(n) {
  if (n <= 4) return Math.max(1, n);
  const exp = 10 ** Math.floor(Math.log10(n));
  const f = n / exp;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * exp;
}

function hourName(ts) {
  return new Date(ts * 1000).toLocaleTimeString([], { hour: "numeric" });
}

function chart(hourly) {
  const nowHour = Math.floor(Date.now() / 3600000) * 3600;
  const byHour = new Map((hourly || []).map((item) => [item.hour, item]));
  const cols = Array.from({ length: 24 }, (_, i) => {
    const ts = nowHour - (23 - i) * 3600;
    const bucket = byHour.get(ts) || {};
    const requests = Number(bucket.requests) || 0;
    const errors = Math.min(requests, Math.max(0, Number(bucket.errors) || 0));
    return { ts, requests, errors, ok: Math.max(0, requests - errors), i };
  });
  const peak = Math.max(0, ...cols.map((c) => c.requests));
  const max = chartMax(peak);
  const mid = max / 2;
  const midLabel = Number.isInteger(mid) ? count(mid) : mid.toFixed(1);
  const failed = cols.reduce((n, c) => n + c.errors, 0);
  const total = cols.reduce((n, c) => n + c.requests, 0);
  const y = `<div class="chart-y" aria-hidden="true"><span>${count(max)}</span><span>${midLabel}</span><span>0</span></div>`;
  const bars = cols.map((c) => {
    const height = c.requests ? Math.max(8, (c.requests / max) * 100) : 0;
    const label = `${hourName(c.ts)}–${hourName(c.ts + 3600)} · ${count(c.ok)} successful · ${count(c.errors)} failed`;
    const stack = c.requests
      ? `<span class="chart-ok" style="flex:${Math.max(c.ok, 0)}"></span><span class="chart-fail" style="flex:${Math.max(c.errors, 0)}"></span>`
      : "";
    return `<div class="chart-col${c.i === 23 ? " is-now" : ""}${c.requests ? "" : " is-empty"}" title="${esc(label)}"><div class="chart-stack" style="height:${height}%">${stack}</div></div>`;
  }).join("");
  const x = cols.map((c, i) => {
    const show = i === 0 || i === 23 || i % 4 === 0;
    const text = i === 23 ? "Now" : hourName(c.ts);
    return show ? `<span>${esc(text)}</span>` : "<span></span>";
  }).join("");
  return `<div class="chart-meta"><span>${count(total)} in 24h${failed ? ` · ${count(failed)} failed` : ""}${peak ? ` · peak ${count(peak)}/h` : ""}</span><span class="chart-legend"><i class="ok"></i> Successful${failed ? `<i class="bad"></i> Failed` : ""}</span></div>
    <div class="chart-frame" role="img" aria-label="Hourly requests over the last 24 hours. ${count(total)} requests, ${count(failed)} failed, peak ${count(peak)} in an hour.">${y}<div class="chart">${bars}</div></div>
    <div class="chart-x" aria-hidden="true"><span></span><div class="chart-x-labels">${x}</div></div>`;
}

function renderStats() {
  const s = state.stats || {};
  const usage = state.route === "ledger";
  const items = s.recent || [];
  const modelRows = Object.entries(s.by_model || {}).sort((a, b) => b[1].requests - a[1].requests);
  const billed = (r) => r.billed_tokens ?? Math.max(0, (r.prompt_tokens || 0) - (r.cache_read_tokens || 0));
  view.innerHTML = `<div class="page-body">${usage ? `<div class="metrics">${[["Requests", count(s.requests)], ["Failed", count(s.errors)], ["Billed tokens", count(billed(s))], ["Cached tokens", count(s.cache_read_tokens)], ["Output tokens", count(s.completion_tokens)], ["p95 latency", s.requests ? ms(s.p95_ms) : "—"]].map(([name, value]) => `<div class="metric"><span>${name}</span><strong>${value}</strong></div>`).join("")}</div>
    <section class="chart-card"><div class="section-head"><h2>Requests over time</h2>${badge("neutral", "Last 24 hours")}</div>${chart(s.hourly)}</section>
    <div class="section-head"><h2>By model</h2>${button("Clear usage", "clear-stats", "quiet", "", s.requests ? "" : "disabled")}</div>
    ${modelRows.length ? table(["Model", "Requests", "Failed", "Tokens", "Avg. latency"], modelRows.map(([name, m]) => [esc(cap(name)), count(m.requests), count(m.errors), count(m.total_tokens), ms(m.requests ? m.duration_ms / m.requests : 0)]), "Usage by model") : empty("chart", "No usage yet")}`
    : `<div class="section-head traffic-head"><h2>Recent requests</h2>${badge("neutral", `${count(s.in_flight)} in flight`)}</div>${items.length ? table(["Time", "Model", "Format", "Status", "TTFT", "Billed", "Cached", "Output", "Error"], items.map((r) => [time(r.at), esc(cap(r.model)), r.stream ? "Stream" : "JSON", badge(r.status === "ok" ? "good" : "bad", r.status === "ok" ? "OK" : cap(r.status || "error")), r.ttft_ms == null ? "—" : ms(r.ttft_ms), count(billed(r)), count(r.cache_read_tokens), count(r.completion_tokens), r.status === "ok" ? "—" : `<span class="traffic-error" title="${esc(r.error || "")}">${esc(r.error || "—")}</span>`]), "Recent requests") : empty("activity", "No requests yet")}`}</div>`;
}

async function loadKeys() {
  try { state.keys = await api("/admin/keys"); }
  catch (e) { state.keys = { error: e.message }; }
  if (state.route === "keys" && state.ready) renderKeys();
}

function renderKeys() {
  const pack = state.keys;
  const keys = pack?.keys || [];
  view.innerHTML = `<div class="page-body">${!pack ? '<p class="loading" role="status">Loading keys…</p>' : pack.error ? empty("key", "Couldn’t load keys", pack.error, button("Try again", "reload-keys")) : `
    <div class="key-summary"><span class="overview-icon">${icon("key")}</span><div><h2>API access</h2><p><code>${esc(state.config?.addr || window.location.host)}/v1</code></p></div>${pack.corrupt ? badge("bad", "Key file unreadable") : badge(pack.required ? "good" : "warn", pack.required ? "Key required" : "No key required", true)}</div>
    <div class="section-head"><h2>Keys <span>${count(keys.length)}</span></h2></div>
    ${keys.length ? table(["Key", "Label", "Created", "Actions"], keys.map((k, i) => [`<code class="api-key">${esc(k.token || k.hint)}</code>`, esc(k.email || "—"), k.created_at ? esc(new Date(k.created_at * 1000).toLocaleDateString()) : "—", `<div class="table-actions">${button("Copy", "copy-saved-key", "quiet", "copy", `data-id="${i}"`)}${button("Revoke", "revoke", "danger-quiet", "", `data-id="${i}"`)}</div>`]), "API keys") : empty("key", "No API keys", "Requests to /v1 do not require a key.", button("Create key", "mint", "primary", "plus"))}`}</div>`;
}

function mint() {
  dialog.dataset.mode = "mint";
  showDialog("Create API key", `<form class="dialog-form" id="mint-form"><label class="field"><span>Label <small>Optional</small></span><input name="email" placeholder="e.g. Local development" autofocus></label>${formError}${formActions("Create key")}</form>`);
  bindDialog("mint-form", async ({ email }) => {
    const created = await api("/admin/keys", "POST", { email: email.trim() });
    dialog.dataset.mode = "secret";
    showDialog("API key created", `<div class="dialog-form"><p>This key stays on the API keys page and can be copied anytime.</p><input class="secret" id="new-key" readonly value="${esc(created.token)}" aria-label="New API key"><div class="dialog-actions">${button("Done", "close")}${button("Copy key", "copy-key", "primary", "copy")}</div></div>`);
    await loadKeys();
  });
}

async function copy(text, fallback) {
  try { await navigator.clipboard.writeText(text); }
  catch {
    if (fallback) {
      const field = document.createElement("input");
      field.value = text;
      field.readOnly = true;
      field.setAttribute("aria-label", "Sign-in link");
      field.className = "login-copy-fallback";
      dialog.querySelector(".login-copy-fallback")?.remove();
      fallback.after(field);
      field.select();
      if (document.execCommand("copy")) { field.remove(); $("login-link")?.focus(); }
      else { field.focus(); field.select(); throw new Error("Copy the selected link manually"); }
    } else {
      const field = $("new-key");
      if (field && field.value === text) { field.focus(); field.select(); }
      throw new Error("Clipboard unavailable");
    }
  }
  toast("Copied to clipboard");
}

function applyAccounts(pack) {
  state.accountVersion++;
  state.account = pack;
  renderHeader();
  renderFooter();
  if (state.route === "accounts") renderAccounts();
  if (state.route === "chat" && $("chat-selectors")) { renderSelectors(); if (!conversation.messages.length) paintChat(); }
}

async function reloadAccounts() {
  applyAccounts(await api("/admin/accounts"));
}

async function mutateAccount(btn, path, id, success) {
  await withBusy(btn, async () => {
    applyAccounts(await api(path, "POST", { id }));
    toast(success);
  });
}

async function loadStats() {
  const latest = await api("/admin/stats");
  const changed = JSON.stringify(state.stats) !== JSON.stringify(latest);
  state.stats = latest;
  if (changed && !view.contains(document.activeElement) && ["ledger", "traffic"].includes(state.route)) {
    renderStats();
    if (state.route === "traffic") renderHeader();
  }
}

async function renderSettings() {
  view.innerHTML = '<p class="loading" role="status">Loading settings…</p>';
  try {
    const prefs = await api("/admin/settings");
    if (state.route !== "settings") return;
    const field = (label, name, attrs = "") => `<label class="field"><span>${label}</span><input name="${name}" value="${esc(prefs[name])}" ${attrs}></label>`;
    const toggle = (label, name) => `<label class="pool-toggle"><input type="checkbox" name="${name}" ${prefs[name] ? "checked" : ""}>${label}</label>`;
    view.innerHTML = `<form class="page-body settings-form" id="settings-form">
      <section class="settings-panel"><h2>Chat defaults</h2>
        <label class="field"><span>Claude agent instructions</span><textarea name="claude_instructions" rows="8" maxlength="5000">${esc(prefs.claude_instructions)}</textarea></label>
        <p class="form-hint">Apply saves these instructions, turns off full Quick context, creates a Claude agent where none is known, and replaces the instructions on discovered agents named Claude. Other agents and their selection are left alone. API clients that send their own system prompt keep that prompt; these instructions are the fallback when they do not.</p>
        <div>${button("Apply Claude instructions to all accounts", "apply-claude", "secondary", "Spark", accounts().length ? "" : "disabled")}</div>
        <div id="claude-results" role="status"></div>
        <div class="settings-grid">${field("Models", "models", 'required placeholder="smart, fast, balanced, auto"')}${field("Default model", "default_model", "required")}
        <label class="field"><span>Default thinking effort</span><span class="field-select"><select name="thinking_effort">${[["", "Provider default"], ["off", "Off"], ["low", "Low"], ["medium", "Medium"], ["high", "High"]].map(([v, label]) => `<option value="${v}" ${prefs.thinking_effort === v ? "selected" : ""}>${label}</option>`).join("")}</select>${icon("chevron")}</span></label>
        ${field("Timeout (seconds)", "timeout_sec", 'type="number" min="1" max="3600" required')}</div>
        <div class="settings-toggles">${toggle("Use full AWS Quick context", "full_context")}${toggle("Keep upstream sessions", "keep_sessions")}</div>
      </section>
      <section class="settings-panel"><h2>Upstream</h2><div class="settings-grid">${field("Time zone", "timezone", 'placeholder="America/New_York"')}${field("Locale", "locale", 'placeholder="en-US"')}${field("App version", "app_version")}${field("Surface", "surface")}</div></section>
      <div class="settings-actions">${formError}${submit("Save settings")}</div></form>`;
    bindDialog("settings-form", async (data) => {
      const saved = await api("/admin/settings", "POST", {
        ...data,
        models: data.models.split(",").map((model) => model.trim()).filter(Boolean),
        default_model: data.default_model.trim(),
        timeout_sec: Number(data.timeout_sec),
        full_context: data.full_context === "on",
        keep_sessions: data.keep_sessions === "on",
        timezone: data.timezone.trim(), locale: data.locale.trim(), app_version: data.app_version.trim(), surface: data.surface.trim(),
      });
      state.config = { ...state.config, ...saved };
      if (state.route === "chat" && $("chat-selectors")) renderSelectors();
      toast("Settings saved");
    });
  } catch (e) {
    if (state.route === "settings") view.innerHTML = empty("settings", "Couldn’t load settings", e.message, button("Try again", "reload-settings", "secondary", "refresh"));
  }
}

async function loadWorkspace() {
  state.loadError = "";
  render();
  try {
    const [account, config, stats] = await Promise.all([api("/admin/accounts"), api("/admin/config"), api("/admin/stats")]);
    state.account = account;
    state.config = config;
    state.stats = stats;
    state.ready = true;
    if (account.login?.state === "waiting") watchLogin();
  } catch (e) { state.loadError = e.message; }
  renderChrome();
  render();
}

async function refreshWorkspace() {
  if (state.poll || !state.ready || document.hidden) return;
  state.poll = true;
  const version = state.accountVersion;
  try {
    const [pack] = await Promise.all([api("/admin/accounts"), loadStats()]);
    if (version !== state.accountVersion) return;
    const signature = (rows) => JSON.stringify(rows.map((a) => [a.id, label(a), usable(a)]));
    const changed = signature(accounts()) !== signature(pack.accounts || []);
    if (state.loginWatch) pack.login = state.account.login;
    state.account = pack;
    if (pack.login?.state === "waiting") watchLogin();
    renderFooter();
    if (state.route === "chat" && $("chat-selectors")) {
      if (changed) { renderSelectors(); if (!conversation.messages.length) paintChat(); }
      else syncSend();
    }
    if (state.route === "accounts") {
      const loginAction = $("top-actions").querySelector('[data-action="add"]');
      if (loginAction && (loginAction.textContent === "Resume sign-in") !== (pack.login?.state === "waiting")) renderHeader();
      if (!dialog.open) renderAccountList();
    }
  } catch { /* A transient poll failure should not interrupt the current view. */ }
  finally { state.poll = false; }
}

const actions = {
  new: () => startChat(),
  "account-chat": (_, id) => {
    rememberChatAccount(id);
    startChat(id);
  },
  prompt: (_, id) => {
    const input = $("chat-input");
    if (input.disabled) {
      const select = $("chat-account");
      if (!select.disabled) select.focus();
      return;
    }
    input.value = prompts[Number(id)][2];
    input.dispatchEvent(new Event("input"));
    input.focus();
  },
  stop: () => conversation?.controller?.abort(),
  retry: () => runChat(conversation, conversation.messages.at(-2).content, true),
  "copy-message": (_, id) => copy(conversation.messages[Number(id)].content),
  "download-artifact": (_, id) => downloadArtifact(id),
  add: () => { if (state.ready) showAdd(); },
  close: () => dialog.close(),
  "close-agent": () => agentDialog.close(),
  "copy-login": () => {
    const link = $("login-link");
    if (link) return copy(link.href, dialog.querySelector(".login-links"));
  },
  "cancel-login": (btn) => withBusy(btn, async () => {
    const login = await api("/admin/login/cancel", "POST");
    state.loginWatch = false;
    clearTimeout(state.loginTimer);
    state.loginTimer = null;
    state.accountVersion++;
    state.account.login = login;
    renderHeader();
    if (dialog.open && dialog.dataset.mode === "login") dialog.close();
    toast("Sign-in cancelled");
  }),
  import: () => { if (state.ready) showImport(); },
  "export-accounts": () => { if (state.ready) showExportDialog(); },
  "download-export-json": () => { dialog.close(); location.assign("/admin/accounts/export?format=json"); },
  "download-export-jsonl": () => { dialog.close(); location.assign("/admin/accounts/export?format=jsonl"); },
  details: (_, id) => showDetails(accounts().find((a) => a.id === id)),
  reconnect: (btn, id) => withBusy(btn, () => reconnect(accounts().find((a) => a.id === id))),
  refresh: (btn, id) => mutateAccount(btn, "/admin/accounts/refresh", id, "Session refreshed"),
  edit: (_, id) => { const a = accounts().find((a) => a.id === id); if (a) editAccount(a); },
  "manage-agent": (_, id) => openAgentManager(id),
  "agent-new": () => agentForm(),
  "agent-open-id": openAgentByID,
  "agent-edit": (btn, id) => editAgent(id, btn),
  "agent-template": () => { $("agent-form").elements.instructions.value = agentPack.claude_instructions; },
  "agent-select": (btn, id = "") => withBusy(btn, () => saveAgent({ action: "select", agent_id: id }, id ? "Agent selected" : "Default agent selected")),
  "delete-agent": (_, id) => {
    const accountID = state.agentAccountId;
    const name = agentPack?.agents.find((a) => a.id === id)?.name || id;
    showAgentDialog("Delete agent?", `<form class="dialog-form" id="agent-delete"><p>Delete ${esc(name)} from this account? This cannot be undone. If it is selected, the account will revert to the default agent.</p>${formError}<div class="dialog-actions">${button("Cancel", "manage-agent", "quiet", "", `data-id="${esc(accountID)}"`)}${submit("Delete agent", "danger")}</div></form>`);
    bindDialog("agent-delete", () => saveAgent({ action: "delete", agent_id: id }, "Agent deleted", accountID));
  },
  "apply-claude": applyClaude,
  "toggle-account": async (btn, id) => {
    const enabled = btn.getAttribute("aria-checked") !== "true";
    await withBusy(btn, async () => {
      applyAccounts(await api("/admin/accounts/enabled", "POST", { id, enabled }));
      toast(enabled ? "Account enabled" : "Account disabled");
    });
    [...view.querySelectorAll('[data-action="toggle-account"]')].find((el) => el.dataset.id === id)?.focus({ preventScroll: true });
  },
  remove: (_, id) => {
    const a = accounts().find((a) => a.id === id);
    if (a) confirmAction("Delete account?", `Remove ${a.email || label(a)} and its saved session from this dashboard? This does not delete your AWS account.`, "Delete account", async () => {
      const pack = await api("/admin/accounts", "DELETE", { id });
      if (lastChatAccount === id) rememberChatAccount("");
      applyAccounts(pack);
      toast("Account deleted");
    });
  },
  mint: () => { if (state.ready) mint(); },
  "copy-key": () => copy($("new-key").value),
  "copy-saved-key": (_, id) => {
    const key = state.keys?.keys?.[Number(id)];
    if (key?.token) return copy(key.token);
    throw new Error("Key is unavailable");
  },
  revoke: (_, id) => {
    const key = state.keys?.keys?.[Number(id)];
    if (key) confirmAction("Revoke API key?", `${key.email || key.hint} will no longer authorize requests.`, "Revoke key", async () => {
      await api("/admin/keys", "DELETE", { token: key.token });
      await loadKeys();
      toast("Key revoked");
    });
  },
  "clear-stats": () => confirmAction("Clear usage?", "This also clears the recent request log.", "Clear usage", async () => {
    state.stats = await api("/admin/stats/reset", "POST");
    try { await reloadAccounts(); } catch {}
    renderHeader();
    if (["ledger", "traffic"].includes(state.route)) renderStats();
    toast("Usage cleared");
  }),
  "clear-errors": () => confirmAction("Clear errors?", "Removes failed requests from the traffic log and resets error counts on Usage, models, and accounts. Successful requests are kept.", "Clear errors", async () => {
    state.stats = await api("/admin/stats/clear-errors", "POST");
    try { await reloadAccounts(); } catch {}
    renderHeader();
    if (["ledger", "traffic"].includes(state.route)) renderStats();
    toast("Errors cleared");
  }),
  "reload-keys": loadKeys,
  "reload-settings": renderSettings,
  reload: loadWorkspace,
};

document.addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-action]");
  if (!btn || btn.disabled || btn.classList.contains("is-busy") || !Object.hasOwn(actions, btn.dataset.action)) return;
  try { await actions[btn.dataset.action](btn, btn.dataset.id); }
  catch (e) { toast(e.message, true); }
});
dialog.addEventListener("keydown", (e) => {
  if (e.key === "Escape" || e.isComposing) return;
  if (e.key !== "Enter") return;
  if (e.target.closest("textarea") && !e.ctrlKey && !e.metaKey) return;
  const submitBtn = dialog.querySelector("form button[type=submit]");
  const go = submitBtn || dialog.querySelector(".dialog-actions .btn.primary") || dialog.querySelector(".dialog-actions [data-action=close]");
  if (!go || go.disabled) return;
  if (e.target.closest("button, a")) return;
  e.preventDefault();
  if (submitBtn && submitBtn.form) submitBtn.form.requestSubmit(submitBtn);
  else go.click();
});
dialog.addEventListener("close", () => {
  if (!dialog.open) {
    dialog.replaceChildren();
    delete dialog.dataset.mode;
    if (state.route === "accounts" && state.ready) renderAccountList();
  }
});
agentDialog.addEventListener("close", () => {
  if (agentDialog.open) return;
  agentLoad?.abort();
  state.agentAccountId = "";
  agentPack = null;
  agentDialog.replaceChildren();
});
window.addEventListener("hashchange", navigate);
navigate();
loadWorkspace();
setInterval(refreshWorkspace, 5000);

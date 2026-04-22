const state = {
  limit: 200,
  offset: 0,
  filters: {},
  selectedId: null,
  refreshTimer: null,
  autoRefresh: true,
  latestItems: [],
  metricsMode: "cards",
};

const el = {
  active: document.getElementById("activeConnections"),
  reqHour: document.getElementById("requestsHour"),
  totalRequests: document.getElementById("totalRequests"),
  outputSec: document.getElementById("outputSec"),
  totalTokens: document.getElementById("totalTokens"),
  ttft: document.getElementById("avgTtft"),
  errorRate: document.getElementById("errorRate"),
  lastUpdated: document.getElementById("lastUpdated"),
  summaryGrid: document.getElementById("summaryGrid"),
  btnMetricsCards: document.getElementById("btnMetricsCards"),
  btnMetricsStrip: document.getElementById("btnMetricsStrip"),
  body: document.getElementById("requestsBody"),
  empty: document.getElementById("emptyState"),
  resultsCount: document.getElementById("resultsCount"),
  drawer: document.getElementById("drawer"),
  drawerOverlay: document.getElementById("drawerOverlay"),
  drawerTitle: document.getElementById("drawerTitle"),
  drawerDelete: document.getElementById("drawerDelete"),
  drawerMeta: document.getElementById("drawerMeta"),
  detailModel: document.getElementById("detailModel"),
  detailStatusBadge: document.getElementById("detailStatusBadge"),
  detailStreamChip: document.getElementById("detailStreamChip"),
  drawerClose: document.getElementById("drawerClose"),
  rawReq: document.getElementById("rawRequest"),
  structuredResp: document.getElementById("structuredResponse"),
  rawResp: document.getElementById("rawResponse"),
  detailStatus: document.getElementById("detailStatus"),
  detailTtft: document.getElementById("detailTtft"),
  detailTotal: document.getElementById("detailTotal"),
  detailPromptRate: document.getElementById("detailPromptRate"),
  detailDecodeRate: document.getElementById("detailDecodeRate"),
  detailPromptTokens: document.getElementById("detailPromptTokens"),
  detailCachedPromptTokens: document.getElementById("detailCachedPromptTokens"),
  detailCacheHitPct: document.getElementById("detailCacheHitPct"),
  detailCompletionTokens: document.getElementById("detailCompletionTokens"),
  detailTotalTokens: document.getElementById("detailTotalTokens"),
  detailRequestBytes: document.getElementById("detailRequestBytes"),
  detailResponseBytes: document.getElementById("detailResponseBytes"),
  detailId: document.getElementById("detailId"),
  detailBackend: document.getElementById("detailBackend"),
  detailClient: document.getElementById("detailClient"),
  detailTime: document.getElementById("detailTime"),
  detailPath: document.getElementById("detailPath"),
  detailQuery: document.getElementById("detailQuery"),
  detailErrorRow: document.getElementById("detailErrorRow"),
  detailError: document.getElementById("detailError"),
  liveState: document.getElementById("liveState"),
  liveStateText: document.getElementById("liveStateText"),
  activeFilters: document.getElementById("activeFilters"),
  filterBody: document.getElementById("filterBody"),
  btnToggleFilters: document.getElementById("btnToggleFilters"),
  autoRefresh: document.getElementById("autoRefresh"),
  refreshNow: document.getElementById("refreshNow"),
  fQuery: document.getElementById("fQuery"),
  fPath: document.getElementById("fPath"),
  fModel: document.getElementById("fModel"),
  modelOptions: document.getElementById("modelOptions"),
  fMethod: document.getElementById("fMethod"),
  fStatus: document.getElementById("fStatus"),
  fSince: document.getElementById("fSince"),
  fStream: document.getElementById("fStream"),
  fErrorsOnly: document.getElementById("fErrorsOnly"),
  fWithTokens: document.getElementById("fWithTokens"),
  fChatCompletions: document.getElementById("fChatCompletions"),
  btnApply: document.getElementById("btnApply"),
  btnReset: document.getElementById("btnReset"),
  tabs: Array.from(document.querySelectorAll(".tab")),
};

const FILTERS_COLLAPSED_KEY = "llama-cpp-router-monitor.filters.collapsed";
const METRICS_MODE_KEY = "llama-cpp-router-monitor.metrics.mode";

async function fetchJSON(url, options = undefined) {
  const resp = await fetch(url, options);
  if (!resp.ok) {
    throw new Error(await resp.text());
  }
  return resp.json();
}

function setLiveState(mode, text) {
  el.liveState.dataset.state = mode;
  el.liveStateText.textContent = text;
}

function fmtMs(value) {
  if (!Number.isFinite(value) || value <= 0) {
    return "-";
  }
  return `${Math.round(value)} ms`;
}

function fmtRate(value) {
  if (!Number.isFinite(value) || value <= 0) {
    return "-";
  }
  return value.toFixed(value >= 100 ? 0 : 1);
}

function fmtNum(value) {
  if (!Number.isFinite(value)) {
    return "0";
  }
  return new Intl.NumberFormat("en-US").format(value);
}

function fmtPercent(value) {
  if (!Number.isFinite(value)) {
    return "0%";
  }
  return `${(value * 100).toFixed(2)}%`;
}

function fmtPercentValue(value) {
  if (!Number.isFinite(value)) {
    return "0%";
  }
  return `${value.toFixed(2)}%`;
}

function fmtBytes(value) {
  if (!Number.isFinite(value) || value <= 0) {
    return "0 B";
  }
  if (value < 1024) {
    return `${value} B`;
  }
  if (value < 1024 * 1024) {
    return `${(value / 1024).toFixed(1)} KB`;
  }
  return `${(value / (1024 * 1024)).toFixed(2)} MB`;
}

function fmtDate(value) {
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) {
    return value || "-";
  }
  return d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

function fmtDateTime(value) {
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) {
    return value || "-";
  }
  return d.toLocaleString([], {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function statusClass(code) {
  if (code === 0) {
    return "status-live";
  }
  if (code >= 500 || code === 0) {
    return "status-err";
  }
  if (code >= 400) {
    return "status-mid";
  }
  return "status-ok";
}

function isCompleted(item) {
  return (item.status_code || 0) > 0 || !!item.response_raw_path || !!item.error_text;
}

function statusText(item) {
  return isCompleted(item) ? String(item.status_code || 0) : "LIVE";
}

function detailStatusClass(item) {
  return statusClass(item.status_code || 0);
}

function setToneClass(node, prefix, tone) {
  const classes = Array.from(node.classList).filter((name) => !name.startsWith(prefix));
  node.className = classes.concat(tone ? [`${prefix}${tone}`] : []).join(" ");
}

function latencyTone(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms >= 20000) return "tone-critical";
  if (ms >= 8000) return "tone-hot";
  if (ms >= 2500) return "tone-warm";
  return "tone-cool";
}

function rateTone(rate) {
  if (!Number.isFinite(rate) || rate <= 0) return "";
  if (rate < 10) return "tone-critical";
  if (rate < 25) return "tone-hot";
  if (rate < 45) return "tone-warm";
  return "tone-good";
}

function cacheTone(pct) {
  if (!Number.isFinite(pct) || pct <= 0) return "";
  if (pct >= 50) return "tone-good";
  if (pct >= 20) return "tone-warm";
  return "tone-hot";
}

function metricCell(primary, secondary = "") {
  const secondaryHTML = secondary
    ? `<div class="cell-subtle">${escapeHTML(secondary)}</div>`
    : "";
  return `<div class="cell-metric">${escapeHTML(primary)}</div>${secondaryHTML}`;
}

function numericCell(primary, secondary = "") {
  const secondaryHTML = secondary
    ? `<div class="cell-subtle cell-subtle-numeric">${escapeHTML(secondary)}</div>`
    : "";
  return `<div class="cell-metric cell-metric-numeric">${escapeHTML(primary)}</div>${secondaryHTML}`;
}

function buildStructuredResponseView(raw, rec) {
  if (!raw || /^response payload unavailable/i.test(raw)) {
    return raw || "response payload unavailable";
  }

  if (!rec?.is_streaming) {
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }

  const chunks = [];
  const contentParts = [];
  const finishReasons = [];
  let usage = null;
  let timings = null;
  let model = rec.model || "";

  for (const line of raw.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed.startsWith("data:")) {
      continue;
    }
    const payload = trimmed.slice(5).trim();
    if (!payload || payload === "[DONE]") {
      continue;
    }
    try {
      const chunk = JSON.parse(payload);
      chunks.push(chunk);
      if (chunk.model && !model) {
        model = chunk.model;
      }
      if (chunk.usage) {
        usage = chunk.usage;
      }
      if (chunk.timings) {
        timings = chunk.timings;
      }
      const choices = Array.isArray(chunk.choices) ? chunk.choices : [];
      for (const choice of choices) {
        if (typeof choice?.text === "string" && choice.text) {
          contentParts.push(choice.text);
        }
        if (typeof choice?.delta?.content === "string" && choice.delta.content) {
          contentParts.push(choice.delta.content);
        }
        if (typeof choice?.message?.content === "string" && choice.message.content) {
          contentParts.push(choice.message.content);
        }
        if (choice?.finish_reason && !finishReasons.includes(choice.finish_reason)) {
          finishReasons.push(choice.finish_reason);
        }
      }
    } catch {
      chunks.push({ unparsed: payload });
    }
  }

  return JSON.stringify({
    model: model || null,
    streaming: true,
    chunks_count: rec?.chunks_count || chunks.length,
    output_text: contentParts.join(""),
    finish_reasons: finishReasons,
    usage,
    timings,
    chunks,
  }, null, 2);
}

function collectFilters() {
  const filters = {};
  if (el.fQuery.value.trim()) filters.q = el.fQuery.value.trim();
  if (el.fPath.value.trim()) filters.path = el.fPath.value.trim();
  if (el.fModel.value.trim()) filters.model = el.fModel.value.trim();
  if (el.fMethod.value) filters.method = el.fMethod.value;
  if (el.fStatus.value.trim()) filters.status = el.fStatus.value.trim();
  if (el.fSince.value.trim()) filters.since_hours = el.fSince.value.trim();
  if (el.fStream.value) filters.stream = el.fStream.value;
  if (el.fErrorsOnly.checked) filters.errors_only = "true";
  if (el.fWithTokens.checked) filters.with_tokens = "true";
  if (el.fChatCompletions.checked) filters.chat_completions_only = "true";
  state.filters = filters;
  renderFilterTags();
}

function resetFilters() {
  el.fQuery.value = "";
  el.fPath.value = "";
  el.fModel.value = "";
  el.fMethod.value = "";
  el.fStatus.value = "";
  el.fSince.value = "";
  el.fStream.value = "";
  el.fErrorsOnly.checked = false;
  el.fWithTokens.checked = false;
  el.fChatCompletions.checked = false;
  collectFilters();
}

function setFiltersCollapsed(collapsed) {
  el.filterBody.hidden = collapsed;
  el.btnToggleFilters.textContent = collapsed ? "Expand" : "Collapse";
  el.btnToggleFilters.setAttribute("aria-expanded", String(!collapsed));
  localStorage.setItem(FILTERS_COLLAPSED_KEY, collapsed ? "1" : "0");
}

function setMetricsMode(mode) {
  state.metricsMode = mode === "strip" ? "strip" : "cards";
  el.summaryGrid.dataset.mode = state.metricsMode;
  el.btnMetricsCards.classList.toggle("is-active", state.metricsMode === "cards");
  el.btnMetricsStrip.classList.toggle("is-active", state.metricsMode === "strip");
  localStorage.setItem(METRICS_MODE_KEY, state.metricsMode);
}

function renderFilterTags() {
  const entries = Object.entries(state.filters);
  el.activeFilters.innerHTML = "";
  if (!entries.length) {
    const span = document.createElement("span");
    span.className = "filter-tag";
    span.textContent = "No active filters";
    el.activeFilters.appendChild(span);
    return;
  }

  const labels = {
    q: "search",
    path: "path",
    model: "model",
    method: "method",
    status: "status",
    since_hours: "since",
    stream: "stream",
    errors_only: "errors",
    with_tokens: "tokens",
    chat_completions_only: "chat",
  };
  entries.forEach(([key, value]) => {
    const tag = document.createElement("span");
    tag.className = "filter-tag";
    if (key === "chat_completions_only") {
      tag.textContent = "POST /v1/chat/completions";
    } else {
      tag.textContent = `${labels[key] || key}: ${value}`;
    }
    el.activeFilters.appendChild(tag);
  });
}

function queryString(obj) {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(obj)) {
    if (value !== undefined && value !== null && `${value}` !== "") {
      params.set(key, value);
    }
  }
  return params.toString();
}

function hasActiveFilters() {
  return Object.keys(state.filters).length > 0;
}

function updateOutputRateFromRows(items) {
  const rates = (items || [])
    .map((item) => Number(item.decode_tok_per_sec || 0))
    .filter((value) => Number.isFinite(value) && value > 0);
  const avg = rates.length
    ? rates.reduce((sum, value) => sum + value, 0) / rates.length
    : 0;
  el.outputSec.textContent = fmtRate(avg);
}

async function loadStats() {
  const hours = Number.parseInt(state.filters.since_hours || "1", 10) || 1;
  const stats = await fetchJSON(`/_monitor/stats?${queryString({ hours, ...state.filters })}`);
  el.active.textContent = fmtNum(stats.active_connections || 0);
  el.reqHour.textContent = fmtNum(Math.round((stats.requests_per_minute || 0) * 60));
  el.totalRequests.textContent = fmtNum(hasActiveFilters() ? (stats.matching_total_requests || 0) : (stats.lifetime_total_requests || 0));
  el.totalTokens.textContent = fmtNum(hasActiveFilters() ? (stats.matching_total_tokens || 0) : (stats.lifetime_total_tokens || 0));
  el.ttft.textContent = fmtMs(stats.avg_first_byte_ms || 0);
  el.errorRate.textContent = fmtPercent(stats.error_rate || 0);
  el.lastUpdated.textContent = fmtDate(new Date().toISOString());
}

async function loadModelOptions() {
  const data = await fetchJSON("/_monitor/models");
  const items = Array.isArray(data.items) ? data.items : [];
  el.modelOptions.innerHTML = "";
  for (const model of items) {
    const option = document.createElement("option");
    option.value = model;
    el.modelOptions.appendChild(option);
  }
}

async function loadRequests() {
  const params = {
    limit: state.limit,
    offset: state.offset,
    ...state.filters,
  };
  const data = await fetchJSON(`/_monitor/requests?${queryString(params)}`);
  state.latestItems = data.items || [];
  renderRequests(state.latestItems);
}

function renderRequests(items) {
  el.body.innerHTML = "";
  el.resultsCount.textContent = fmtNum(items.length);
  el.empty.hidden = items.length > 0;

  for (const item of items) {
    const row = document.createElement("tr");
    const completed = isCompleted(item);
    if (state.selectedId === item.id) {
      row.classList.add("is-selected");
    }
    if (!completed) {
      row.classList.add("is-live");
      row.setAttribute("aria-disabled", "true");
      row.title = "Request is still in progress";
    }

    const requestCell = `
      <div class="request-cell">
        <div class="request-top">
          <span class="method-badge">${escapeHTML(item.method || "-")}</span>
          <span class="path-text">${escapeHTML(item.path || "-")}</span>
        </div>
        <div class="subtle">${escapeHTML(item.query || "No query string")}</div>
      </div>`;

    row.innerHTML = `
      <td class="align-left sticky-col sticky-time">${metricCell(fmtDate(item.created_at), item.client_ip || "unknown client")}</td>
      <td class="sticky-col sticky-request">${requestCell}</td>
      <td class="align-center"><span class="status-badge ${statusClass(item.status_code || 0)}">${escapeHTML(statusText(item))}</span></td>
      <td class="align-left">
        <div class="model-text cell-metric">${escapeHTML(item.model || "-")}</div>
        <div class="cell-subtle">${!completed ? "in progress" : item.is_streaming ? "streaming" : "standard"}</div>
      </td>
      <td class="align-right ${latencyTone(item.first_byte_ms)}">${numericCell(fmtMs(item.first_byte_ms), "first token")}</td>
      <td class="align-right">${numericCell(fmtMs(item.total_ms), !completed ? "running" : `${fmtNum(item.chunks_count || 0)} chunks`)}</td>
      <td class="align-right">${numericCell(fmtNum(item.prompt_tokens || 0))}</td>
      <td class="align-right">${numericCell(fmtNum(item.completion_tokens || 0))}</td>
      <td class="align-right">${numericCell(fmtNum(item.total_tokens || 0))}</td>
      <td class="align-right ${cacheTone(item.cache_hit_pct || 0)}">${numericCell(fmtPercentValue(item.cache_hit_pct || 0), item.cached_prompt_tokens > 0 ? `${fmtNum(item.cached_prompt_tokens)} cached` : "no cache")}</td>
      <td class="align-right ${rateTone(item.decode_tok_per_sec || 0)}">${numericCell(fmtRate(item.decode_tok_per_sec || 0), `prompt ${fmtRate(item.prompt_tok_per_sec || 0)}`)}</td>
    `;

    if (completed) {
      row.addEventListener("click", () => openDetails(item.id));
    }
    el.body.appendChild(row);
  }
  updateOutputRateFromRows(items);
}

async function fetchRaw(id, part) {
  const resp = await fetch(`/_monitor/raw/${encodeURIComponent(id)}/${part}`);
  if (!resp.ok) {
    return `${part} payload unavailable (${resp.status})`;
  }
  const text = await resp.text();
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

async function openDetails(id) {
  state.selectedId = id;
  renderRequests(state.latestItems);
  el.drawer.classList.add("open");
  el.drawer.setAttribute("aria-hidden", "false");
  el.drawerOverlay.hidden = false;
  document.body.classList.add("drawer-open");
  el.drawerDelete.disabled = true;

  const rec = await fetchJSON(`/_monitor/request/${encodeURIComponent(id)}`);
  el.drawerTitle.textContent = `${rec.method || "-"} ${rec.path || "-"}`;
  el.drawerMeta.textContent = `${rec.method || "-"} ${rec.path || "-"}${rec.query ? `?${rec.query}` : ""}`;
  el.detailModel.textContent = rec.model || "-";
  el.detailStatusBadge.textContent = statusText(rec);
  el.detailStatusBadge.className = `status-badge ${detailStatusClass(rec)}`;
  el.detailStreamChip.textContent = rec.is_streaming ? "Streaming" : "Standard";

  el.detailStatus.textContent = statusText(rec);
  el.detailTtft.textContent = fmtMs(rec.first_byte_ms);
  el.detailTotal.textContent = fmtMs(rec.total_ms);
  el.detailPromptRate.textContent = fmtRate(rec.prompt_tok_per_sec || 0);
  el.detailDecodeRate.textContent = fmtRate(rec.decode_tok_per_sec || 0);
  el.detailPromptTokens.textContent = fmtNum(rec.prompt_tokens || 0);
  el.detailCachedPromptTokens.textContent = fmtNum(rec.cached_prompt_tokens || 0);
  el.detailCacheHitPct.textContent = rec.prompt_tokens > 0 ? fmtPercentValue(rec.cache_hit_pct || 0) : "-";
  el.detailCompletionTokens.textContent = fmtNum(rec.completion_tokens || 0);
  el.detailTotalTokens.textContent = fmtNum(rec.total_tokens || 0);
  el.detailRequestBytes.textContent = fmtBytes(rec.request_bytes || 0);
  el.detailResponseBytes.textContent = fmtBytes(rec.response_bytes || 0);
  el.detailId.textContent = rec.id || "-";
  el.detailBackend.textContent = rec.backend_url || "-";
  el.detailClient.textContent = rec.client_ip || "-";
  el.detailTime.textContent = fmtDateTime(rec.created_at);
  el.detailPath.textContent = `${rec.method || "-"} ${rec.path || "-"}`;
  el.detailQuery.textContent = rec.query || "-";
  el.detailErrorRow.hidden = !rec.error_text;
  el.detailError.textContent = rec.error_text || "-";
  el.drawerDelete.disabled = !isCompleted(rec);
  setToneClass(el.detailStatus.parentElement, "mini-stat-", detailStatusClass(rec).replace("status-", ""));
  setToneClass(el.detailTtft.parentElement, "mini-stat-", latencyTone(rec.first_byte_ms).replace("tone-", ""));
  setToneClass(el.detailTotal.parentElement, "mini-stat-", latencyTone(rec.total_ms).replace("tone-", ""));
  setToneClass(el.detailPromptRate.parentElement, "mini-stat-", rateTone(rec.prompt_tok_per_sec || 0).replace("tone-", ""));
  setToneClass(el.detailDecodeRate.parentElement, "mini-stat-", rateTone(rec.decode_tok_per_sec || 0).replace("tone-", ""));
  setToneClass(el.detailCacheHitPct.parentElement, "mini-stat-", cacheTone(rec.cache_hit_pct || 0).replace("tone-", ""));
  el.rawReq.textContent = "Loading request payload...";
  el.structuredResp.textContent = "Building structured response...";
  el.rawResp.textContent = "Loading response payload...";

  const [rawReq, rawResp] = await Promise.all([
    fetchRaw(id, "request"),
    fetchRaw(id, "response"),
  ]);

  if (state.selectedId !== id) {
    return;
  }

  el.rawReq.textContent = rawReq;
  el.structuredResp.textContent = buildStructuredResponseView(rawResp, rec);
  el.rawResp.textContent = rawResp;
}

function clearDetails() {
  state.selectedId = null;
  el.drawer.classList.remove("open");
  el.drawer.setAttribute("aria-hidden", "true");
  el.drawerOverlay.hidden = true;
  document.body.classList.remove("drawer-open");
  el.drawerTitle.textContent = "Select a request";
  el.drawerMeta.textContent = "Choose a row to inspect request metadata and raw payloads.";
  el.detailModel.textContent = "-";
  el.detailStatusBadge.textContent = "-";
  el.detailStatusBadge.className = "status-badge status-live";
  el.detailStreamChip.textContent = "-";
  el.detailStatus.textContent = "-";
  el.detailTtft.textContent = "-";
  el.detailTotal.textContent = "-";
  el.detailPromptRate.textContent = "-";
  el.detailDecodeRate.textContent = "-";
  el.detailPromptTokens.textContent = "-";
  el.detailCachedPromptTokens.textContent = "-";
  el.detailCacheHitPct.textContent = "-";
  el.detailCompletionTokens.textContent = "-";
  el.detailTotalTokens.textContent = "-";
  el.detailRequestBytes.textContent = "-";
  el.detailResponseBytes.textContent = "-";
  el.detailId.textContent = "-";
  el.detailBackend.textContent = "-";
  el.detailClient.textContent = "-";
  el.detailTime.textContent = "-";
  el.detailPath.textContent = "-";
  el.detailQuery.textContent = "-";
  el.detailErrorRow.hidden = true;
  el.detailError.textContent = "-";
  el.drawerDelete.disabled = true;
  setToneClass(el.detailStatus.parentElement, "mini-stat-", "");
  setToneClass(el.detailTtft.parentElement, "mini-stat-", "");
  setToneClass(el.detailTotal.parentElement, "mini-stat-", "");
  setToneClass(el.detailPromptRate.parentElement, "mini-stat-", "");
  setToneClass(el.detailDecodeRate.parentElement, "mini-stat-", "");
  setToneClass(el.detailCacheHitPct.parentElement, "mini-stat-", "");
  el.rawReq.textContent = "No request selected.";
  el.structuredResp.textContent = "No request selected.";
  el.rawResp.textContent = "No request selected.";
  renderRequests(state.latestItems);
}

async function deleteSelectedRequest() {
  if (!state.selectedId) {
    return;
  }
  const id = state.selectedId;
  const ok = window.confirm(`Delete request ${id}? This will remove the DB row and raw payloads.`);
  if (!ok) {
    return;
  }
  el.drawerDelete.disabled = true;
  await fetchJSON(`/_monitor/request/${encodeURIComponent(id)}`, { method: "DELETE" });
  clearDetails();
  await refreshAll();
}

function setupTabs() {
  for (const tab of el.tabs) {
    tab.addEventListener("click", () => {
      for (const other of el.tabs) {
        other.classList.remove("active");
      }
      tab.classList.add("active");
      const target = tab.dataset.tab;
      el.rawReq.classList.toggle("active", target === "request");
      el.structuredResp.classList.toggle("active", target === "structured");
      el.rawResp.classList.toggle("active", target === "response");
    });
  }
}

async function refreshAll() {
  await Promise.all([loadStats(), loadRequests()]);
}

function scheduleEventRefresh() {
  if (!state.autoRefresh) {
    return;
  }
  if (state.refreshTimer) {
    clearTimeout(state.refreshTimer);
  }
  state.refreshTimer = setTimeout(() => {
    refreshAll().catch((err) => {
      setLiveState("error", `Refresh failed: ${err.message}`);
    });
  }, 180);
}

function connectEvents() {
  const source = new EventSource("/_monitor/events");

  source.onopen = () => {
    setLiveState("live", "Live stream connected");
  };

  source.onerror = () => {
    setLiveState("retry", "Reconnecting to event stream");
  };

  source.addEventListener("request", () => {
    scheduleEventRefresh();
  });
}

function wireFilters() {
  el.btnToggleFilters.addEventListener("click", () => {
    setFiltersCollapsed(!el.filterBody.hidden);
  });

  el.btnMetricsCards.addEventListener("click", () => setMetricsMode("cards"));
  el.btnMetricsStrip.addEventListener("click", () => setMetricsMode("strip"));

  el.btnApply.addEventListener("click", async () => {
    collectFilters();
    await loadRequests();
  });

  el.btnReset.addEventListener("click", async () => {
    resetFilters();
    await loadRequests();
  });

  el.refreshNow.addEventListener("click", () => {
    refreshAll().catch((err) => {
      setLiveState("error", `Refresh failed: ${err.message}`);
    });
  });

  el.autoRefresh.addEventListener("change", () => {
    state.autoRefresh = el.autoRefresh.checked;
    el.autoRefresh.parentElement.classList.toggle("is-active", state.autoRefresh);
  });

  el.drawerClose.addEventListener("click", clearDetails);
  el.drawerDelete.addEventListener("click", () => {
    deleteSelectedRequest().catch((err) => {
      setLiveState("error", `Delete failed: ${err.message}`);
    });
  });
  el.drawerOverlay.addEventListener("click", clearDetails);

  [el.fQuery, el.fPath, el.fModel, el.fStatus, el.fSince].forEach((node) => {
    node.addEventListener("keydown", async (event) => {
      if (event.key === "Enter") {
        collectFilters();
        await loadRequests();
      }
    });
  });
}

async function boot() {
  renderFilterTags();
  setFiltersCollapsed(localStorage.getItem(FILTERS_COLLAPSED_KEY) === "1");
  setMetricsMode(localStorage.getItem(METRICS_MODE_KEY) || "cards");
  el.autoRefresh.parentElement.classList.toggle("is-active", el.autoRefresh.checked);
  setupTabs();
  wireFilters();
  connectEvents();
  await Promise.all([refreshAll(), loadModelOptions()]);
  window.setInterval(() => {
    if (!state.autoRefresh) {
      return;
    }
    loadStats().catch(() => {});
  }, 5000);
  window.setInterval(() => {
    if (!state.autoRefresh) {
      return;
    }
    loadRequests().catch(() => {});
  }, 9000);
  window.setInterval(() => {
    loadModelOptions().catch(() => {});
  }, 30000);
}

boot().catch((err) => {
  setLiveState("error", `Boot failed: ${err.message}`);
});

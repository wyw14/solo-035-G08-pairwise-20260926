const stamp = document.getElementById("stamp");
const content = document.getElementById("content");
const form = document.getElementById("submit-form");
const submitMsg = document.getElementById("submit-msg");
const actionSelect = form.elements.action;
const delayLabel = form.querySelector('[data-when="schedule-failover"]');

const ACTION_LABELS = {
  "stop-program": "停止节目",
  "audio-failover": "音频切备路",
  "schedule-failover": "定时触发备路",
  "hold-event": "挂起节目",
  "mark-failed-over": "标记已切备",
};

function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"']/g, ch => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[ch]));
}

function syncDelayField() {
  delayLabel.hidden = actionSelect.value !== "schedule-failover";
}
actionSelect.addEventListener("change", syncDelayField);
syncDelayField();

async function loadEvents() {
  const response = await fetch("/api/operations", { headers: { Accept: "application/json" } });
  const payload = await response.json();
  const select = form.elements.event_id;
  const previous = select.value;
  select.innerHTML = (payload.operations || [])
    .map(item => `<option value="${escapeHTML(item.event_id)}">${escapeHTML(item.name)}（${escapeHTML(item.phase)}）</option>`)
    .join("");
  if (previous) select.value = previous;
}

form.addEventListener("submit", async event => {
  event.preventDefault();
  const data = {
    event_id: form.elements.event_id.value,
    action: actionSelect.value,
    initiator: form.elements.initiator.value.trim(),
    ttl_millis: Number(form.elements.ttl_seconds.value) * 1000,
  };
  if (actionSelect.value === "schedule-failover") {
    data.parameters = { delay_millis: Number(form.elements.delay_millis.value) };
  }
  submitMsg.textContent = "提交中…";
  try {
    const response = await fetch("/api/approvals", {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify(data),
    });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || `HTTP ${response.status}`);
    submitMsg.textContent = `已创建批准 ${payload.id}`;
    form.elements.initiator.value = "";
    refresh();
  } catch (err) {
    submitMsg.textContent = `提交失败：${err.message}`;
  }
});

async function decide(id, decision, confirmer, reason) {
  const response = await fetch(`/api/approvals/${encodeURIComponent(id)}/decision`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({ confirmer, decision, reason }),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok && !payload.id) {
    alert(payload.error || `HTTP ${response.status}`);
  }
  refresh();
}

function timelineHTML(item) {
  if (!item.timeline || !item.timeline.length) return "";
  return `<details class="timeline"><summary>时间线（${item.timeline.length}）</summary><ul>${
    item.timeline.map(entry => `<li><strong>${escapeHTML(entry.kind)}</strong> · ${escapeHTML(entry.actor || "系统")} · ${
      new Date(entry.at).toLocaleString()}${entry.detail ? ` — ${escapeHTML(entry.detail)}` : ""}</li>`).join("")
  }</ul></details>`;
}

function decisionHTML(item) {
  if (item.status !== "pending") return "";
  return `<div class="decision">
<input name="confirmer" maxlength="64" placeholder="确认人工号/姓名（须与发起人不同）">
<input name="reason" maxlength="200" placeholder="理由（拒绝时必填建议）">
<button type="button" data-decision="approve">确认执行</button>
<button type="button" class="danger" data-decision="reject">拒绝</button>
</div>`;
}

function card(item) {
  const params = item.parameters && Object.keys(item.parameters).length
    ? JSON.stringify(item.parameters) : "—";
  return `<article class="item approval status-${escapeHTML(item.status)}">
<h2>${escapeHTML(ACTION_LABELS[item.action] || item.action)}</h2>
<dl>
<dt>批准单号</dt><dd>${escapeHTML(item.id)}</dd>
<dt>节目</dt><dd>${escapeHTML(item.event_name || item.event_id)}</dd>
<dt>状态</dt><dd>${escapeHTML(item.status)}</dd>
<dt>发起人</dt><dd>${escapeHTML(item.initiator)}</dd>
<dt>确认人</dt><dd>${escapeHTML(item.confirmer || "—")}</dd>
<dt>预期节目代次</dt><dd>${escapeHTML(item.expected_generations.program_revision)}</dd>
<dt>预期信号代次</dt><dd>${escapeHTML(item.expected_generations.source_generation)}</dd>
<dt>预期命令代次</dt><dd>${escapeHTML(item.expected_generations.command_epoch)}</dd>
<dt>参数</dt><dd>${escapeHTML(params)}</dd>
<dt>提交时间</dt><dd>${new Date(item.submitted_at).toLocaleString()}</dd>
<dt>截止时间</dt><dd>${new Date(item.expires_at).toLocaleString()}</dd>
</dl>
${decisionHTML(item)}
${timelineHTML(item)}
</article>`;
}

async function refresh() {
  try {
    await loadEvents();
    const response = await fetch("/api/approvals", { headers: { Accept: "application/json" } });
    const payload = await response.json();
    const list = payload.approvals || [];
    content.innerHTML = list.length ? list.map(card).join("") : `<p class="empty">当前没有批准记录</p>`;
    stamp.textContent = `状态 ${response.status} · ${new Date().toLocaleTimeString()}`;
  } catch (err) {
    content.innerHTML = `<p class="empty">服务连接失败 ${escapeHTML(err.message)}</p>`;
    stamp.textContent = "连接已中断";
  }
}

content.addEventListener("click", event => {
  const button = event.target.closest("button[data-decision]");
  if (!button) return;
  const article = button.closest("article.approval");
  const id = article.querySelector("dl dd").textContent;
  const confirmer = article.querySelector('input[name="confirmer"]').value.trim();
  const reason = article.querySelector('input[name="reason"]').value.trim();
  if (!confirmer) {
    alert("请填写确认人身份");
    return;
  }
  if (button.dataset.decision === "reject" && !reason) {
    alert("拒绝时请填写理由");
    return;
  }
  decide(id, button.dataset.decision, confirmer, reason);
});

refresh();
setInterval(refresh, 5000);

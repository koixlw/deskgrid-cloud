// 共享算力 - 总控面板前端
const state = {
  workers: new Map(),
  tasks: new Map(),
  stats: { completed: 0, failed: 0 }, // lifetime counters from gateway; survives task cleanup / eviction
  expanded: new Set(), // task IDs that are expanded
  filter: 'all',
  ws: null,
  authToken: null,
  authRequired: false,
};

const $ = (s) => document.querySelector(s);
const $$ = (s) => Array.from(document.querySelectorAll(s));

// ---------- Auth ----------
function authHeaders() {
  return state.authToken ? { 'Authorization': `Bearer ${state.authToken}` } : {};
}

function authUrl(url) {
  if (!state.authToken) return url;
  const sep = url.includes('?') ? '&' : '?';
  return `${url}${sep}token=${encodeURIComponent(state.authToken)}`;
}

async function authedFetch(url, opts = {}) {
  const headers = { ...(opts.headers || {}), ...authHeaders() };
  const r = await fetch(url, { ...opts, headers });
  if (r.status === 401 && state.authToken) {
    // token rejected; reset and show banner
    state.authToken = null;
    localStorage.removeItem('deskgrid_token');
    if (state.ws) { try { state.ws.close(); } catch (e) {} state.ws = null; }
    showAuthBanner();
    alert('令牌无效，请重新设置');
  }
  return r;
}

async function initAuth() {
  try {
    const r = await fetch('/api/auth-check');
    const d = await r.json();
    if (!d.required) {
      state.authRequired = false;
      hideAuthBanner();
      return;
    }
    state.authRequired = true;
    showAuthBanner();
    const stored = localStorage.getItem('deskgrid_token');
    if (stored) {
      state.authToken = stored;
      // verify token works
      const test = await authedFetch('/api/workers');
      if (test.ok) {
        hideAuthBanner();
        return;
      }
      state.authToken = null;
      localStorage.removeItem('deskgrid_token');
    }
  } catch (e) {
    console.error('auth init failed', e);
  }
}

function showAuthBanner() {
  const el = $('#auth-banner');
  if (el) el.hidden = false;
}

function hideAuthBanner() {
  const el = $('#auth-banner');
  if (el) el.hidden = true;
}

$('#auth-set').addEventListener('click', () => {
  const t = prompt('输入访问令牌：');
  if (t && t.trim()) {
    localStorage.setItem('deskgrid_token', t.trim());
    state.authToken = t.trim();
    location.reload();
  }
});

$('#auth-clear').addEventListener('click', () => {
  localStorage.removeItem('deskgrid_token');
  state.authToken = null;
  location.reload();
});

// ---------- WebSocket ----------
function connect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = authUrl(`${proto}//${location.host}/ws`);
  state.ws = new WebSocket(url);

  state.ws.onopen = () => {
    setConnStatus(true);
  };
  state.ws.onclose = () => {
    setConnStatus(false);
    setTimeout(connect, 2000);
  };
  state.ws.onerror = () => {
    setConnStatus(false);
  };
  state.ws.onmessage = (ev) => {
    try {
      const msg = JSON.parse(ev.data);
      handleEvent(msg);
    } catch (e) {
      console.error('ws parse error', e);
    }
  };
}

function setConnStatus(connected) {
  const el = $('#conn-status');
  if (connected) {
    el.classList.add('connected');
    el.querySelector('.label').textContent = '实时已连接';
  } else {
    el.classList.remove('connected');
    el.querySelector('.label').textContent = '连接断开，重连中...';
  }
}

function handleEvent(msg) {
  const { event, payload } = msg;
  switch (event) {
    case 'init':
      state.workers.clear();
      state.tasks.clear();
      (payload.workers || []).forEach((w) => state.workers.set(w.id, w));
      (payload.tasks || []).forEach((t) => state.tasks.set(t.id, t));
      if (payload.stats) state.stats = { ...state.stats, ...payload.stats };
      render();
      break;
    case 'worker_added':
    case 'worker_updated':
      state.workers.set(payload.id, payload);
      renderWorkers();
      renderKPIs();
      break;
    case 'worker_removed':
      state.workers.set(payload.id, payload);
      renderWorkers();
      renderKPIs();
      break;
    case 'worker_deleted':
      state.workers.delete(payload.id);
      renderWorkers();
      renderKPIs();
      break;
    case 'task_added':
      state.tasks.set(payload.id, payload);
      renderTasks();
      renderKPIs();
      break;
    case 'task_updated':
      state.tasks.set(payload.id, payload);
      renderTasks();
      renderKPIs();
      break;
    case 'task_done':
      state.tasks.set(payload.id, payload);
      renderTasks();
      renderKPIs();
      break;
    case 'task_deleted':
      state.tasks.delete(payload.id);
      state.expanded.delete(payload.id);
      renderTasks();
      renderKPIs();
      break;
    case 'stats_updated':
      // Gateway pushes this whenever a task transitions to done/failed.
      // Lifetime counters — task_done/task_deleted events don't move them.
      state.stats = { ...state.stats, ...payload };
      renderKPIs();
      break;
  }
}

// ---------- Rendering ----------
function render() {
  renderKPIs();
  renderWorkers();
  renderTasks();
}

function renderKPIs() {
  const workers = Array.from(state.workers.values());
  const tasks = Array.from(state.tasks.values());
  const online = workers.filter((w) => w.status !== 'offline').length;
  const running = tasks.filter((t) => t.status === 'running').length;
  // 累计完成 / 失败任务 come from gateway-side lifetime counters, not the
  // current in-memory task list: a deleted or evicted task still counts.
  $('#kpi-workers').textContent = online;
  $('#kpi-running').textContent = running;
  $('#kpi-done').textContent = state.stats.completed || 0;
  $('#kpi-failed').textContent = state.stats.failed || 0;
}

function renderWorkers() {
  const grid = $('#workers-grid');
  const workers = Array.from(state.workers.values())
    .sort((a, b) => {
      if (a.status === 'offline' && b.status !== 'offline') return 1;
      if (a.status !== 'offline' && b.status === 'offline') return -1;
      return a.type.localeCompare(b.type) || a.name.localeCompare(b.name);
    });
  if (workers.length === 0) {
    grid.innerHTML = '<div class="empty">暂无在线节点，请先启动 worker</div>';
    return;
  }
  const tpl = $('#worker-card-tpl');
  grid.innerHTML = '';
  workers.forEach((w) => {
    const node = tpl.content.firstElementChild.cloneNode(true);
    node.dataset.status = w.status;
    node.querySelector('.worker-name').textContent = w.name || w.id.slice(0, 8);
    node.querySelector('.worker-type').textContent = w.type;
    node.querySelector('.worker-status').textContent = w.status;
    node.querySelector('.worker-cpu').textContent = `${w.cpu || '未知CPU'} · ${w.cores || 0}核 · ${formatMB(w.memory_mb)}`;
    setMeter(node.querySelector('.meter-cpu'), node.querySelector('.meter-cpu-v'), w.cpu_usage);
    setMeter(node.querySelector('.meter-mem'), node.querySelector('.meter-mem-v'), w.mem_usage);
    node.querySelector('.worker-queue').textContent = w.queue_len || 0;
    node.querySelector('.worker-done').textContent = w.tasks_done || 0;
    const delBtn = node.querySelector('.worker-delete');
    delBtn.addEventListener('click', async (e) => {
      e.stopPropagation();
      const label = w.name || w.id.slice(0, 8);
      if (!confirm(`确认删除节点 "${label}" 吗？\n\n远端进程不会停止，仍可能自动重新上线。`)) return;
      delBtn.disabled = true;
      try {
        const r = await authedFetch(`/api/workers/${w.id}`, { method: 'DELETE' });
        if (!r.ok) {
          const err = await r.json().catch(() => ({}));
          alert(`删除失败: ${err.error || r.status}`);
          delBtn.disabled = false;
        }
      } catch (err) {
        alert('请求失败: ' + err.message);
        delBtn.disabled = false;
      }
    });
    grid.appendChild(node);
  });
}

function setMeter(fillEl, valEl, pct) {
  const v = Math.max(0, Math.min(100, +(pct || 0)));
  fillEl.style.width = v.toFixed(1) + '%';
  valEl.textContent = v.toFixed(1) + '%';
  // color shift based on load
  if (v > 80) fillEl.style.background = 'linear-gradient(90deg, #f85149, #d29922)';
  else if (v > 50) fillEl.style.background = 'linear-gradient(90deg, #d29922, #58a6ff)';
  else fillEl.style.background = '';
}

function formatMB(mb) {
  if (!mb) return '?MB';
  if (mb >= 1024) return (mb / 1024).toFixed(1) + 'GB';
  return mb + 'MB';
}

function renderTasks() {
  const list = $('#task-list');
  let tasks = Array.from(state.tasks.values());
  if (state.filter !== 'all') {
    tasks = tasks.filter((t) => t.status === state.filter);
  }
  // sort by created desc
  tasks.sort((a, b) => new Date(b.created_at) - new Date(a.created_at));
  if (tasks.length === 0) {
    list.innerHTML = '<div class="empty">暂无任务</div>';
    return;
  }
  const tpl = $('#task-item-tpl');
  list.innerHTML = '';
  tasks.forEach((t) => {
    const node = tpl.content.firstElementChild.cloneNode(true);
    node.dataset.status = t.status;
    node.dataset.taskId = t.id;
    node.querySelector('.task-type').textContent = `${t.type}  #${t.id.slice(0, 8)}`;
    node.querySelector('.task-status').textContent = t.status;
    node.querySelector('.task-time').textContent = formatTime(t.created_at);
    const progress = t.progress || 0;
    node.querySelector('.task-progress-fill').style.width = progress + '%';
    node.querySelector('.task-progress-val').textContent = progress + '%';
    node.querySelector('.task-payload').textContent = t.payload || '';

    const logs = t.logs || [];
    node.querySelector('.task-logs').textContent = logs.join('\n') || '(暂无日志)';

    const resultSection = node.querySelector('.task-result-section');
    const errorSection = node.querySelector('.task-error-section');
    if (t.status === 'done' && t.result) {
      resultSection.hidden = false;
      const resultEl = node.querySelector('.task-result');
      try {
        const parsed = JSON.parse(t.result);
        resultEl.textContent = JSON.stringify(parsed, null, 2);
        // also render any media referenced in the result
        const media = renderMediaFromResult(parsed);
        if (media) {
          resultEl.parentNode.insertBefore(media, resultEl.nextSibling);
        }
      } catch (e) {
        resultEl.textContent = t.result;
      }
    }
    if (t.status === 'failed' && t.error) {
      errorSection.hidden = false;
      node.querySelector('.task-error').textContent = t.error;
    }

    if (state.expanded.has(t.id)) {
      node.classList.add('open');
      node.querySelector('.task-body').hidden = false;
    }

    node.querySelector('.task-head').addEventListener('click', () => {
      const body = node.querySelector('.task-body');
      const open = node.classList.toggle('open');
      body.hidden = !open;
      if (open) state.expanded.add(t.id);
      else state.expanded.delete(t.id);
    });

    // Cancel button: only enabled while pending/running, posts to
    // gateway which forwards to the worker via pushCancelToWorker.
    const cancelBtn = node.querySelector('.task-cancel');
    const canCancel = t.status === 'pending' || t.status === 'running';
    cancelBtn.disabled = !canCancel;
    if (!canCancel) cancelBtn.title = '任务已结束，无需中断';
    cancelBtn.addEventListener('click', async (ev) => {
      ev.stopPropagation();
      if (!t.worker_id) { alert('该任务尚未派发给 worker'); return; }
      if (!confirm(`确认中断任务 ${t.id.slice(0, 8)} ？`)) return;
      cancelBtn.disabled = true;
      try {
        const r = await authedFetch(`/api/workers/${t.worker_id}/task/cancel`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ task_id: t.id }),
        });
        if (!r.ok) {
          const msg = await r.text().catch(() => '');
          alert(`中断失败 (${r.status}): ${msg || r.statusText}`);
          cancelBtn.disabled = false;
        }
      } catch (e) {
        alert(`中断失败: ${e}`);
        cancelBtn.disabled = false;
      }
    });

    // Delete button: removes the task from gateway store. If the task
    // is still running, the gateway also pushes a cancel to the worker.
    const deleteBtn = node.querySelector('.task-delete');
    deleteBtn.addEventListener('click', async (ev) => {
      ev.stopPropagation();
      const verb = canCancel ? '中断并删除' : '删除';
      if (!confirm(`确认${verb}任务 ${t.id.slice(0, 8)} ？此操作不可恢复。`)) return;
      deleteBtn.disabled = true;
      try {
        const r = await authedFetch(`/api/tasks/${t.id}`, { method: 'DELETE' });
        if (!r.ok) {
          const msg = await r.text().catch(() => '');
          alert(`删除失败 (${r.status}): ${msg || r.statusText}`);
          deleteBtn.disabled = false;
        }
        // On success, the gateway emits task_deleted over WebSocket
        // and the item drops out of the list via handleEvent().
      } catch (e) {
        alert(`删除失败: ${e}`);
        deleteBtn.disabled = false;
      }
    });

    list.appendChild(node);
  });
}

function formatTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  const now = new Date();
  const diff = (now - d) / 1000;
  if (diff < 60) return Math.floor(diff) + '秒前';
  if (diff < 3600) return Math.floor(diff / 60) + '分钟前';
  return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

// ---------- Form ----------
// Detect media paths in a parsed task result and produce a preview element.
function renderMediaFromResult(parsed) {
  if (!parsed || typeof parsed !== 'object') return null;
  const artsDir = 'E:\\deskgrid-cloud\\artifacts\\';
  const artsDirAlt = 'E:/deskgrid-cloud/artifacts/';

  function toUrl(p) {
    if (!p) return null;
    const norm = p.replace(/\\/g, '/');
    const idx = norm.lastIndexOf('/artifacts/');
    if (idx >= 0) {
      return '/artifacts/' + norm.substring(idx + '/artifacts/'.length);
    }
    if (norm.includes(artsDirAlt)) {
      return '/artifacts/' + norm.substring(norm.indexOf(artsDirAlt) + artsDirAlt.length);
    }
    return null;
  }

  const wrap = document.createElement('div');
  wrap.className = 'task-media';
  let added = false;
  for (const k of ['image_path', 'audio_path']) {
    const url = toUrl(parsed[k]);
    if (!url) continue;
    if (k === 'image_path') {
      const img = document.createElement('img');
      img.src = authUrl(url);
      img.alt = parsed.prompt || 'generated';
      img.className = 'task-media-img';
      wrap.appendChild(img);
      added = true;
    } else if (k === 'audio_path') {
      const a = document.createElement('audio');
      a.src = authUrl(url);
      a.controls = true;
      a.className = 'task-media-audio';
      wrap.appendChild(a);
      added = true;
    }
  }
  const vurl = toUrl(parsed.video_path);
  if (vurl) {
    const v = document.createElement('video');
    v.src = authUrl(vurl);
    v.controls = true;
    v.className = 'task-media-video';
    wrap.appendChild(v);
    added = true;
  }
  return added ? wrap : null;
}
// Per-type scheduling hints (capabilities + min resources). Mirrors
// `DEFAULT_REQUIREMENTS_BY_TYPE` in client_wrapper.py — keeping the two in
// lockstep is a Phase 1 invariant: if you add a new task type, add it here
// and there.
const REQUIREMENTS_BY_TYPE = {
  code_format:            { require_caps: [],                  min_cores: 1,  min_memory_mb: 1024 },
  code_ast:               { require_caps: [],                  min_cores: 1,  min_memory_mb: 1024 },
  doc_summary:            { require_caps: [],                  min_cores: 1,  min_memory_mb: 2048 },
  doc_wordfreq:           { require_caps: [],                  min_cores: 1,  min_memory_mb: 1024 },
  csv_montecarlo:         { require_caps: [],                  min_cores: 2,  min_memory_mb: 1024 },
  csv_stats:              { require_caps: [],                  min_cores: 1,  min_memory_mb: 512  },
  image_txt2img:          { require_caps: ['pillow'],          min_gpu_free_mb: 4096 },
  image_img2img:          { require_caps: ['pillow'],          min_gpu_free_mb: 4096 },
  image_inpaint:          { require_caps: ['pillow'],          min_gpu_free_mb: 4096 },
  image_controlnet:       { require_caps: ['pillow','model:controlnet'], min_gpu_free_mb: 6144 },
  audio_tts_line:         { require_caps: ['edge-tts'],        min_gpu_free_mb: 1024 },
  audio_tts_with_emotion: { require_caps: ['edge-tts'],        min_gpu_free_mb: 2048 },
  video_assemble_clip:    { require_caps: ['ffmpeg'],          min_gpu_free_mb: 4096 },
  video_add_subtitle:     { require_caps: ['ffmpeg','python'], min_gpu_free_mb: 2048 },
  video_kenburns:         { require_caps: ['ffmpeg'],          min_gpu_free_mb: 4096 },
};

function readRequirementsFromForm() {
  // Returns {} when all 4 fields are empty (= backwards-compatible: no hard filter).
  const caps = ($('#req-caps')?.value || '').trim();
  const cores = parseInt($('#req-cores')?.value || '', 10);
  const mem = parseInt($('#req-mem')?.value || '', 10);
  const gpu = parseInt($('#req-gpu')?.value || '', 10);
  const req = {};
  if (caps) req.require_caps = caps.split(',').map((s) => s.trim()).filter(Boolean);
  if (Number.isFinite(cores) && cores > 0) req.min_cores = cores;
  if (Number.isFinite(mem)   && mem   > 0) req.min_memory_mb = mem;
  if (Number.isFinite(gpu)   && gpu   > 0) req.min_gpu_free_mb = gpu;
  return req;
}

function applyRequirementsToForm(req) {
  $('#req-caps').value  = (req.require_caps    || []).join(', ');
  $('#req-cores').value = (req.min_cores       && req.min_cores       > 0) ? req.min_cores : '';
  $('#req-mem').value   = (req.min_memory_mb   && req.min_memory_mb   > 0) ? req.min_memory_mb : '';
  $('#req-gpu').value   = (req.min_gpu_free_mb && req.min_gpu_free_mb > 0) ? req.min_gpu_free_mb : '';
}

$('#task-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const type = $('#task-type').value;
  const payload = $('#task-payload').value.trim();
  if (!payload) {
    alert('请填写 payload');
    return;
  }
  const requirements = readRequirementsFromForm();
  const btn = e.target.querySelector('.btn-primary');
  btn.disabled = true;
  btn.textContent = '下发中...';
  try {
    const body = { type, payload, source: 'panel' };
    if (Object.keys(requirements).length > 0) body.requirements = requirements;
    const r = await authedFetch('/api/tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      const err = await r.json();
      alert('下发失败: ' + (err.error || r.status));
    } else {
      const task = await r.json();
      state.expanded.add(task.id);
      // reset form payload only
      $('#task-payload').value = '';
    }
  } catch (err) {
    alert('请求失败: ' + err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = '下发到最优节点';
  }
});

$('#load-template').addEventListener('click', () => {
  const type = $('#task-type').value;
  const templates = {
    code_format: 'def f(x):\n  return x*2',
    code_ast: 'class A:\n  def m1(self):\n    return 1\n  def m2(self):\n    for i in range(10):\n      print(i)',
    doc_summary: '人工智能是计算机科学的一个分支。它企图了解智能的实质，并生产出一种新的能以人类智能相似的方式做出反应的智能机器。该领域的研究包括机器人、语言识别、图像识别、自然语言处理和专家系统等。',
    doc_wordfreq: '今天天气很好，今天我们去公园玩。公园里有很多花，很多树，还有很多小朋友在玩耍。今天真开心！',
    csv_montecarlo: '{"samples": 100000, "distribution": "normal", "mean": 0, "stddev": 1}',
    csv_stats: '[1.5, 2.3, 3.7, 4.1, 5.9, 6.2, 7.8, 8.4, 9.0, 10.5, 11.2, 12.8, 13.3, 14.7, 15.1]',
    image_txt2img: 'a cute orange cat sitting on a wooden floor, soft sunlight',
    image_img2img: '{"prompt": "oil painting style", "strength": 0.6}',
    image_inpaint: '{"prompt": "a window with mountains view", "image_path": ""}',
    image_controlnet: '{"prompt": "character pose reference", "image_path": ""}',
    audio_tts_line: '今天天气真好，我们一起去公园吧。',
    audio_tts_with_emotion: '{"text": "The quick brown fox jumps over the lazy dog.", "emotion": "happy"}',
    video_assemble_clip: '{"scenes": ["开场：朝阳", "发展：主角登场", "高潮：追逐", "结尾：和解"], "fps": 8, "sec_per_scene": 1.5}',
    video_add_subtitle: '{"scenes": ["Hello", "World"], "fps": 8, "sec_per_scene": 1.5}',
    video_kenburns: '{"prompt": "ancient castle on a hill", "fps": 12, "duration": 3}',
  };
  $('#task-payload').value = templates[type] || '';
  // Mirror the per-type template into the advanced fields so the operator
  // sees the defaults and can tweak before submitting.
  applyRequirementsToForm(REQUIREMENTS_BY_TYPE[type] || {});
});

// When the user manually changes the task type, only update payload; the
// advanced requirements block stays as-is so a manual override isn't clobbered.
$('#task-type').addEventListener('change', () => {
  // intentionally empty — payload loads via the explicit "载入模板" button.
});

$$('.filter').forEach((btn) => {
  btn.addEventListener('click', () => {
    $$('.filter').forEach((b) => b.classList.remove('active'));
    btn.classList.add('active');
    state.filter = btn.dataset.filter;
    renderTasks();
  });
});

// Periodic refresh fallback (if WS disconnected for a while)
setInterval(async () => {
  if (!state.ws || state.ws.readyState !== 1) {
    try {
      const [wr, tr, st] = await Promise.all([
        authedFetch('/api/workers').then((r) => r.json()),
        authedFetch('/api/tasks').then((r) => r.json()),
        authedFetch('/api/stats').then((r) => r.json()),
      ]);
      state.workers.clear();
      state.tasks.clear();
      wr.forEach((w) => state.workers.set(w.id, w));
      tr.forEach((t) => state.tasks.set(t.id, t));
      if (st && typeof st.completed === 'number') state.stats = { ...state.stats, ...st };
      render();
    } catch (e) { /* ignore */ }
  }
}, 10000);

// ---------- Boot ----------
(async function () {
  await initAuth();
  connect();
  render();
})();

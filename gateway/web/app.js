// 共享算力 - 总控面板前端
const state = {
  workers: new Map(),
  tasks: new Map(),
  expanded: new Set(), // task IDs that are expanded
  filter: 'all',
  ws: null,
};

const $ = (s) => document.querySelector(s);
const $$ = (s) => Array.from(document.querySelectorAll(s));

// ---------- WebSocket ----------
function connect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = `${proto}//${location.host}/ws`;
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
  const done = tasks.filter((t) => t.status === 'done').length;
  const failed = tasks.filter((t) => t.status === 'failed').length;
  $('#kpi-workers').textContent = online;
  $('#kpi-running').textContent = running;
  $('#kpi-done').textContent = done;
  $('#kpi-failed').textContent = failed;
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
      try {
        const parsed = JSON.parse(t.result);
        node.querySelector('.task-result').textContent = JSON.stringify(parsed, null, 2);
      } catch (e) {
        node.querySelector('.task-result').textContent = t.result;
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
$('#task-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const type = $('#task-type').value;
  const payload = $('#task-payload').value.trim();
  if (!payload) {
    alert('请填写 payload');
    return;
  }
  const btn = e.target.querySelector('.btn-primary');
  btn.disabled = true;
  btn.textContent = '下发中...';
  try {
    const r = await fetch('/api/tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type, payload, source: 'panel' }),
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
  };
  $('#task-payload').value = templates[type] || '';
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
      const [wr, tr] = await Promise.all([
        fetch('/api/workers').then((r) => r.json()),
        fetch('/api/tasks').then((r) => r.json()),
      ]);
      state.workers.clear();
      state.tasks.clear();
      wr.forEach((w) => state.workers.set(w.id, w));
      tr.forEach((t) => state.tasks.set(t.id, t));
      render();
    } catch (e) { /* ignore */ }
  }
}, 10000);

connect();
render();

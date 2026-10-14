# deskgrid-cloud — 内网分布式算力调度系统

> 把办公室内多台电脑的空闲 CPU / 内存聚合起来，零成本为企业提供分布式算力。本系统为"硬件交互"赛道的完整实现。

## 项目结构

```
deskgrid-cloud/
├── README.md                       # 本文档
├── bin/
│   └── gateway.exe                 # Go 编译产物（13MB 静态二进制）
├── gateway/                        # 网关（Go + Gin + gorilla/websocket）
│   ├── main.go                     # 入口 + 嵌入式静态文件服务
│   ├── go.mod
│   ├── internal/
│   │   ├── store.go                # 内存存储（workers / tasks / results）
│   │   ├── server.go               # HTTP 路由 + CORS + 任务派发
│   │   ├── ws.go                   # WebSocket Hub
│   │   ├── scheduler.go            # 评分调度（CPU+Mem+队列）
│   │   ├── worker_mgr.go           # （集成在 store.go 中）
│   │   ├── openai.go               # OpenAI 兼容 /v1/chat/completions
│   │   └── log.go
│   └── web/                        # go:embed 嵌入式面板
│       ├── index.html              # 响应式 PC + 手机
│       ├── app.js                  # WebSocket + REST
│       └── style.css               # 深色 GitHub 风格主题
├── workers/                        # 3 类 Python Worker（FastAPI + psutil）
│   ├── common/
│   │   ├── base.py                 # BaseWorker 基类
│   │   ├── hardware.py             # psutil 真实硬件采集
│   │   └── reporter.py             # 进度上报
│   ├── code/                       # 代码算力节点（端口 3001）
│   │   ├── main.py
│   │   └── tasks.py                # code_format（AST unparse）/ code_ast（静态分析）
│   ├── doc/                        # 文档算力节点（端口 3002）
│   │   ├── main.py
│   │   └── tasks.py                # doc_summary（TextRank）/ doc_wordfreq
│   └── csv/                        # CSV 算力节点（端口 3003）
│       ├── main.py
│       └── tasks.py                # csv_montecarlo / csv_stats
├── scripts/
│   ├── install_go.ps1              # Go 1.23.4 安装脚本
│   ├── start_all.bat               # 一键启动（推荐）
│   ├── start_gateway.bat           # 单启网关
│   ├── start_worker_code.bat       # 单启 code worker
│   ├── start_worker_doc.bat        # 单启 doc worker
│   ├── start_worker_csv.bat        # 单启 csv worker
│   ├── build_worker_exe.bat        # 把 worker 打成一个 .exe（给同事直接拷贝）
│   └── stop_all.bat                # 一键停止
├── worker_launcher.py              # 通用 worker 启动器（被打成 .exe 的入口）
├── worker.conf.example             # .exe 用的配置模板
├── dist/                           # 打包后产物：deskgrid-worker.exe + README.txt
└── logs/                           # 运行时日志
```

## 端口规划

| 服务 | 端口 | 说明 |
|---|---|---|
| Gateway（网关） | **13000** | REST + WebSocket + 静态面板 + OpenAI API |
| Code Worker | 3001 | Python AST 格式化 / 静态分析 |
| Doc Worker | 3002 | 文本摘要 / 词频 |
| CSV Worker | 3003 | 蒙特卡洛模拟 / 描述统计 |

## 快速开始（Windows）

### 1. 启动服务（一键）

```bat
scripts\start_all.bat
```

输出：
```
[start_all] launching gateway on :13000 ...
[start_all] waiting for gateway to be ready...
[start_all] gateway ready.
[start_all] launching 3 workers ...

============================================
 共享算力已启动
   总控面板:  http://localhost:13000/
   OpenAI API: http://localhost:13000/v1/chat/completions
   Code 节点:  http://localhost:3001
   Doc  节点:  http://localhost:3002
   CSV  节点:  http://localhost:3003
 日志目录:     E:\deskgrid-cloud\logs\
 停止所有:     scripts\stop_all.bat
============================================
```

### 2. 访问面板

浏览器打开 `http://localhost:13000/` 即可看到三个在线节点的实时状态。

### 3. 停止服务

```bat
scripts\stop_all.bat
```

## 核心 API

### REST 端点

```bash
# 查看所有节点
curl http://localhost:13000/api/workers

# 查看所有任务
curl http://localhost:13000/api/tasks

# 下发任务（gateway 自动选最优节点）
curl -X POST http://localhost:13000/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"type":"code_format","payload":"def f(x):return x*2"}'

# 健康检查
curl http://localhost:13000/health
```

### OpenAI 兼容 API（TRAE 直接调用）

```bash
curl -X POST http://localhost:13000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deskgrid-cloud-gateway",
    "messages": [{"role":"user","content":"格式化代码：def f(x):return x*2"}]
  }'
```

**支持的关键词分类：**

| 任务类型 | 触发关键词 | worker |
|---|---|---|
| `code_format` | 格式化、format、代码、重构、refactor | code |
| `code_ast` | ast、静态分析 | code |
| `doc_summary` | 摘要、summary、总结 | doc |
| `doc_wordfreq` | 词频、wordfreq | doc |
| `csv_montecarlo` | 蒙特卡洛、monte carlo、模拟 | csv |
| `csv_stats` | csv、统计、stats | csv |

**模型列表：**

```bash
curl http://localhost:13000/v1/models
```

返回 `deskgrid-cloud-gateway / deskgrid-cloud-code / deskgrid-cloud-doc / deskgrid-cloud-csv` 4 个模型。

### WebSocket 实时推送

`ws://localhost:13000/ws` — 推送事件：

- `init` — 连接时全量快照
- `worker_added` / `worker_updated` / `worker_removed` — 节点变更
- `task_added` / `task_updated` / `task_done` — 任务状态变更（含 progress / logs）

每条消息格式：`{"event": "...", "payload": {...}, "time": "..."}`

## 调度算法

任务到达时，网关按以下公式对所有同类型在线节点评分，选最高分派发：

```
score = 0.5 × (100 - cpu_usage)        # CPU 越闲分越高
      + 0.3 × (100 - mem_usage)        # 内存越闲分越高
      + 0.2 × (100 - min(queue*20, 100))  # 队列越短分越高
```

评分每 5 秒心跳时刷新（节点上报实时 CPU/Mem/queue_len）。

## 故障恢复

- Worker 启动后调用 `POST /api/workers/register` 网关立即感知
- Worker 每 5 秒发心跳，10 秒未收到心跳的节点自动标记 offline
- 节点重新上线后心跳发现 404 立即重新注册
- 任务执行中 Worker 崩溃 → 任务保持 running 直到下次派发前清理（10s 内 offline 化）

## 真实硬件采集

Worker 通过 `psutil` + `platform` 读取真实信息：

```json
{
  "cpu": "Intel64 Family 6 Model 151 Stepping 2, GenuineIntel",
  "cores": 20,
  "memory_mb": 32578
}
```

实时 CPU / 内存使用率每次心跳更新，前端以进度条 + 百分比展示。

## 真实任务逻辑（无 mock）

| Worker | 任务 | 实现 |
|---|---|---|
| code | code_format | `ast.parse` + `ast.unparse` + PEP8 缩进空行规范化 |
| code | code_ast | AST 遍历统计：函数/类/循环/分支/导入/最大嵌套深度 |
| doc | doc_summary | 中英文混合分句 + TextRank（PageRank + 句子相似度） |
| doc | doc_wordfreq | 中文字符 bigram + 拉丁词分词 + 停用词 + Top-20 |
| csv | csv_montecarlo | 纯 Python 实现 uniform/normal/exponential 采样 + 95% CI |
| csv | csv_stats | 均值/方差/标准差/分位数（P25/P50/P75/P95） |

## 极简链路

```
TRAE (OpenAI client)
   │ POST /v1/chat/completions
   ▼
Gateway (Go) :13000
   │ score + dispatch (HTTP)
   ▼
┌────────┐ ┌────────┐ ┌────────┐
│Worker  │ │Worker  │ │Worker  │
│ :3001  │ │ :3002  │ │ :3003  │
│ (code) │ │ (doc)  │ │ (csv)  │
└────────┘ └────────┘ └────────┘
   │ POST /api/workers/{id}/progress
   │ POST /api/workers/{id}/result
   ▼
Gateway → WebSocket 推送给面板
```

没有 Skill 中间层，没有 MCP 协议转换，离线环境部署只需 4 个进程。

## 手机访问

同局域网手机浏览器访问 `http://<电脑IP>:13000/`，面板 CSS 自适应（< 768px 切换单列布局）。

## 受控电脑零依赖部署（单 .exe）

想给同事的电脑装一个 worker，但对方没有 Python？打成一个 exe：

```bat
scripts\build_worker_exe.bat
```

产物：`dist\deskgrid-worker.exe`（约 137 MB，PyInstaller --onefile）。

给同事拷两个文件：
- `deskgrid-worker.exe`
- `worker.conf.example`（改名 `worker.conf`）

同事在 `worker.conf` 里填：
```
TYPE=code
GATEWAY=http://主控机IP:13000
GATEWAY_HMAC_SECRET=跟主控机一样
WORKER_NAME=office-pc-02-code
```

双击 exe 即可。`--install` 注册成 Windows 计划任务（登录自启 + 失败重试）。详见 `dist\README.txt`。

## 已知约束

- **中文输入**必须使用 UTF-8 编码。Windows CMD / bash 默认 GBK，请用 PowerShell 或 Python `requests` 提交。
- 内存存储，重启 Gateway 后 Worker 需要重新注册（已自动处理：心跳 404 后自动重注册）。
- 演示项目未做鉴权，生产环境请加 token / mTLS。

### 中文 payload 必须 UTF-8

中文 / emoji 等非 ASCII 字符必须以 UTF-8 字节到达网关，否则被解析成 `U+FFFD` 替换字符。某些 worker（edge-tts）会因此直接失败。

```bash
# 正确 - PowerShell 默认 UTF-8
powershell -Command "
  \$body = '{\"type\":\"audio_tts_line\",\"payload\":\"{\\\"text\\\":\\\"你好\\\"}\"}'
  Invoke-RestMethod -Uri 'http://localhost:13000/api/tasks' -Method POST `
    -ContentType 'application/json; charset=utf-8' -Body \$body
"

# 正确 - Python requests 自动 UTF-8
python -c "import requests; r=requests.post('http://localhost:13000/api/tasks', json={'type':'audio_tts_line','payload':'{\"text\":\"你好\"}'})"

# 错误 - bash 直接 curl 中文，GBK 编码后网关替换成 U+FFFD
curl -X POST http://localhost:13000/api/tasks -d '{"type":"...","payload":"{\"text\":\"你好\"}"}'
```

## 性能参考

- Gateway 内存占用：< 30 MB
- 单个 code_format 任务：200ms（10 行 Python 代码）
- csv_montecarlo 100k 样本：~300ms
- WebSocket 推送延迟：< 50ms
- 节点心跳：每 5s 一次，开销 < 1KB/次

## License

MIT

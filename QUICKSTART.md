# 共享算力 — 快速上手（60 秒）

## 启动

```bash
cd E:\deskgrid-cloud
python scripts/start_all.py
```

等几秒看到 `all 3 worker types online (3 workers).` 就好了。
浏览器打开 **http://localhost:13000/** 看到面板即成功。

> 改动了前端或 Go 代码后，必须重启：`python scripts/start_all.py --stop && python scripts/start_all.py`
> （前端是通过 `//go:embed` 打进 `bin/gateway.exe` 的，刷新页面无效）

## 停止

```bash
python scripts/start_all.py --stop
```

## 面板三个区

1. **📡 算力节点** — 每张卡 = 一台机器的一个算力节点。CPU/内存进度条 5 秒刷新。
   卡片右上角 `×` = 从网关移除（远端进程不杀，会自动复活）。
2. **📋 任务列表** — 已派发的任务，点开看 payload / 日志 / 结果。
3. **🚀 下发任务** — 选类型 → 填 payload → 点「下发到最优节点」（按 CPU+内存+队列自动评分）。

## 内置 6 种任务

| 任务 | 输入示例 | 干什么 |
|------|---------|--------|
| `code_format` | `def f(x):return x*2` | Python 代码 AST 格式化 |
| `code_ast` | `class A:\n  def m():...` | AST 静态分析（函数/类/嵌套深度） |
| `doc_summary` | 一段中文/英文 | TextRank 摘要 |
| `doc_wordfreq` | 一段文本 | Top-20 词频 |
| `csv_montecarlo` | `{"samples":10000,"distribution":"normal","mean":0,"stddev":1}` | 蒙特卡洛模拟 |
| `csv_stats` | `[1.5,2.3,3.7,...]` 或 `10,20,30,...` | 均值/标准差/分位数 |

> 点「载入模板」按钮自动填示例。

## 直接用 curl 下发任务

```bash
curl -X POST http://localhost:13000/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"type":"code_format","payload":"def f(x):return x*2"}'
```

返回 task id，过几秒 `curl http://localhost:13000/api/tasks/<id>` 看结果。

## 加一台机器（2 分钟）

在被控 PC 上：

```bash
# 1. 装依赖
pip install fastapi uvicorn requests psutil

# 2. 拷 workers/ 目录（或 git clone 整个项目）

# 3. 设置网关地址后启动（假设网关在 192.168.1.10）
set GATEWAY=http://192.168.1.10:13000
set PORT=3001
set WORKER_NAME=office-pc-02-code
python workers\code\main.py
```

启动后 5 秒内，面板上多一张新卡片 `office-pc-02-code`，无需重启网关。

> 一台 PC 可同时跑三种 worker（端口 3001/3002/3003 各一个进程）。
> 主控机防火墙放行 TCP 13000 入站。

## TRAE / Cursor 等 LLM 客户端接入

把 base_url 设为 `http://localhost:13000/v1`，model 选 `deskgrid-cloud-gateway`。
聊天时用关键词触发（参见 README 表格）：

- 「**格式化代码**：def f(x):return x*2」 → code_format
- 「**摘要**：今天天气很好……」 → doc_summary
- 「**蒙特卡洛**模拟正态分布 10000 个样本」 → csv_montecarlo

## 一行健康检查

```bash
curl http://localhost:13000/health
# → {"ok":true,"workers":3}
```

`workers` 数字 = 在线节点数。低于预期就检查对应端口的 worker 进程。

## 故障排查

| 现象 | 排查 |
|------|------|
| 面板卡住 | 看 `logs/gateway.out.log` 末尾 |
| 某个节点一直 offline | 看 `logs/worker-<type>.out.log`，多半是 Python 依赖没装 |
| `where go` 找不到 | `powershell -File scripts\install_go.ps1`（需要 `go.zip` 在 Temp） |
| 改了前端没生效 | 重新编译 `cd gateway && E:\Go\bin\go.exe build -o ../bin/gateway.exe .` |

## 日志位置

- `logs/gateway.out.log` — 网关
- `logs/worker-code.out.log` / `worker-doc.out.log` / `worker-csv.out.log` — 三个 worker
# deskgrid-client — 算力调度提交器

> 让你本机卡的时候，把任务甩到集群最闲的机器上。零 Python 依赖，单文件 .exe。

## 这是什么

`deskgrid-client.exe` 是 deskgrid-cloud 集群的客户端命令行包装器，模仿 `python` / `ffmpeg` 这类命令的使用方式。
它的特点是 **自动判断本机负载** —— 闲就本机跑，忙就把任务甩给集群空闲的 worker。

```
                  本机 CPU/Mem 闲                本机 CPU/Mem 忙
                       │                              │
                       ▼                              ▼
              ┌─────────────┐               ┌──────────────────┐
              │ subprocess   │               │ POST /api/tasks  │
              │ 本地透传     │               │ gateway 评分派发 │
              └─────────────┘               └──────────────────┘
                       │                              │
                       ▼                              ▼
                  直接出结果            远端 exec worker 跑 → 回传结果
```

## 部署

把这两个文件拷到同事机器：

```
deskgrid-client.exe      ← 主程序 (~15 MB，单文件)
client.conf.example      ← 改名为 client.conf
```

改 `client.conf`：

```ini
GATEWAY=http://你的主控机IP:13000
GATEWAY_TOKEN=
GATEWAY_HMAC_SECRET=
CPU_THRESHOLD=80          # 本机 CPU > 80% 触发外派
MEM_THRESHOLD=85
LOCAL_FALLBACK=1          # 集群全忙时本地跑
```

## 用法

### 智能模式（默认）

```cmd
:: 本机闲 → 本地跑；本机忙 → 提交远端
deskgrid-client.exe --cmd "python train.py --epochs 3"
deskgrid-client.exe --cmd "ffmpeg -i in.mp4 -c:v libx264 out.mp4"
deskgrid-client.exe --cmd "edge-tts --text 你好 --write-media hello.mp3"
```

### 强制本地 / 强制远端

```cmd
deskgrid-client.exe --mode local  --cmd "echo 本机跑"
deskgrid-client.exe --mode remote --cmd "echo 远端跑"
```

### 同步阻塞等结果（默认开）

```cmd
deskgrid-client.exe --watch --cmd "python long_script.py"
```

### 控制 cwd 和环境变量

```cmd
deskgrid-client.exe --cmd "python main.py" ^
    --cwd "E:\projects\my-app" ^
    --env "CUDA_VISIBLE_DEVICES=0" ^
    --env "DEBUG=1" ^
    --timeout 1800
```

## 返回值

| code | 含义 |
|---|---|
| 0 | 成功 |
| 2 | gateway 网络/拒绝错误 |
| 3 | 远端 worker 任务失败 |
| 4 | 轮询超时 |
| 64 | 参数错误 |
| 127 | 命令找不到（local 模式） |
| 其它 | 子进程本身的 exit code（透传） |

## 安全约束

> 远端 exec worker 默认拒绝：

- **cwd** 不在 `artifacts/` 或 `EXEC_ALLOWED_CWD_ROOTS` 前缀
- **env** 覆盖 `PATH` / `USERPROFILE` / `APPDATA` / `PYTHONPATH` / `WINDIR` / `OS` 等保护键
- **OpenAI 桥**调 `/v1/chat/completions` 触发 shell/exec 关键词 → HTTP 400

> 客户端这边的 `client.conf` 决定 `GATEWAY_TOKEN` / `GATEWAY_HMAC_SECRET`；
> 内网部署时这两个可以留空（dev mode）。

## 架构图

```
deskgrid-client.exe  ──HTTP──►  Gateway :13000  ──HTTP──►  Exec Worker :3007
                                (评分 + 派发)              (沙箱 + 跑命令)
```

评分公式（gateway 端）：
```
score = 0.40×(100-CPU) + 0.25×(100-Mem) + 0.10×(100-GPU) +
        0.15×(100-queue) + 0.10×(stability×100)
```
最高分 worker 拿到任务。

## 卸载

直接删两个文件即可。无注册表项，无后台服务。

## License

MIT

# 内网部署指南

本文面向**在办公室/小团队 LAN 内自托管** deskgrid-cloud 的管理员。涵盖：IP 固定、防火墙、令牌鉴权、自动备份。

> 如果是公网部署（哪怕加鉴权），**当前代码还不够**，需要先做限流、审计、加密传输、HTTPS 反代等。详见 README 的"已知约束"。

---

## 1. 主控机 IP 固定

deskgrid-cloud 网关通过 IP 寻址 Worker。如果主控机 IP 每次重启都变，工位机就连不上了。

### 方法 A：路由器 DHCP 静态分配（不动 Windows）
登录路由器后台（一般是 192.168.1.1）→ DHCP 静态地址分配 → 把主控机的 MAC 地址绑定到固定 IP。

### 方法 B：Windows 自己设固定 IP
1. 设置 → 网络 → 当前网络 → 属性 → "Internet 协议版本 4"
2. 选择"使用下面的 IP 地址"
3. 填一个**局域网内不冲突的 IP**，例如：
   - IP：`192.168.1.100`
   - 子网掩码：`255.255.255.0`
   - 默认网关：`192.168.1.1`
   - DNS：`192.168.1.1`（或 `8.8.8.8`）
4. 确认

之后路由器分配的 IP 就固定是 `192.168.1.100`。

---

## 2. DNS 友好名（可选）

如果你希望同事输入 `deskgrid.local` 而不是 IP：

### 在主控机上
1. 安装 [Bonjour Print Services](https://support.apple.com/bonjour/)（Windows 版的 mDNS / ZeroConf）
2. 或者装 [Acrylic DNS](https://github.com/AcrylicDNS/AcrylicDNS) 自己当 DNS 服务器

### 在公司路由器（如果支持）
某些路由器（OpenWrt、爱快、RouterOS）支持自定义 LAN DNS 条目，添加：
```
deskgrid.local → 192.168.1.100
```

之后同事浏览器输入 `http://deskgrid.local:13000/` 就能访问。

---

## 3. Windows 防火墙只放内网

主控机和被控机都建议把端口限制为内网访问。

### 用脚本一键配（PowerShell 管理员）：

`scripts\setup_firewall.ps1`：
```powershell
# 仅允许 192.168.1.0/24 网段访问 13000 端口
New-NetFirewallRule -DisplayName "deskgrid-gateway (LAN only)" `
    -Direction Inbound -Protocol TCP -LocalPort 13000 `
    -RemoteAddress 192.168.1.0/24 -Action Allow

# 仅允许主控机访问 worker 端口
New-NetFirewallRule -DisplayName "deskgrid-worker-code" `
    -Direction Inbound -Protocol TCP -LocalPort 3001 `
    -RemoteAddress 192.168.1.100 -Action Allow

New-NetFirewallRule -DisplayName "deskgrid-worker-doc" `
    -Direction Inbound -Protocol TCP -LocalPort 3002 `
    -RemoteAddress 192.168.1.100 -Action Allow

New-NetFirewallRule -DisplayName "deskgrid-worker-csv" `
    -Direction Inbound -Protocol TCP -LocalPort 3003 `
    -RemoteAddress 192.168.1.100 -Action Allow
```

跑一次即可。如要删除：
```powershell
Remove-NetFirewallRule -DisplayName "deskgrid-*"
```

### 验证
从一台非白名单 IP 测试（手机切 4G、用 VPN 等）：
```
telnet 192.168.1.100 13000  # 应该连不上 / 超时
```

---

## 4. 启用 Token 鉴权（即使内网也建议）

### 生成一个随机 token
PowerShell：
```powershell
-join ( (1..32 | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }) )
```
或者 Python：
```bash
python -c "import secrets; print(secrets.token_hex(16))"
```

输出 32 字符的 hex，类似 `a1b2c3d4e5f6...`。**这就是你的令牌**，值发安全渠道给同事。

### 配置 token

**选项 A：环境变量**（推荐，配置后所有路径都生效）

`scripts\run_gateway.cmd` 里加一行：
```bat
set GATEWAY_TOKEN=a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4
```

同样的方式加到 3 个 `run_worker_*.cmd`，以及启动 Python worker 的环境变量（其实 worker 不需要 token，只需要网关信任自己 — 网关到 worker 走内网默认信任）。

**选项 B：临时调试**
启动前手动：
```powershell
$env:GATEWAY_TOKEN = "a1b2c3d4..."
python scripts\start_all.py
```

### 验证
启动后看 `logs\gateway.out.log` 头部：
```
[INFO] auth ENABLED (token set via GATEWAY_TOKEN)
```
或：
```
[INFO] auth DISABLED (GATEWAY_TOKEN unset; dev mode)
```

### 客户端怎么用

**浏览器面板**：
打开页面 → 顶部橙色横幅 "需要访问令牌" → 点 "设置令牌" → 输入 → 自动刷新。

**curl / Python**：
```bash
curl -H "Authorization: Bearer a1b2c3d4..." http://localhost:13000/api/workers
# 或
curl "http://localhost:13000/api/workers?token=a1b2c3d4..."
```

**OpenAI 客户端**（TRAE / Cursor / Python openai 库）：
```python
from openai import OpenAI
client = OpenAI(
    api_key="a1b2c3d4...",   # 这里填你的 deskgrid token
    base_url="http://deskgrid.local:13000/v1",
)
```

**WebSocket**：浏览器里自动用 ?token= 查询参数，curl/wscat 也支持：
```bash
wscat -c "ws://localhost:13000/ws?token=a1b2c3d4..."
```

---

## 5. 备份计划

### 一键备份脚本（[scripts/backup.ps1](scripts/backup.ps1)）

包含：
- 备份 `data/`（任务持久化）
- 备份 `artifacts/`（最近 7 天产物）
- 备份 `logs/`（最近 7 天日志）
- 备份 `bin\gateway.exe`（防止源码重建失败时回退）
- 保留最近 30 份，超期清空
- 压缩成 zip 存到 `E:\deskgrid-cloud\backups\backup-YYYY-MM-DD.zip`

### 设置每周自动备份

PowerShell 管理员：
```powershell
$action = New-ScheduledTaskAction -Execute "powershell.exe" `
    -Argument "-ExecutionPolicy Bypass -NoProfile -File E:\deskgrid-cloud\scripts\backup.ps1"
$trigger = New-ScheduledTaskTrigger -Weekly -DaysOfWeek Sunday -At 3am
Register-ScheduledTask -TaskName "deskgrid-backup" `
    -Action $action -Trigger $trigger -User "SYSTEM" -RunLevel Highest
```

每周日凌晨 3 点跑一次，备份 7 天内的活动文件。

### 手动备份
```powershell
.\scripts\backup.ps1
```

### 恢复
把 zip 拷到目标机器解压，覆盖 `data/` `bin\gateway.exe`，启动即恢复。

---

## 6. 健康监控

### 看任务运行情况
```bash
curl http://localhost:13000/health
# {"ok":true,"workers":3}
```

### 上层定时检查 + 通知（推荐）

`scripts\health_check.ps1`（你写一个）：
```powershell
$r = Invoke-WebRequest http://localhost:13000/health -UseBasicParsing
if ($r.StatusCode -ne 200) {
    # 发通知给飞书 / 钉钉 webhook
    Invoke-RestMethod -Uri "https://open.feishu.cn/open-apis/bot/v2/hook/xxx" `
        -Method Post -Body '{"msg_type":"text","content":{"text":"deskgrid gateway down"}}' `
        -ContentType "application/json"
}
```

设置每小时跑一次，挂了能 1 小时内收到通知。

### 看 watchdog 自动重启日志
```
E:\deskgrid-cloud\logs\watchdog.log
```

每分钟一次心跳，看到 `[DOWN] port X -> restart task` 行代表某节点崩过又被自动拉起。

---

## 7. 升级流程

```bash
cd E:\deskgrid-cloud
git pull                          # 拉最新源码
python scripts\start_all.py --stop
cd gateway
E:\Go\bin\go.exe build -o ..\bin\gateway.exe .
cd ..
python scripts\start_all.py
```

升级期间约 30 秒不可用。任务持久化保证已完成的任务不丢。

---

## 部署后检查清单

- [ ] 主控机 IP 固定了
- [ ] 防火墙只放内网
- [ ] `GATEWAY_TOKEN` 设置了，日志显示 "auth ENABLED"
- [ ] `scripts\install_autostart.ps1` 跑过，开机自动启动
- [ ] `scripts\backup.ps1` 配了每周计划任务
- [ ] 健康监控 webhook 配了
- [ ] 同事知道怎么从面板设置令牌

全部 ✅ 之后这套内网部署就稳了。
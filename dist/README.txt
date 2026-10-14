deskgrid-cloud Worker — 单文件版
====================================

这个目录里的 `deskgrid-worker.exe` 是一台"被控电脑"上跑的全部东西。
不需要装 Python，不需要装任何依赖。一次拷贝，永远运行。

────────────────────────────────────
📦 拷贝给同事的最小集合：
────────────────────────────────────
   deskgrid-worker.exe     ← 主程序（~137 MB）
   worker.conf.example     ← 配置模板（复制成 worker.conf 再填）

────────────────────────────────────
🚀 三步上手
────────────────────────────────────

1) 把上面两个文件拷到目标电脑（比如 D:\deskgrid-cloud-worker\）

2) 复制 worker.conf.example 为 worker.conf，用记事本编辑：

       TYPE=code              ← 这一台跑哪种: code/doc/csv/image/audio/video
       GATEWAY=http://192.168.1.178:13000   ← 改成主控机的 IP 和端口
       GATEWAY_HMAC_SECRET=主控机给的密钥   ← 必须跟主控机一样
       WORKER_NAME=office-pc-02-code        ← 面板里显示的名字
       WORKER_HOST=0.0.0.0
       PORT=                                  ← 留空就用默认端口

3) 双击 deskgrid-worker.exe
   → 黑色窗口出现 `[run] ... on :3001 -> http://...`
   → 主控机面板 5 秒内多一张卡片，开始接活

────────────────────────────────────
🔁 开机自启动（可选，推荐）
────────────────────────────────────

在黑色窗口里运行（管理员权限的 cmd）：

       deskgrid-worker.exe --install

会创建一个 Windows 计划任务：
   - 触发器：登录时启动
   - 失败重试：每 1 分钟一次，最多 3 次
   - 不依赖窗口/界面

取消：

       deskgrid-worker.exe --uninstall

────────────────────────────────────
🛠 排查
────────────────────────────────────

看现在读到啥配置：

       deskgrid-worker.exe --show-config

面板上没出现？
   1. 主控机到这台机器的防火墙：放行 PORT（默认 3001 等）入站
   2. GATEWAY_HMAC_SECRET 跟主控机一致
   3. GATEWAY 地址能 ping 通（用浏览器试试能不能打开 http://GATEWAY/）

────────────────────────────────────
❓ 常见问题
────────────────────────────────────

Q: 一台电脑可以同时跑多种 worker 吗？
A: 可以。复制 exe 到不同目录，每个目录一份 worker.conf，
   TYPE/PORT 错开就行。例：
     D:\dg-code\deskgrid-worker.exe  (TYPE=code PORT=)
     D:\dg-doc\deskgrid-worker.exe   (TYPE=doc  PORT=)
     D:\dg-csv\deskgrid-worker.exe   (TYPE=csv  PORT=)

Q: 主控机换了 IP 怎么办？
A: 编辑 worker.conf，改 GATEWAY=... 那一行，双击 exe 重启即可。

Q: Windows Defender 提示不安全？
A: 因为没做代码签名。可以点"更多信息 → 仍要运行"。
   公司批量部署建议做代码签名证书。

────────────────────────────────────
📞 给主控机操作员的信息
────────────────────────────────────

新建一台 worker 时，主控机操作员需要提供：
   1. GATEWAY 地址       例: http://192.168.1.178:13000
   2. GATEWAY_HMAC_SECRET   例: lan-demo-shared-secret-1234567890

被控电脑操作员只需：
   1. 填好 worker.conf
   2. 双击 exe
   3. （可选）右键 exe → 以管理员身份运行 → 输入 deskgrid-worker.exe --install
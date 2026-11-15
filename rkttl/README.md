# rkttl — Rockchip TTL 串口多端共享工具

把一块 **USB 转 TTL 串口适配器**（如 CH340）插在本地电脑上，通过
**网页控制台 / HTTP API / TCP 桥** 把串口共享给远端服务器 ——
**本地与远端可以同时收发同一串口**，效果类似 `tmux` 的多端 attach：

```
                        ┌──────────────────────────────┐
   USB 转 TTL (CH340)   │  rkttl（串口宿主机）          │
   ────────────────────►│  · rkttl-gui.exe  Windows GUI │
        COM3 @1500000   │  · rkttl serve    Linux CLI   │
                        └───────┬──────────────┬───────┘
                                │ HTTP+SSE      │ 裸 TCP
              ┌─────────────────┴───┐   ┌──────┴──────────────┐
              │ 远端服务器           │   │ 远端                 │
              │ · 浏览器控制台(多开) │   │ nc / socat / script │
              │ · AI Agent(API 轮询)│   │                     │
              └─────────────────────┘   └─────────────────────┘
```

- **多端共享**：GUI、浏览器（可多开）、`nc`、AI 脚本同时"attach"到同一串口，
  所有人看到同一路输出，所有人都能输入 —— 就像 `tmux attach` 同一个会话。
- **AI 友好**：JSON API + SSE 实时流 + 全局序号(seq)游标，`curl` 即可完成
  "打开串口 → 发命令 → 收输出"的完整控制循环，远端 AI 由此间接控制 TTL。
- **两个前端**：
  - `rkttl-gui.exe` — Windows 原生图形界面（walk/Win32，单文件 ~9MB）
  - `rkttl` — Linux / Windows 命令行（serve / bridge / monitor / ports）

Rockchip 调试串口默认 **1500000 8N1**，工具已按此预设。

---

## 一、快速开始（Windows 图形界面）

1. 安装 CH340 驱动（仓库上层目录有 `USB转串口驱动-CH340.zip` / `DriverAssitant/`）。
2. 运行 `rkttl-gui.exe`：
   - 顶部选择串口（自动优先选中 CH340）→ 点 **打开串口**；
   - 中间终端区可查看输出、底部输入框回车发送（支持 Ctrl+C / HEX / CRLF）；
   - **网络共享** 区默认启动 HTTP 服务（`0.0.0.0:7373`），点
     **打开网页控制台** 或 **复制控制台URL** 把地址交给远端。
3. 远端浏览器打开 `http://<宿主机IP>:7373/?token=<令牌>` 即可同时收发。

> **终端直连输入（xshell 式）**：GUI 与网页控制台都去掉了独立输入框 ——
> 直接在终端末尾打字，`Enter` 发送（CR）；GUI 终端聚焦时 `Ctrl+C`
> **一律映射为 TTL 中断 0x03**（复制文本用 `Ctrl+Insert`）；
> `Tab`/`Esc`/`Ctrl+D`/`Ctrl+U` 透传；新输出到达时未发送的输入行自动
> 悬浮在最新内容之后。HEX 发送在 GUI"工具"菜单，网页端点 `HEX` 按钮。
> **全屏程序（htop/top/vim 等）**：内置 VT100/xterm 子集终端引擎
> （光标寻址/清屏/滚动区域/备用屏/反色与颜色）。网页端自动切换为
> 彩色网格视图（行级 diff 渲染 + 光标），键盘直接映射终端序列
> （方向键/F 键/翻页）；GUI 端检测到全屏程序时切换为屏幕快照模式
> （纯文本快照，键盘直发），退出程序后自动恢复流式终端。
> **操作日志**：GUI 勾选"操作日志"展开折叠面板，记录本机与远端的关键
> 操作（串口开关/断线重连、HTTP/TCP 启停、令牌轮换、远端打开/清空/导出、
> TCP 客户端增减），最近 500 条。
> **终端主题**：菜单「视图」内置 12 款终端配色（默认深色 / Dracula / One
> Dark / Nord / Monokai / Gruvbox Dark / Solarized Dark / Tokyo Night /
> Ubuntu 紫 / Windows 终端 / Solarized Light / 纸白），作用于终端区与操作
> 日志区；选择持久化到 exe 同目录 `rkttl-gui.json`，下次启动自动恢复。
> **TCP 桥帮助**：GUI 的 TCP 桥行有 **帮助** 按钮，悬停有简短提示，点击弹出
> nc/socat 具体连接命令（带本机实际 IP，可一键复制）。
> GUI 的"过滤ANSI"开关只影响本地终端区显示；网页控制台支持 ANSI 彩色渲染。
> **会话日志**：打开串口后原始字节流自动记录到 exe 同目录 `logs/`
> （`rkttl-serial-<时间戳>.log`，含全部历史、不受 1MB 缓冲限制）。菜单"日志"
> 支持导出、清空（含接收缓冲）、打开日志目录；网络共享区的"会话日志"行
> 实时显示文件与大小。
> **对外网卡**：控制台地址由所选网卡决定。默认"全部网卡 (0.0.0.0)"监听并
> 自动展示最佳**物理**网卡地址（虚拟网卡如 VMware/WSL/Docker 会标注
> `[虚拟]` 且排序靠后）；选中具体网卡后监听地址自动绑定该 IP，点
> "应用网络设置"生效。

## 二、快速开始（Linux 命令行）

```bash
# 构建（Go 1.22+）
make linux          # 产出 bin/rkttl-linux-amd64

# 列出串口
./bin/rkttl-linux-amd64 ports

# 启动共享服务：HTTP API + 网页控制台 + TCP 桥（-port 可省略，由远端打开）
# 加 -logdir 可像 GUI 一样把串口原始字节流记录到文件
./bin/rkttl-linux-amd64 serve -port /dev/ttyUSB0 -listen 0.0.0.0:7373 -tcp 0.0.0.0:9999 -logdir ./logs

# 本地直连（与远端共享同一路输出）
./bin/rkttl-linux-amd64 monitor -port /dev/ttyUSB0

# 仅 TCP 桥（无 HTTP）
./bin/rkttl-linux-amd64 bridge -port /dev/ttyUSB0 -listen 0.0.0.0:9999
```

启动横幅会打印控制台地址与访问令牌。串口意外断开（USB 重插）加 `-reopen` 自动重连。

## 三、远端接入方式

### 1) 浏览器控制台（人类）

`http://<ip>:7373/?token=<token>` —— ANSI 彩色终端、**终端直连输入**（无独立
输入框，直接打字 Enter 发送，Ctrl+C/Tab/Esc 透传，粘贴逐行执行）、清空/导出
日志、多开共享、**?** 按钮查看 nc/socat 远端接入命令。控制台采用**流式渲染**：
换行/退格/颜色与网络分块边界无关，逐字符回显（如 U-Boot 行编辑、退格修正）
显示正确。**连接即回放**：网页打开时会自动收到接收缓冲中的历史日志（SSE
断线重连后按 seq 去重补齐，不重不漏）；URL 追加 `&history=N` 可只回放最近
N 字节、`&history=0` 关闭回放。

### 2) TCP 桥（nc / socat）

```bash
nc 192.168.1.10 9999                          # 交互式收发
socat -,raw,echo=0 TCP:192.168.1.10:9999      # 更接近真终端
# 远端伪装成本地串口（给只认 /dev/tty* 的工具用）：
socat pty,link=/tmp/ttyrkttl,raw TCP:192.168.1.10:9999
```

多个 TCP 客户端同时在线时共享同一路输出。`-quiet` 可关闭横幅纯透传。

### 3) HTTP API（AI / 脚本，核心）

所有接口支持 `Authorization: Bearer <token>` 头或 `?token=` 参数。
返回统一为 `{"ok":true,"data":{...}}` 或 `{"ok":false,"error":"..."}`。

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/ports` | GET | 列出宿主机串口（含 USB VID/PID，CH340=1A86:7523） |
| `/api/netifs` | GET | 列出宿主机网卡与各网卡的控制台地址（`is_virtual` 标记虚拟网卡） |
| `/api/status` | GET | 服务/串口状态与统计（含 `log` 日志文件状态） |
| `/api/open` | POST | `{"port":"COM3","baud":1500000}`（字段可省，默认 1500000 8N1） |
| `/api/close` | POST | 关闭串口 |
| `/api/write` | POST | `{"data":"boot_help","eol":"crlf"}`；`{"data":"0d0a41","hex":true}` |
| `/api/read` | GET | **非破坏性**读取，见下方 seq 说明 |
| `/api/flush` | POST | 清空接收缓冲；`{"file":true}` 同时清空会话日志文件 |
| `/api/export` | GET | 下载完整会话日志（日志文件优先，octet-stream） |
| `/api/break` | POST | `{"ms":100}` 发送 break |
| `/api/stream` | GET | SSE 实时流；连接时回放缓冲历史（`history=N` 限最近 N 字节，`0` 关闭） |

#### `/api/read` 的 seq 游标（多端共享的关键）

接收数据按全局字节序号 `seq` 索引。读取**不会消费**数据，多个客户端
各持自己的游标互不干扰 —— 两个 AI/脚本可同时跟踪同一路输出。

```bash
curl -H "Authorization: Bearer $TOKEN" \
     "http://192.168.1.10:7373/api/read?since=0"          # 从缓冲区最旧开始
# → {"ok":true,"data":{"seq":4832,"n":4096,"text":"...","b64":"...","hex":"...","truncated":true,...}}

curl ... "http://192.168.1.10:7373/api/read?since=4832&wait_ms=1000&max=65536"
#                                                         └─ 等新数据最多 1s
```

- `since` 缺省 = 最近 4096 字节；`0` = 缓冲区最旧处开始
- `truncated:true` 表示请求的起点之前的旧数据已被环形缓冲覆盖（默认 1MB）
- `wait_ms` 无新数据时的最长等待（长轮询），`-1` 等价 60000

#### AI 控制循环示例（bash）

```bash
T="Authorization: Bearer xxxx"; H="http://192.168.1.10:7373"

curl -H "$T" $H/api/open  -d '{"port":"COM3"}'            # 打开（默认 1500000）
curl -H "$T" $H/api/flush -X POST                          # 丢弃历史日志，从"现在"开始
curl -H "$T" $H/api/write -d '{"data":"version","eol":"crlf"}'
SEQ=$(curl -s -H "$T" "$H/api/read?since=0" | jq .data.seq) # 记下当前游标
curl -H "$T" "$H/api/read?since=$SEQ&wait_ms=2000" | jq -r .data.text
# ...反复 write / read(携带最新 seq)，即完成对板子的间接控制...
curl -H "$T" $H/api/write -d '{"data":"03","hex":true}'   # Ctrl+C 中断
```

#### SSE 实时流

```bash
curl -N -H "Authorization: Bearer $TOKEN" http://192.168.1.10:7373/api/stream
# event: data
# data: {"seq":1234,"b64":"..."}
```

## 四、构建

```bash
# Linux 上交叉编译全部产物（需要 python3 生成图标，仅首次）
make                 # = test + linux + windows
# 产物：
#   bin/rkttl-linux-amd64    Linux 命令行
#   bin/rkttl.exe            Windows 命令行（控制台程序）
#   bin/rkttl-gui.exe        Windows 图形界面（-H windowsgui）
```

Windows 本机构建：双击 `build.bat`（需要 Go 1.22+）。

```
rkttl/
├── cmd/rkttl/          # CLI 前端（Linux/Windows）
├── cmd/rkttl-gui/      # Windows GUI 前端（walk 声明式 UI）
├── internal/hub/       # 核心：串口生命周期 + 环形缓冲(seq) + 订阅广播
├── internal/api/       # HTTP JSON API + SSE + 内嵌网页控制台
├── internal/tcpbridge/ # 裸 TCP 多客户端桥
├── internal/ansi/      # ANSI 转义清理（GUI 显示用）
└── tools/mkicon.py     # 图标生成（无第三方依赖）
```

## 五、安全须知

- 默认**监听 0.0.0.0 且自动生成随机令牌**；不要使用 `-no-auth`，
  除非处于完全隔离的网络（串口 = 完全控制开发板，甚至可以刷机）。
- 令牌会出现在 URL 中（`?token=`），注意浏览器历史/代理日志。
- 建议部署在 VPN / WireGuard / tailscale 之内，或在前面加 TLS 反代。
- `rkttl-gui.exe` 可用"重新生成"随时轮换令牌。

## 六、常见问题

- **打开串口失败 "Access is denied"**：串口被其他程序占用（RTT Tool、
  PuTTY、另一个 rkttl 实例等）。
- **1500000 波特率乱码**：确认 CH340 驱动版本较新（支持 1.5Mbps）；
  RK3588/RK3399 调试串口固定 1500000，RK3328 等部分型号为 115200。
- **Windows COM10 以上**：直接填 `COM10` 即可（库自动处理 `\\.\` 前缀）。
- **GUI 终端区颜色**：Windows EDIT 控件不支持 ANSI 色，故默认过滤转义序列；
  需要看颜色请用网页控制台。
- **多人同时输入**：字节可能交错（与 tmux 相同的语义），实际使用中通常
  一端主导输入、其余端观察。

## 七、测试

```bash
make test    # 单元测试：环形缓冲 seq 语义（含随机压力）+ ANSI 清理
```

端到端验证（PTY 模拟串口，覆盖 29 项：多客户端共享、SSE 双流、TCP 桥、
鉴权、seq 游标、wait_ms、HEX、flush/close/重开等）在开发机通过。

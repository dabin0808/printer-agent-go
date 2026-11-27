# printer-agent

![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)
![Platform](https://img.shields.io/badge/Platform-Windows%2010%2F11%20x64-0078D6?logo=windows&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-green)
![Size](https://img.shields.io/badge/exe-~5.8MB-orange)
![Deps](https://img.shields.io/badge/dependencies-zero-success)

**printer-agent** 是一个面向「云打印」场景的 **Windows PC 中转端**（Agent），用 Go 编写，
编译产物为**单个绿色 exe（约 5.8MB），零依赖、免安装**——拷到任何 Windows 10/11 x64
电脑上双击即用，不需要安装 Go、Python 或任何运行时。

它补齐的是云打印架构里「最后一公里」的角色：

```
┌──────────────┐      ┌────────────────────┐      ┌──────────────────┐
│  云端 / APP   │ ───▶ │  printer-agent.exe  │ ───▶ │  普通打印机        │
│ (任务调度/文件) │ HTTP │  (本仓库, PC 中转端)  │printto│ (局域网/USB/虚拟) │
└──────────────┘      └────────────────────┘      └──────────────────┘
```

> 典型业务：连锁门店/经销商收银台电脑运行本 Agent，总部云平台把打印任务（小票、
> 面单、单据）远程投递到门店的普通打印机——**不需要云端直达打印机协议支持**，
> 任何 Windows 能"打印"的东西都能发。

---

## ✨ 功能总览

### 1. 单文件绿色部署
- 页面通过 `go:embed` 编译期内嵌进 exe，**新电脑只需拷 1 个文件**
- 纯 Go 标准库 + syscall 实现，**零第三方依赖**（供应链风险为零，内网可离线构建）
- 监听 `http://127.0.0.1:18080`，关掉控制台窗口即停止服务

### 2. 内置 Web 操作台
双击 exe → 浏览器打开 `http://127.0.0.1:18080/` 即可：

| 能力 | 说明 |
|------|------|
| 📄 文件选择 | 自动列出 exe 同目录 `files\` 下的可打印文件 |
| 🖨️ 打印机选择 | 直接枚举本机 Windows 打印机（含网络/连接打印机） |
| 🔢 打印参数 | 份数、彩色/黑白（DevMode 语义，字段已预留） |
| 📡 任务日志 | 页面实时显示任务投递结果 |

页面同时支持 `file://` 双击打开（响应带 CORS 头）。

### 3. HTTP API（供上层系统对接）

| 路由 | 方法 | 说明 |
|------|------|------|
| `/` | GET | 内嵌操作页面（磁盘 `app.html` 优先，支持热更覆盖） |
| `/list_files` | GET | 列 `files\` 目录可打印文件 |
| `/list_printers` | GET | 枚举本机打印机（Win32 `EnumPrintersW`） |
| `/app_send_task` | POST | 表单 `filename/copies/color/printer`，入队打印 |
| `/version` | GET | 当前版本号（热更新探测基准） |

示例：

```bash
curl -X POST "http://127.0.0.1:18080/app_send_task" \
     -d "filename=sample-order.txt&copies=2&printer=Microsoft Print to PDF"
```

### 4. 可靠的打印消费线程
- 每 2 秒扫描内存队列，`ShellExecuteW` 的 `print`/`printto` 动词送打印机
- 指定打印机时自动切 `printto` 动词并**给打印机名加引号**（含空格名称不会截断）
- **失败自动回队首重试**（10 秒间隔，任务不丢失），直到打印机恢复
- `filepath.Base` 削掉路径 + 校验文件必须存在于 `files\`，杜绝路径穿越

### 5. 完整的自动热更新（生产级闭环）🔥

exe 运行中 Windows 不允许覆盖、但**允许改名**——这是 Chrome/VSCode 同款热更方案：

```
Agent(每N秒) ─ GET manifest.json ──▶ latest > 当前版本？
                                        │ 是
                                        ▼
        ① 下载新 exe → agent.exe.new（临时名，失败不污染正式文件）
        ② sha256 强校验（防传输损坏/篡改）
        ③ 改名换位: agent.exe → .old；.new → agent.exe
        ④ 打印队列非空则推迟换位（不打断正在打印的单据）
        ⑤ os.StartProcess 拉起新进程，旧进程退出
        ⑥ 新进程 30s 自检通过 → 自动清理 .old（回滚窗口关闭）
```

- **页面/程序双通道**：`app.html` 独立热更（sha256 变化即下载覆盖，刷新浏览器即生效，**无需重启**）；exe 走版本比较通道
- **语义化版本比较**：`v1.10.0 > v1.9.0`（不是字符串比较）
- **任何一步失败都不影响当前打印业务**：清单拉不到、校验不过、换位失败均静默记日志、下轮重试
- **回滚兜底**：换位失败立即回滚 `.old`；拉起新进程失败自动恢复旧版继续服务
- 更新地址/间隔可用环境变量覆盖：`AGENT_MANIFEST_URL`、`AGENT_UPDATE_INTERVAL_SEC`

### 6. 演示模式自足
内置"模拟云端"语义：单进程即可完整跑通「页面发任务 → 本地打印」，
不需要任何外部服务。接真实云平台时只需把「HTTP 接任务」换成「轮询/WS 订阅云端任务」，打印逻辑不变。

---

## 🚀 快速开始

### 部署（零环境）

```
部署 = 拷贝 2 个东西到任意目录（如 D:\printer-agent\）：
  printer-agent.exe      ← 程序本体（页面已内嵌）
  files\sample-order.txt ← 想打印的文件（可从本包拷）
```

1. 双击 `printer-agent.exe`（建议右键「以管理员身份运行」，份数/双面设置才能生效）
2. 控制台显示 `PC中转端已启动 v1.1.0: http://127.0.0.1:18080`
3. 浏览器打开 **http://127.0.0.1:18080/**
4. 选文件 → 选打印机 → 填份数 → 点「发送打印任务」→ 打印机出纸

### 从源码构建

构建环境零侵入（缓存/工具链均可放非系统盘）：

```bash
git clone https://github.com/dabin0808/printer-agent-go.git
cd printer-agent-go

# Windows
set GOPATH=D:\gopath& set GOCACHE=D:\gocache
go build -trimpath -ldflags="-s -w" -o printer-agent.exe .

# 或直接双击 build.bat
```

产物为单文件 `printer-agent.exe`（约 5.8MB）。

### 配置项（环境变量）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `AGENT_MANIFEST_URL` | `http://127.0.0.1:8000/agent/manifest.json` | 热更新清单地址 |
| `AGENT_UPDATE_INTERVAL_SEC` | `60` | 热更新检查间隔（秒，最小 10） |

---

## 🔄 热更新 · 云端侧怎么配

云端**不需要任何"更新服务"代码**——一个静态文件服务器（nginx/caddy/IIS）+
一个 `manifest.json` 就是最小更新协议：

```json
{
  "latest": "v1.1.0",
  "minRequired": "v1.0.0",
  "files": {
    "agent": {
      "url": "https://your-domain.com/agent/v1.1.0/printer-agent.exe",
      "sha256": "<新exe的sha256>",
      "size": 6123008
    },
    "page": {
      "url": "https://your-domain.com/agent/v1.1.0/app.html",
      "sha256": "<新页面的sha256>",
      "size": 6324
    }
  }
}
```

**发版三原则**：
1. **旧版本目录永不删**——回滚 = 把 `latest` 改回旧版本号，全网 Agent 自动"降级"
2. **manifest 是唯一开关**——所有推进/回滚动作只改这一个文件
3. **先灰度后全量**——可用 `manifest-beta.json` 独立清单通道灰度

完整手册（目录布局、nginx 配置、安全加固、带宽保护）见 **[HOTUPDATE.md](HOTUPDATE.md)**；
源码逐段讲解见 **[CODE-GUIDE.md](CODE-GUIDE.md)**。

---

## 🏗️ 架构与实现要点

单进程双 goroutine，一把互斥锁保护的内存切片做队列，刻意不用 channel/数据库，
聚焦「HTTP 服务 + Windows 打印」两条主线：

```
printer-agent.exe（单进程）
├── 内嵌 Web 页面 (go:embed app.html)
├── HTTP API（5 个路由）
├── printWorker  打印消费线程（队列 → ShellExecute → 失败重试）
└── updater      热更新器（清单 → 下载 → 校验 → 换位 → 择机重启）
```

### 踩坑实录（对同类项目有参考价值）

| 决策 | 原因 |
|------|------|
| `EnumPrintersW` 用 level=1 不用 level=4 | 实测 level=4 返回的名称指针指向独立堆、无法从返回缓冲区读；level=1 指针在缓冲区内可直接解析 |
| `printto` 参数带引号 `"打印机名"` | ShellExecute 动词要求参数为带引号的打印机名，否则含空格名称解析失败 |
| manifest.json 必须**无 BOM** 的 UTF-8 | PowerShell `Set-Content -Encoding UTF8` 默认写 BOM，Go `json.Decoder` 会报 `invalid character 'ï'`（本次开发实测踩坑） |
| 失败任务回**队首**而非队尾 | 打印机暂时离线时保持 FIFO 公平性，重试时不让后面任务插队 |
| 更新用"改名替换"不用"覆盖写" | Windows 对运行中 exe 加写锁（覆盖报 5 拒绝访问），但改名不受限 |
| 热更检查失败静默沿用当前版本 | 更新的失败 ≠ 当前功能失败，打印业务永远优先 |

---

## 🗺️ 生产化路线（接入真实云打印平台）

| Demo 现状 | 生产方案方向 |
|-----------|-------------|
| 演示模式自收自发 | 轮询/WS 拉取云端任务（设备绑定 + 校验码鉴权） |
| 任务在内存队列 | 云端任务表落库；本地文件队列防断电丢任务 |
| 无鉴权 | deviceId + 校验码 + HTTPS；任务签名防伪造 |
| ShellExecute 火后不管 | `EnumJobs` 读打印队列确认真实出纸，回报成功/失败 |
| 份数/双面未生效 | GDI/RAW 通道按任务传 DevMode（当前 SetPrinter 方案需管理员） |
| 单机页面自用 | Agent 上报打印机清单到云端，APP 远程选择任意门店打印机 |

---

## 📁 仓库结构

```
printer-agent-go/
├── main.go                   ← 全部主逻辑（HTTP/枚举/队列/打印，~280行）
├── updater.go                ← 热更新器（清单/下载/校验/换位/semver，~270行）
├── app.html                  ← 操作页面（go:embed 打进 exe）
├── build.bat                 ← 双击构建
├── build-hotupdate-demo.bat  ← 一键搭建热更新演示环境（构建双版本+生成 manifest）
├── HOTUPDATE.md              ← 热更新完整指南（云端手册+安全清单）
├── CODE-GUIDE.md             ← 源码逐段解读（新人上手/二次开发）
├── files/sample-order.txt    ← 示例打印文件
└── tools/enumtest/           ← 打印机枚举踩坑最小复现（调试用）
```

## 🧪 本地验证热更新闭环

```bash
# 1. 启动静态文件服务器（演示用 python，生产用 nginx）
cd somewhere && python -m http.server 8000
#    目录结构: agent_repo/manifest.json + agent_repo/v1.1.0/printer-agent.exe + app.html

# 2. 运行旧版 Agent（加速检查间隔）
set AGENT_UPDATE_INTERVAL_SEC=15
printer-agent.exe

# 3. 观察日志: 发现新版本 → 下载 → 校验通过 → 换位 → 新进程启动 → .old 清理
curl http://127.0.0.1:18080/version   # 应返回新版本号
```

## 📜 License

[MIT](LICENSE)

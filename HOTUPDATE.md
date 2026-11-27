# printer-agent 热更新指南（云端版本分发）

> 场景：你有一台云服务器，新版 exe / 新版页面放上去，全国经销商电脑上的 Agent
> **自动发现新版本 → 下载 → 校验 → 替换 → 重启**，全程无人到场。
> 配套阅读：`README.md` §3（更新原理）、`CODE-GUIDE.md` §7（差距地图）。

---

## 1. 总体架构：云服务器只做"静态文件 + 清单"

```
        你的云服务器（如 nginx / caddy / 甚至 IIS，纯静态即可）
        ┌─────────────────────────────────────────┐
        │  /agent/manifest.json     ← 版本清单(核心) │
        │  /agent/v1.1.0/printer-agent.exe          │
        │  /agent/v1.1.0/app.html                   │
        │  /agent/v1.0.0/...  (旧版本保留可回滚)      │
        └─────────────────────────────────────────┘
                      ▲           │
        ①轮询清单(每5分钟)   ②下载新版本+sha256校验
                      │           ▼
        经销商 Windows 电脑上的 printer-agent.exe
```

**关键认知：云端不需要任何"更新服务"代码。** manifest.json 就是最小的更新协议，
Agent 是唯一客户端。后续要灰度/强制，也只是往 json 里加字段。

## 2. 云端文件布局与清单格式

### 2.1 目录布局

```
/var/www/agent/                    ← nginx 站点根目录
├── manifest.json                  ← 当前最新稳定版指向
├── manifest-beta.json             ← 灰度通道（可选）
├── v1.0.0/
│   └── printer-agent.exe
└── v1.1.0/
    ├── printer-agent.exe          ← 新版完整包
    ├── app.html                   ← 新版页面（可选，走 override 热更）
    └── printer-agent.exe.sha256   ← 校验文件（或写进 manifest）
```

### 2.2 manifest.json（版本清单）

```json
{
  "latest": "v1.1.0",
  "minRequired": "v1.0.0",
  "channel": "stable",
  "publishAt": "2026-11-27T10:00:00+08:00",
  "notes": "修复打印机枚举遗漏；页面支持外置覆盖",
  "files": {
    "agent": {
      "url": "https://your-domain.com/agent/v1.1.0/printer-agent.exe",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      "size": 5488640
    },
    "page": {
      "url": "https://your-domain.com/agent/v1.1.0/app.html",
      "sha256": "ab12...",
      "size": 8420
    }
  }
}
```

| 字段 | 作用 |
|------|------|
| `latest` | 最新版本号，Agent 用它与 `AgentVersion` 比较 |
| `minRequired` | 最低强制版本：低于它的 Agent 弹强提示/拒绝继续打印任务 |
| `files.agent.sha256` | **安全核心**：下载后必须校验，防止传输损坏与被篡改的包 |
| `notes` | 更新说明，Agent 可在页面/日志展示 |

## 3. 更新流程（Agent 侧完整时序）

```
Agent (每5分钟 / 每次启动)
  │
  ① GET /agent/manifest.json            （带 If-None-Match，304 则本轮结束）
  │
  ② 比较 latest > AgentVersion ？
  │    └─ 否 → 睡到下个周期
  │
  ③ GET files.agent.url 下载到  agent.exe.new（同目录临时名）
  ④ 计算 sha256 与 manifest 比对，不符 → 删 .new，记日志，下轮重试
  │
  ⑤ 原子换名（Windows 运行中 exe 不能覆盖、但能改名）：
  │    agent.exe      → agent.exe.old
  │    agent.exe.new  → agent.exe
  │
  ⑥ 页面热更（独立进行，不依赖⑤）：
  │    下载 app.html → 覆盖 exe 同目录 app.html → 浏览器刷新即生效
  │    （Agent 的 / 路由优先读磁盘文件，磁盘没有才用内嵌页面）
  │
  ⑦ 重启程序逻辑（三选一，按更新内容决定）：
  │    a. 仅页面变了 → 无需重启，等下次重启自然生效
  │    b. 逻辑变了   → Agent 提示"重启生效"，或空闲时（打印队列清空）自杀重启：
  │                     os.StartProcess(新exe) + 老进程退出
  │
  ⑧ 新进程启动自检：
       - 正常 → 删除 agent.exe.old
       - 异常（如连续崩溃）→ 启动回滚：agent.exe.old → agent.exe
```

### 3.1 为什么"改名替换"而不是"覆盖写"

Windows 对**正在运行的 exe 文件加了写锁**（覆盖报错 5 拒绝访问），
但**改名不受限**。所以标准做法永远是：跑旧的下新 → 改名换位 → 重启。
这也是 Chrome / VSCode / 微信 PC 版同款方案，稳定可靠。

## 4. Agent 侧实现要点（updater.go，约 100 行）

生产接入时在现有项目新增一个文件即可，骨架：

```go
//go:build windows

package main

// updater 周期性向云端检查新版本并自更新
func startUpdater(manifestURL string, interval time.Duration) {
    go func() {
        for {
            check(manifestURL)
            time.Sleep(interval)
        }
    }()
}

func check(manifestURL string) {
    // 1. 拉清单: GET manifestURL → 解析 Manifest{Latest, Files}
    // 2. 比较版本: latest 与 AgentVersion 语义化比较(semver)，无新版即返回
    // 3. 下载: http.Get(files.agent.url) → 写 exePath+".new"
    // 4. 校验: sha256.Sum256(读 .new) hex 后与 manifest 比对，不符删 .new 返回
    // 5. 换位: os.Rename(exePath, exePath+".old")
    //          os.Rename(exePath+".new", exePath)
    // 6. 重启: exePath 参数 os.StartProcess(exePath, nil, &os.ProcAttr{})
    //          然后老进程 os.Exit(0)；新进程启动时若自检正常则删 .old
}
```

实现注意清单：

- [ ] **版本比较用 semver 规则**（v1.10.0 > v1.9.0），不能直接字符串比较
- [ ] **下载必须写临时文件名**（.new），失败/校验不过就删，永不污染正式文件
- [ ] **sha256 必验**；有条件再叠加：manifest 本身做 RSA 签名（防清单被篡改）
- [ ] **重启时机避开打印中**：检查内存队列 `len(queue)==0` 再重启，或等打印完成
- [ ] **启动自检**：新进程首屏做一次 `/version` + 打印机枚举，连续 N 次失败触发回滚 `.old`
- [ ] **清单请求带 ETag/If-None-Match**，几千台 Agent 轮询时省流量
- [ ] exe 同目录需要**写权限**（不要放 Program Files；放 `D:\printer-agent\` 这类目录）

## 5. 云服务器操作手册（你只管做这几步）

### 5.1 首次布置（一次性）

```bash
# 服务器上（以 nginx 为例）
mkdir -p /var/www/agent/v1.1.0
# 把构建好的 exe 和页面传上去
scp printer-agent.exe  root@你的云服务器:/var/www/agent/v1.1.0/
scp app.html           root@你的云服务器:/var/www/agent/v1.1.0/
# 生成校验值
sha256sum /var/www/agent/v1.1.0/printer-agent.exe   # 填进 manifest
```

编写 `manifest.json`（§2.2 格式）放到 `/var/www/agent/`，nginx 配置加一段：

```nginx
location /agent/ {
    root /var/www;
    autoindex off;
    add_header Cache-Control "no-cache";   # manifest 必须不缓存
}
```

### 5.2 日常发版（每次更新的标准动作）

```bash
# 1. 本机构建新版并改 AgentVersion（main.go 常量）
# 2. 上传新版本目录
ssh root@云服务器 "mkdir -p /var/www/agent/v1.2.0"
scp printer-agent.exe root@云服务器:/var/www/agent/v1.2.0/
scp app.html          root@云服务器:/var/www/agent/v1.2.0/
# 3. 算 sha256 → 更新 manifest.json 的 latest 与 files.*.url/sha256
# 4. 完成。全网 Agent 最迟 5 分钟后自动更新，无需任何现场操作
```

**发版三原则**：
1. **旧版本目录永不删**（回滚通道：把 manifest 的 latest 改回旧版本号即可全网回滚）
2. **先灰度后全量**：灰度期内 `manifest-beta.json` 指向新版，挑 1~2 台测试机改其
   清单 URL 指向 beta，观察 24h 再切 stable
3. **manifest 是唯一开关**：所有回滚/推进动作都只改这一个文件

## 6. 页面(app.html)与程序(exe)更新节奏对照

| 更新对象 | 云端动作 | Agent 行为 | 生效方式 | 是否重启 |
|---------|---------|-----------|---------|---------|
| 页面 app.html | manifest `files.page` 换 url+sha256 | 下载覆盖同目录文件 | 用户刷新浏览器 | ❌ |
| 程序逻辑 exe | manifest `latest`+`files.agent` 换 | 下载→换名→择机重启 | 重启后 | ✅ |
| 紧急回滚 | manifest `latest` 改回旧版本 | 下轮轮询自动"降级" | 自动 | ✅ |

> 两类更新走**同一个 manifest、同一个轮询循环**，只是 page 更新不需要重启，
> 所以可以先给"只改页面"的需求开 5 分钟级热更通道，迭代速度接近 Web。

## 7. 安全清单（公网分发必做）

| 风险 | 对策 |
|------|------|
| 下载包被劫持/损坏 | sha256 强校验（必做）；https 分发（必做） |
| 清单被篡改指向恶意包 | manifest RSA 签名，Agent 内置公钥验签（推荐） |
| 云服务器被打挂，Agent 拉不到清单 | 拉失败沿用当前版本静默重试，不影响打印业务（本次更新的失败≠当前功能失败） |
| 全网同时下载打爆带宽 | Agent 轮询加随机抖动（如 5min±60s）；大客户规模再考虑 CDN |
| Agent 被降级攻击（推旧有漏洞版） | `minRequired` 字段：低于最低版本即使 latest 更旧也不接受 |

## 8. 演示环境可立即验证的最小闭环（可选实施）

当前 `printer-agent-go` 已预留 `/version`。要本地演练完整闭环，加 3 个东西：

1. `mock_cloud.py`（Python demo 现成进程）加一个 `/agent/manifest.json` 路由
2. Agent 加 `updater.go`（§4 骨架，~100 行），轮询 `http://127.0.0.1:8000/agent/manifest.json`
3. 构建一个 `v1.1.0` 假新版（改掉 AgentVersion 常量重新 build）放进清单

验证路径：启动 mock_cloud + v1.0.0 Agent → 等 5 分钟（或调短 interval）→
看日志出现"发现新版本 v1.1.0 → 下载 → 校验通过 → 换名 → 重启" →
`tasklist` 里 exe 变成新版进程、`/version` 返回 v1.1.0。

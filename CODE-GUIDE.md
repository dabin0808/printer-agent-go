# printer-agent 源代码解读指南

> 面向第一次读这个项目的 Go/后端开发者。全文对照 `main.go`（约 330 行）逐段讲解，
> 读完你应能：说清每个函数的职责 → 独立改出一个新接口 → 知道生产化时要动哪里。
> 配套阅读：`README.md`（使用与架构）、`tools\enumtest\main.go`（枚举踩坑复现）。

---

## 0. 一图总览

```
main.go 单文件，共 6 个部分：

  ① 全局声明        L22-47    内嵌页面 / Task结构 / Win32句柄 / 任务队列 / 版本号
  ② main()          L49-89    初始化目录 → 起打印线程 → 注册5个HTTP路由 → 监听18080
  ③ listPrinters    L91-146   枚举 Windows 打印机（win32 API，全项目最"底层"的代码）
  ④ sendTask        L148-177  收页面任务：校验→入队
  ⑤ printWorker     L179-215  打印线程：出队→ShellExecute送打→失败重试
  ⑥ 工具函数         L217-252  CORS / 参数解析 / 文件路径

数据流（演示模式）：
  浏览器页面 ──POST /app_send_task──▶ sendTask ──内存queue──▶ printWorker ──printto──▶ 打印机
```

**设计要点：整份代码只有两个 goroutine**（main 主线程跑 HTTP、printWorker 跑打印），
用一把 `sync.Mutex` 保护的内存切片做队列——刻意不用 channel/数据库，让初读者聚焦在
"HTTP 服务 + Windows 打印"两条主线上。

---

## 1. 头部与全局声明（L1-47）

### 1.1 构建约束与内嵌

```go
//go:build windows        ← 第1行：只能在 Windows 上编译（调用了 Win32 API）

//go:embed app.html       ← 编译时把 app.html 的内容打进 exe
var embeddedPage embed.FS
```

- `//go:build windows`：交叉编译保护。在 Linux/Mac 上 `go build` 会直接报"找不到 syscall.StringToUTF16Ptr"等，防止误编。
- **`go:embed` 是"单文件绿色 exe"的关键**：页面不是磁盘上的文件，而是 exe 内部资源。
  改了 app.html 必须重新 `go build` 才会生效（没有热加载）。

### 1.2 Task 结构体（与 Python demo 字段一一对应）

```go
type Task struct {
    ID       int64  // 任务号：时间戳毫秒 % 1e8，演示用；生产版=云端任务表主键
    Filename string // 只取文件名（filepath.Base），防路径穿越
    Color    int    // 1=彩色 2=黑白（Windows DevMode 取值，沿袭原版）
    Side     int    // 1=单面 2=双面（当前版本未消费，见 §7 差距）
    Copies   int    // 份数（同上，DevMode 需管理员，暂缺）
    Printer  string // 目标打印机名；空字符串 = 系统默认打印机
}
```

> 命名与 JSON tag 刻意与 Python demo 的任务字段保持同语义，后续接 Java 云端时
> 把 tag 改成 contract 字段名即可，业务代码不动。

### 1.3 Win32 句柄声明（懒加载模式）

```go
var (
    shell32           = syscall.NewLazyDLL("shell32.dll")
    procShellExecuteW = shell32.NewProc("ShellExecuteW")
)
```

- `NewLazyDLL`：**不在程序启动时加载 dll，第一次调用才加载**——即使某天 dll 缺失，
  程序也能正常启动到不需要它的分支。
- `ShellExecuteW`：Windows shell 的"像双击一样执行"API，支持 `print` / `printto` 动词，
  是本项目的打印核心（§5 详解）。

### 1.4 任务队列与版本号

```go
var (
    queueMu sync.Mutex   // 保护 queue 的互斥锁
    queue   []Task       // 内存队列；进程重启即丢（生产版落库）
)
const AgentVersion = "v1.0.0"   // /version 接口返回；热更新的比对基准
```

> 为什么不用 channel？——轮询写法对初读者更直白，且打印线程天然单消费者，
> 锁切片足够。生产化时建议换成带缓冲 channel 或持久化队列。

---

## 2. main()：启动流程（L49-89）

按编号三步：

| 步骤 | 代码 | 说明 |
|------|------|------|
| 1 | `os.MkdirAll(filesDir, 0755)` | 确保 exe 同目录有 `files\`，没有就建 |
| 2 | `go printWorker(filesDir)` | 起打印消费 goroutine（后台无限循环） |
| 3 | `mux.HandleFunc(...)` × 5 + `ListenAndServe(":18080")` | 注册路由并常驻监听 |

### 2.1 五个路由一览

| 路由 | 方法 | 处理函数 | 返回 |
|------|------|---------|------|
| `/` | GET | 内联闭包 | 内嵌的 app.html |
| `/list_files` | GET | 内联闭包 | `["a.txt","b.pdf"]` |
| `/list_printers` | GET | `listPrinters` | `["打印机A","打印机B"]` |
| `/app_send_task` | POST | `sendTask` | `{"code":0,"msg":"queued","tid":123}` |
| `/version` | GET | 内联闭包 | `{"version":"v1.0.0"}` |

### 2.2 改动指引：新增一个接口只需 3 步模式

```go
mux.HandleFunc("/your_api", func(w http.ResponseWriter, r *http.Request) {
    // 1. 读参数（r.FormValue / r.URL.Query()）
    // 2. 业务处理
    // 3. writeCORS(w); json.NewEncoder(w).Encode(结果)
})
```

`writeCORS` 必须在所有 JSON 返回前调用——它同时设置了 CORS 头和 Content-Type（见 §6.1）。

---

## 3. listPrinters：Windows 打印机枚举（L91-146）★ 全项目最底层代码

这是唯一一段"真正的 Win32 编程"，也是踩过坑的地方，值得逐行理解。

### 3.1 两段式调用（Windows API 惯例）

`EnumPrintersW` 需要调**两次**：第一次探测需要多大缓冲区，第二次真正枚举：

```go
// 第一次：缓冲区传 0，让系统告诉我们 needed 是多大
r1, _, _ := procEnum.Call(flags, 0, level, 0, 0,
    uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
// 此时 r1==0、GetLastError==122（ERROR_INSUFFICIENT_BUFFER）是【预期行为】
// 关键产物是 needed 字节数

buf := make([]byte, needed)
// 第二次：给足缓冲区，真正拿数据
r1, _, err := procEnum.Call(flags, 0, level, uintptr(unsafe.Pointer(&buf[0])),
    uintptr(needed), ...)
```

### 3.2 为什么用 level=1 而不是 level=4（踩坑实录）

`EnumPrintersW` 的 `level` 参数决定返回什么结构。**实测结论**（`tools\enumtest` 复现）：

| level | 现象 | 结论 |
|-------|------|------|
| 4（最轻量） | 返回的 `pPrinterName` 指针指向**独立堆内存**，不在我们提供的 buf 里 → 按 buf 内偏移去读必乱码 | ❌ 弃用 |
| 1 | 名称指针指向 buf 内部，可直接偏移读取 | ✅ 采用 |

> 这是 Win32 API 的一个隐蔽契约差异：不同 level 的实现里字符串存放位置不同。
> 遇到类似"指针读出来是乱码"的 Win32 问题，第一反应应是"指针可能不在我给的缓冲区里"。

### 3.3 结构体对齐与字符串解析

```go
type printerInfo1W struct {
    Flags       uint32
    _           uint32   // 手工补 4 字节 padding，保证 8 字节对齐（64位）
    Description *uint16
    Name        *uint16
    Comment     *uint16
}                        // 64 位下正好 32 字节

infos := unsafe.Slice((*printerInfo1W)(unsafe.Pointer(&buf[0])), returned)
```

- `_ uint32` 是**手工对齐**：Win32 的 DWORD 是 4 字节，后面跟 64 位指针时需要补 4 字节。
  不写这行 Go 也会自动对齐，但显式写出能与 C 头文件定义逐字段对应，可读性好。
- 字符串是 **NUL 结尾的 UTF-16**，指向 buf 内部。解析三步：
  1. 指针转偏移：`start = 指针地址 - buf首地址`
  2. 以 `[]uint16` 视角读，直到遇到 `\0`
  3. `syscall.UTF16ToString` 转成 Go string

```go
u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[start])), (uintptr(len(buf))-start)/2)
for j, c := range u16 {
    if c == 0 { names = append(names, syscall.UTF16ToString(u16[:j])); break }
}
```

---

## 4. sendTask：接任务接口（L148-177）

```go
// 1. 仅接受 POST 表单
if r.Method != "POST" { http.Error(w, "method not allowed", 405); return }
r.ParseForm()

// 2. 构造 Task
t := Task{
    ID:       time.Now().UnixNano() / 1e6 % 1e8,     // 毫秒时间戳取模做演示ID
    Filename: filepath.Base(r.FormValue("filename")), // ★安全点：只留文件名
    Color:    atoiDefault(r.FormValue("color"), 1),  // 非法/空值回退默认
    Copies:   atoiDefault(r.FormValue("copies"), 1),
    Printer:  r.FormValue("printer"),
}

// 3. 校验文件存在 → 入队 → 返回 tid
```

两个防御性设计：

1. **`filepath.Base`**：用户传 `../../evil.exe` 也会被削成 `evil.exe`，
   配合下一步"文件必须存在于 files\ 目录"，杜绝路径穿越。
2. **`atoiDefault`**：所有数字参数都有默认值兜底，脏数据不会 panic。

> curl 等价调用：
> `curl -X POST "http://127.0.0.1:18080/app_send_task" -d "filename=sample-order.txt&copies=2&printer=Microsoft Print to PDF"`

---

## 5. printWorker：打印消费线程（L179-215）

goroutine 里的无限循环，每轮做三件事：

```
① 取任务：锁 → 队列空则 sleep 2s 继续 → 非空则取出队首
② 送打印：ShellExecuteW(hwnd=0, verb, 文件路径, 参数, 0, SW_HIDE)
③ 结果处理：ret<=32 失败 → 回队首 + sleep 10s 重试；否则记日志完成
```

### 5.1 print 与 printto 的切换（核心逻辑）

```go
verb := syscall.StringToUTF16Ptr("print")     // 默认：送系统默认打印机
var param uintptr
if t.Printer != "" {
    verb = syscall.StringToUTF16Ptr("printto") // 指定目标：换动词
    param = uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(`"`+t.Printer+`"`)))
}
```

| 动词 | 行为 | 参数 |
|------|------|------|
| `print` | 送**默认**打印机 | 无 |
| `printto` | 送**指定**打印机 | `"打印机名"` —— **引号必须有**，含空格的名称（如 Microsoft Print to PDF）不加引号会被截断 |

> 这就是"页面能选打印机"的实现原理：不是 Printer 对象选中它，而是把打印机名作为
> shell 动词参数传给 Windows 关联程序（PDF→Acrobat/Edge、txt→记事本），由它去连目标打印机。

### 5.2 ShellExecuteW 返回值契约

```
ret >  32  → 成功（返回的是新进程的 HINSTANCE，历史上是个>32的句柄值）
ret <= 32  → 失败（错误码：2=文件没找到，11=exe无效，31=无关联程序…）
```

> 注意："成功"仅代表**关联程序接受并启动了打印**，不保证纸张真的出来了
> （火后不管问题，生产化要读 EnumJobs 打印队列，见 §7）。

### 5.3 失败重试策略

```go
if ret <= 32 {
    time.Sleep(10 * time.Second)
    queue = append([]Task{t}, queue...)   // 头插：保持 FIFO 公平性
    continue
}
```

失败任务**回队首而非队尾**：如果它是因打印机暂时离线失败的，重试时仍是下一个要处理的，
不会让后面的任务"插队"。无限重试 + 每次间隔 10s，直到打印机恢复。

---

## 6. 工具函数（L217-252）

| 函数 | 作用 | 关键点 |
|------|------|--------|
| `printerName(p)` | 日志显示辅助：空→"系统默认" | 纯可读性 |
| `filesDirOf()` | 拼工作目录下的 files 路径 | 每次现取 cwd，支持任意位置双击运行 |
| `atoiDefault(s, def)` | 手写数字解析 | 拒绝负数/非数字，回退默认值；刻意不用 strconv，展示容错写法 |
| `writeCORS(w)` | 每个接口出口必调 | 一次设两个头：`Access-Control-Allow-Origin:*` + `Content-Type: application/json; charset=utf-8` |

### 6.1 CORS 为什么存在

支持**双击 app.html 以 file:// 协议打开**的场景：此时页面的 origin 是 `null`，
浏览器会拦截对 `http://127.0.0.1:18080` 的 fetch。响应带 `*` 才放行。
（从 `http://127.0.0.1:18080/` 本身打开页面时同源，CORS 无感。）

---

## 7. 与生产版的差距地图（改哪里）

| 位置 | Demo 现状 | 生产化动作 |
|------|----------|-----------|
| `Task` 结构体 | 本地文件名 | 加 `taskId`/`tenantId`/`fileUrl`（对象存储下载地址） |
| `sendTask`（接任务） | 页面本机 POST | 换成**轮询/WS 从 Java 云端拉任务** + 校验码鉴权（API-02f/02g） |
| `queue` 内存切片 | 重启即丢 | 落库（云端任务表为主，本地可加文件队列防断电） |
| `printWorker` ②步 | ShellExecute 火后不管 | 打印后 `EnumJobs` 轮询队列确认真实出纸，回报状态 |
| `Copies/Side` 字段 | 未消费 | 改用 GDI 打印或 RAW 通道按任务传 DevMode（当前 SetPrinter 方案需管理员） |
| `AgentVersion` + `/version` | 只读版本 | 加 `updater.go`：版本比对→下载→sha256→改名替换→重启（见 README §3） |
| `writeCORS` 全开 `*` | 内网演示 | 生产收紧为指定来源或不走浏览器 |

---

## 8. 常见改码任务速查

**改监听端口** → `main()` 里 `addr := ":18080"`。

**换页面** → 改 `app.html` → 重新 `build.bat`（embed 编译期固化，改页面必须重编）。

**加任务字段（如纸张型号）** → ① Task 加字段与 json tag → ② sendTask 解析 → ③ printWorker 消费 → ④ app.html 表单加控件。

**加打印结果回执** → printWorker 成功分支后，POST 状态到云端接口（对照 Python demo 的 `/finishTask.php`）。

**排除打印问题** →
1. 看 exe 控制台日志：`收到任务` 有而 `已送打印机` 无 → ShellExecute 失败，看 ret 码
2. ret=31：该文件类型没有关联程序（装 PDF 阅读器）
3. `.txt` 部分系统不响应 print 动词 → 换 PDF 验证
4. 手动验证动词：资源管理器右键文件 → 打印，效果应与 Agent 一致

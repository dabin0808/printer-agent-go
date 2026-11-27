//go:build windows

// printer-agent — CCS 云打印 PC 中转端（Go 绿色版）
// 架构对应：云端(Java微服务) → 本 Agent(轮询拉任务) → Windows 打印机(printto)
// 演示模式：内置模拟云端，单进程即可完整跑通"页面发任务→本地打印"
package main

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed app.html
var embeddedPage embed.FS

// Task 对应云端任务结构（与 Python demo 字段语义一致）
type Task struct {
	ID       int64  `json:"id"`
	Filename string `json:"filename"`
	Color    int    `json:"color"`   // 1=彩色 2=黑白
	Side     int    `json:"side"`    // 1=单面 2=双面
	Copies   int    `json:"copies"`
	Printer  string `json:"printer"` // 空=系统默认打印机
}

// shell32 提供 ShellExecuteW 调用（print/printto 动词）
var (
	shell32           = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// queue 内存任务队列（演示模式）；生产版替换为轮询 Java 云端
var (
	queueMu sync.Mutex
	queue   []Task
)

const AgentVersion = "v1.1.0"

func main() {
	// 1. 初始化工作目录（files 目录存待打印文件）
	workDir, _ := os.Getwd()
	filesDir := filepath.Join(workDir, "files")
	os.MkdirAll(filesDir, 0755)

	// 2. 启动本地打印消费循环（每2秒扫队列）
	go printWorker(filesDir)

	// 3. 启动热更新器：轮询云端清单（地址/间隔可用环境变量覆盖，默认本地演示地址）
	manifestURL := envDefault("AGENT_MANIFEST_URL", "http://127.0.0.1:8000/agent/manifest.json")
	interval := 60 * time.Second
	if s := os.Getenv("AGENT_UPDATE_INTERVAL_SEC"); s != "" {
		if n := atoiDefault(s, 60); n > 10 {
			interval = time.Duration(n) * time.Second
		}
	}
	startUpdater(manifestURL, interval)
	cleanupOld()

	// 4. 注册 HTTP 路由：页面 + 文件列表 + 打印机列表 + 发任务 + 版本
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 1. 磁盘覆盖优先（override）：exe 同目录有 app.html 用磁盘的（热更通道）
		exePath, err := os.Executable()
		if err == nil {
			if data, err := os.ReadFile(filepath.Join(filepath.Dir(exePath), "app.html")); err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Write(data)
				return
			}
		}
		// 2. 磁盘没有 → 用编译期内嵌页面（保持单文件绿色体验）
		data, _ := embeddedPage.ReadFile("app.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	mux.HandleFunc("/list_files", func(w http.ResponseWriter, r *http.Request) {
		entries, _ := os.ReadDir(filesDir)
		files := []string{}
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, e.Name())
			}
		}
		writeCORS(w)
		json.NewEncoder(w).Encode(files)
	})
	mux.HandleFunc("/list_printers", listPrinters)
	mux.HandleFunc("/app_send_task", sendTask)
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeCORS(w)
		json.NewEncoder(w).Encode(map[string]string{"version": AgentVersion})
	})

	addr := ":18080"
	log.Printf("PC中转端已启动 %s: http://127.0.0.1%s", AgentVersion, addr)
	log.Printf("工作目录: %s | 热更新清单: %s (每%ds检查)", workDir, manifestURL, int(interval.Seconds()))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal("HTTP服务失败: ", err)
	}
}

// printerInfo1W 对应 Win32 PRINTER_INFO_1W 结构（64 位下 32 字节）
// 经最小复现验证：EnumPrintersW level=1 时名称指针可用（level=4 的指针指向独立堆不可用）
type printerInfo1W struct {
	Flags       uint32
	_           uint32
	Description *uint16
	Name        *uint16
	Comment     *uint16
}

// listPrinters 枚举本机打印机供页面下拉选择
func listPrinters(w http.ResponseWriter, r *http.Request) {
	// 1. 调 winspool 枚举本地+连接打印机（PRINTER_INFO_1W）
	winspool := syscall.NewLazyDLL("winspool.drv")
	procEnum := winspool.NewProc("EnumPrintersW")
	flags := uintptr(2 | 4) // PRINTER_ENUM_LOCAL | PRINTER_ENUM_CONNECTIONS
	level := uintptr(1)
	var needed, returned uint32
	r1, _, _ := procEnum.Call(flags, 0, level, 0, 0,
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
	if r1 == 0 && needed == 0 {
		log.Printf("枚举打印机失败: 无可枚举项")
		writeCORS(w)
		json.NewEncoder(w).Encode([]string{})
		return
	}
	buf := make([]byte, needed)
	r1, _, err := procEnum.Call(flags, 0, level, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
	if r1 == 0 {
		log.Printf("枚举打印机失败: %v", err)
		writeCORS(w)
		json.NewEncoder(w).Encode([]string{})
		return
	}
	// 2. 按结构体数组解析，取每台打印机名称
	infos := unsafe.Slice((*printerInfo1W)(unsafe.Pointer(&buf[0])), returned)
	names := make([]string, 0, returned)
	for _, it := range infos {
		if it.Name == nil {
			continue
		}
		// 3. 读 NUL 结尾的 UTF16 字符串（指针指向 buf 内部）
		start := uintptr(unsafe.Pointer(it.Name)) - uintptr(unsafe.Pointer(&buf[0]))
		u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[start])), (uintptr(len(buf))-start)/2)
		for j, c := range u16 {
			if c == 0 {
				names = append(names, syscall.UTF16ToString(u16[:j]))
				break
			}
		}
	}
	log.Printf("枚举到 %d 台打印机: %v", len(names), names)
	writeCORS(w)
	json.NewEncoder(w).Encode(names)
}

// sendTask 模拟APP下发：POST 表单 filename/copies/color/printer
func sendTask(w http.ResponseWriter, r *http.Request) {
	// 1. 仅接受 POST 表单
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.ParseForm()
	t := Task{
		ID:       time.Now().UnixNano() / 1e6 % 1e8,
		Filename: filepath.Base(r.FormValue("filename")),
		Color:    atoiDefault(r.FormValue("color"), 1),
		Copies:   atoiDefault(r.FormValue("copies"), 1),
		Printer:  r.FormValue("printer"),
	}
	// 2. 校验文件存在
	fp := filepath.Join(filesDirOf(), t.Filename)
	if _, err := os.Stat(fp); err != nil {
		writeCORS(w)
		json.NewEncoder(w).Encode(map[string]any{"code": 1, "msg": "files目录不存在: " + t.Filename})
		return
	}
	// 3. 入队等待打印
	queueMu.Lock()
	queue = append(queue, t)
	queueMu.Unlock()
	log.Printf("收到任务 tid=%d file=%s x%d -> [%s]", t.ID, t.Filename, t.Copies, printerName(t.Printer))
	writeCORS(w)
	json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "queued", "tid": t.ID})
}

// printWorker 打印消费循环：出队 → print/printto 送打印机 → 失败回队重试
func printWorker(filesDir string) {
	for {
		// 1. 取一个任务，空则等待
		queueMu.Lock()
		if len(queue) == 0 {
			queueMu.Unlock()
			time.Sleep(2 * time.Second)
			continue
		}
		t := queue[0]
		queue = queue[1:]
		queueMu.Unlock()

		// 2. printto 指定目标打印机；空则 print 送系统默认
		fp := filepath.Join(filesDir, t.Filename)
		verb := syscall.StringToUTF16Ptr("print")
		var param uintptr
		if t.Printer != "" {
			verb = syscall.StringToUTF16Ptr("printto")
			param = uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(`"`+t.Printer+`"`)))
		}
		ret, _, _ := procShellExecuteW.Call(
			0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(fp))),
			param, 0, 0 /*SW_HIDE*/)
		// 3. 返回值<=32 表示 ShellExecute 失败，回队首 10 秒后重试
		if ret <= 32 {
			log.Printf("打印失败 tid=%d (ShellExecute ret=%d)，10秒后重试", t.ID, ret)
			time.Sleep(10 * time.Second)
			queueMu.Lock()
			queue = append([]Task{t}, queue...)
			queueMu.Unlock()
			continue
		}
		log.Printf("已送打印机[%s]: %s x%d tid=%d", printerName(t.Printer), t.Filename, t.Copies, t.ID)
	}
}

func printerName(p string) string {
	if p == "" {
		return "系统默认"
	}
	return p
}

func filesDirOf() string {
	wd, _ := os.Getwd()
	return filepath.Join(wd, "files")
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return def
	}
	return n
}

// writeCORS 允许 file:// 打开页面时跨域调用
func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
}

// envDefault 读取环境变量，空则回退默认值（热更新清单地址/间隔的可配置入口）
func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

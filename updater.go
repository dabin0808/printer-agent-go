//go:build windows

// updater — 云端热更新器：轮询 manifest → 下载 → sha256 校验 → 改名换位 → 空闲重启
// 设计详见 HOTUPDATE.md §3-§4；exe 与页面走同一清单、两条独立通道
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// manifestFile 清单中的单个文件描述（HOTUPDATE.md §2.2 格式）
type manifestFile struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// manifest 云端版本清单
type manifest struct {
	Latest string                  `json:"latest"`
	Files  map[string]manifestFile `json:"files"`
}

// startUpdater 启动周期检查循环（业务先就绪，再进入更新节奏）
func startUpdater(manifestURL string, interval time.Duration) {
	go func() {
		// 1. 启动缓冲 10 秒，让 HTTP 服务与打印线程先就绪
		time.Sleep(10 * time.Second)
		for {
			// 2. 单次检查（失败只记日志，不影响当前打印业务）
			checkUpdate(manifestURL)
			time.Sleep(interval)
		}
	}()
}

// cleanupOld 启动自检通过后清理上次更新残留的 .old（回滚窗口关闭）
func cleanupOld() {
	go func() {
		// 1. 启动 30 秒无异常，认为本次运行健康
		time.Sleep(30 * time.Second)
		exePath, err := os.Executable()
		if err != nil {
			return
		}
		// 2. 删除旧版本备份
		if err := os.Remove(exePath + ".old"); err == nil {
			log.Printf("[更新] 已清理旧版本备份 %s.old", filepath.Base(exePath))
		}
	}()
}

// checkUpdate 单次检查：exe 通道 + 页面通道
func checkUpdate(manifestURL string) {
	// 1. 拉清单
	m, err := fetchManifest(manifestURL)
	if err != nil {
		log.Printf("[更新] 清单拉取失败(沿用当前版本继续服务): %v", err)
		return
	}
	// 2. exe 通道：latest > 当前版本才更新（语义化版本比较）
	if cmpSemver(m.Latest, AgentVersion) > 0 {
		if mf, ok := m.Files["agent"]; ok {
			if err := updateAgent(mf, m.Latest); err != nil {
				log.Printf("[更新] exe 更新未完成(下轮重试): %v", err)
			}
		}
	}
	// 3. 页面通道：sha256 与磁盘不一致即热更（无版本概念、无需重启）
	if pf, ok := m.Files["page"]; ok {
		if err := updatePage(pf); err != nil {
			log.Printf("[更新] 页面更新未完成(下轮重试): %v", err)
		}
	}
}

// fetchManifest 拉取并解析清单
func fetchManifest(url string) (*manifest, error) {
	// 1. GET 清单（超时 10 秒，内网/公网都够用）
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	// 2. 解析 JSON
	var m manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// updateAgent exe 更新：下载→校验→改名换位→空闲时拉起新进程退出旧进程
func updateAgent(mf manifestFile, newVer string) error {
	// 1. 下载到临时名 .new（失败/校验不过都不污染正式文件）
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	newPath := exePath + ".new"
	if err := downloadVerify(mf, newPath); err != nil {
		os.Remove(newPath)
		return err
	}
	// 2. 打印队列非空则推迟（重启不能打断正在打印的单据）
	queueMu.Lock()
	busy := len(queue) > 0
	queueMu.Unlock()
	if busy {
		return fmt.Errorf("打印队列非空，推迟换位")
	}
	// 3. 改名换位：Windows 运行中 exe 不可覆盖、但可改名
	oldPath := exePath + ".old"
	os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		return err
	}
	if err := os.Rename(newPath, exePath); err != nil {
		os.Rename(oldPath, exePath) // 换位失败立即回滚
		return err
	}
	log.Printf("[更新] %s → %s 换位完成，拉起新进程...", AgentVersion, newVer)
	// 4. 启动新进程（继承控制台句柄，日志连续可见）
	child, err := os.StartProcess(exePath, os.Args, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		// 5. 拉起失败：回滚换位，旧进程继续服务
		os.Remove(exePath)
		os.Rename(oldPath, exePath)
		return fmt.Errorf("启动新进程失败已回滚: %w", err)
	}
	_ = child
	log.Printf("[更新] 新进程已启动，旧进程(%s)退出", AgentVersion)
	os.Exit(0)
	return nil
}

// updatePage 页面热更：磁盘 app.html 与清单 sha256 不一致才下载覆盖
func updatePage(pf manifestFile) error {
	// 1. 定位磁盘页面（exe 同目录，/ 路由磁盘优先）
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	pagePath := filepath.Join(filepath.Dir(exePath), "app.html")
	// 2. 磁盘已有且哈希一致 → 已是最新
	if cur, err := sha256OfFile(pagePath); err == nil && cur == pf.SHA256 {
		return nil
	}
	// 3. 下载到临时名再覆盖（先删旧文件，Windows 允许改名替换）
	tmp := pagePath + ".new"
	if err := downloadVerify(pf, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(pagePath)
	if err := os.Rename(tmp, pagePath); err != nil {
		return err
	}
	log.Printf("[更新] 页面 app.html 已热更 → 浏览器刷新即生效（无需重启）")
	return nil
}

// downloadVerify 下载文件并强校验 sha256，通过才落盘为 dest
func downloadVerify(mf manifestFile, dest string) error {
	// 1. 流式下载到临时文件
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(mf.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("下载 http %d: %s", resp.StatusCode, mf.URL)
	}
	tmp := dest + ".dl"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	// 2. sha256 强校验（防传输损坏与篡改）
	got, err := sha256OfFile(tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got != mf.SHA256 {
		os.Remove(tmp)
		return fmt.Errorf("sha256 不符: got=%s want=%s", got[:12], mf.SHA256[:12])
	}
	// 3. 校验通过才移动到正式临时名
	os.Remove(dest)
	return os.Rename(tmp, dest)
}

// sha256OfFile 流式计算文件 sha256（十六进制小写）
func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cmpSemver 语义化版本比较：>0 表示 a 更新（v1.10.0 > v1.9.0，字符串比较会错）
func cmpSemver(a, b string) int {
	pa, pb := parseSemver(a), parseSemver(b)
	// 1. 逐段比较 主.次.补丁
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// parseSemver 解析 "v1.2.3" → [1,2,3]，脏值按 0 处理
func parseSemver(s string) [3]int {
	var out [3]int
	// 1. 去前缀 v
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	// 2. 按点分段取数字
	parts := strings.Split(s, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n := 0
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}

// 热迁移实时演示服务（单文件，仅标准库）
//
// 运行在「当前持有虚拟机」的那台宿主机上（libvirt 本地 socket 所在机器）。
// 功能：
//   - 列出本机所有虚拟机
//   - 发起真实的热迁移：virsh migrate --live --persistent --copy-storage-*
//   - 每秒采样 virsh domjobinfo，通过 SSE 实时推给浏览器
//   - 浏览器展示真实的进度 / 速率 / 迭代 / 停机时间
package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var staticFS embed.FS

// ---------- 配置 ----------

var (
	listenAddr   = flag.String("listen", ":8000", "监听地址")
	defaultVM    = flag.String("vm", "", "默认虚拟机名")
	defaultTgt   = flag.String("target", "", "默认迁移目标，如 qemu+ssh://root@10.19.207.10/system?port=55555")
	samplePeriod = flag.Duration("interval", 1*time.Second, "domjobinfo 采样周期")
)

// ---------- 数据结构 ----------

// Sample 是某一时刻 virsh domjobinfo 解析出来的一组真实数字。
type Sample struct {
	Time          string  `json:"time"`
	Status        string  `json:"status"`
	ElapsedS      float64 `json:"elapsed_s"`
	DataProcessed float64 `json:"data_processed"` // MiB
	DataTotal     float64 `json:"data_total"`     // MiB
	DataRemaining float64 `json:"data_remaining"` // MiB
	MemBandwidth  float64 `json:"mem_bandwidth"`  // MiB/s
	DiskBandwidth float64 `json:"disk_bandwidth"` // MiB/s
	DirtyRate     float64 `json:"dirty_rate"`     // MiB/s
	Iteration     int     `json:"iteration"`
	DowntimeMs    float64 `json:"downtime_ms"`
}

// State 是推给前端的完整状态快照。
type State struct {
	Sample  Sample   `json:"sample"`
	Running bool     `json:"running"`
	VM      string   `json:"vm"`
	Target  string   `json:"target"`
	Error   string   `json:"error"`
	Logs    []string `json:"logs"`
}

// App 保存服务端内存状态。
type App struct {
	mu      sync.Mutex
	sample  Sample
	running bool
	vm      string
	target  string
	lastErr string
	logs    []string
	cancel  context.CancelFunc
	startAt time.Time
}

func newApp() *App {
	return &App{logs: make([]string, 0, 256)}
}

func (a *App) appendLog(s string) {
	line := time.Now().Format("15:04:05") + "  " + strings.TrimRight(s, "\n")
	log.Println(line) // 关键：同时写到进程 stdout，便于 nohup 落盘排查
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, line)
	if len(a.logs) > 200 {
		a.logs = a.logs[len(a.logs)-200:]
	}
}

func (a *App) snapshot() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	logs := make([]string, len(a.logs))
	copy(logs, a.logs)
	return State{
		Sample:  a.sample,
		Running: a.running,
		VM:      a.vm,
		Target:  a.target,
		Error:   a.lastErr,
		Logs:    logs,
	}
}

// ---------- virsh 调用 ----------

// runVirsh 执行一条 virsh 命令并返回 stdout。
func runVirsh(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "virsh", args...)
	out, err := cmd.Output()
	return string(out), err
}

// listVMs 返回本机全部虚拟机名（含关机）。
func listVMs() ([]string, error) {
	out, err := runVirsh(context.Background(), "list", "--all", "--name")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// ---------- domjobinfo 解析 ----------

var reNum = regexp.MustCompile(`^([0-9.]+)\s*([A-Za-z/]*)`)

// parseMiB 把 "5.123 GiB" / "120.5 MiB/s" 之类换算成 MiB（速率单位去掉 /s）。
func parseMiB(v string) float64 {
	m := reNum.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	unit := strings.ToLower(strings.TrimSuffix(m[2], "/s"))
	switch unit {
	case "gib", "gb":
		n *= 1024
	case "tib", "tb":
		n *= 1024 * 1024
	case "kib", "kb":
		n /= 1024
	case "b":
		n /= 1024 * 1024
	}
	return n
}

// parseHMS 把 "00:00:12" 解析成秒。
func parseHMS(v string) float64 {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 3 {
		return 0
	}
	h, _ := strconv.ParseFloat(parts[0], 64)
	m, _ := strconv.ParseFloat(parts[1], 64)
	s, _ := strconv.ParseFloat(parts[2], 64)
	return h*3600 + m*60 + s
}

// parseMillis 把 "123 ms" / "0.123 s" / "1.2 s" 统一成毫秒。
func parseMillis(v string) float64 {
	m := reNum.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	if strings.ToLower(m[2]) == "s" || strings.ToLower(m[2]) == "sec" {
		n *= 1000
	}
	return n
}

// parseJobInfo 解析 virsh domjobinfo 的输出。found=false 表示当前没有迁移任务。
func parseJobInfo(out string) (Sample, bool) {
	s := Sample{Time: time.Now().Format("15:04:05")}
	found := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		switch k {
		case "Job type":
			s.Status = v
			found = true
		case "Time elapsed":
			s.ElapsedS = parseHMS(v)
		case "Data processed":
			s.DataProcessed = parseMiB(v)
		case "Data total":
			s.DataTotal = parseMiB(v)
		case "Data remaining":
			s.DataRemaining = parseMiB(v)
		case "Memory bandwidth":
			s.MemBandwidth = parseMiB(v)
		case "Disk bandwidth":
			s.DiskBandwidth = parseMiB(v)
		case "Dirty rate":
			s.DirtyRate = parseMiB(v)
		case "Iteration":
			s.Iteration, _ = strconv.Atoi(v)
		case "Total downtime":
			s.DowntimeMs = parseMillis(v)
		}
	}
	return s, found
}

// ---------- 迁移执行 ----------

// startMigration 在后台执行一次真实热迁移（三次尝试：copy-storage-* 旧参数 → --disks-uri → 共享存储）。
func (a *App) startMigration(vm, target, copyMode string, bandwidth int) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return fmt.Errorf("已有迁移任务在进行中")
	}
	if vm == "" || target == "" {
		a.mu.Unlock()
		return fmt.Errorf("虚拟机与目标地址不能为空")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.running = true
	a.vm = vm
	a.target = target
	a.lastErr = ""
	a.logs = a.logs[:0]
	a.sample = Sample{Status: "starting", Time: time.Now().Format("15:04:05")}
	a.startAt = time.Now()
	a.mu.Unlock()

	go a.runMigration(ctx, vm, target, copyMode, bandwidth)
	return nil
}

// parseSSHTarget 从 qemu+ssh://user@host:port/system?port=55555 里拆出 host 与 port。
// 注意：查询串在 path 之后，必须先取 ?query 再从剩余部分切 /path。
func parseSSHTarget(target string) (host, port string) {
	host, port = "", "22"
	s := target
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// 先摘掉查询串（?port=55555&...）
	if i := strings.Index(s, "?"); i >= 0 {
		q := s[i+1:]
		s = s[:i]
		for _, kv := range strings.Split(q, "&") {
			p := strings.SplitN(kv, "=", 2)
			if len(p) == 2 && p[0] == "port" {
				port = p[1]
			}
		}
	}
	// 再去掉 /system 之类的路径
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	// uri 里显式写的 host:port 优先
	if h, p, err := net.SplitHostPort(s); err == nil {
		return h, p
	}
	return s, port
}

// runMigration 执行迁移，并在发起前做一次“能不能迁”的硬检查。
func (a *App) runMigration(ctx context.Context, vm, target, copyMode string, bandwidth int) {
	// 预检：PCI 直通设备在原理上不可迁移，先查出来给出人话解释。
	if reason, blocked := checkMigrationBlockers(vm); blocked {
		a.appendLog("预检不通过: " + reason)
		a.finish("failed", reason)
		return
	}

	base := []string{"migrate", "--live", "--persistent", "--verbose"}
	if bandwidth > 0 {
		base = append(base, "--bandwidth", strconv.Itoa(bandwidth))
	}

	type attempt struct {
		name string
		args []string
	}
	var attempts []attempt
	switch copyMode {
	case "all", "inc":
		flag := "--copy-storage-all"
		if copyMode == "inc" {
			flag = "--copy-storage-inc"
		}
		attempts = append(attempts, attempt{"copy-storage", append(append([]string{}, base...), flag, vm, target)})
	default:
		attempts = append(attempts, attempt{"共享存储", append(append([]string{}, base...), vm, target)})
	}

	lastErrs := make([]string, 0, len(attempts))
	for _, at := range attempts {
		if ctx.Err() != nil {
			a.finish("cancelled", "迁移已被取消")
			return
		}
		a.appendLog("尝试 [" + at.name + "]: virsh " + strings.Join(at.args, " "))
		err := a.runOne(ctx, vm, at.args)
		if err == nil {
			a.finish("completed", "")
			return
		}
		if ctx.Err() != nil {
			a.finish("cancelled", "迁移已被取消")
			return
		}
		a.appendLog("该方式失败: " + err.Error())
		lastErrs = append(lastErrs, "["+at.name+"] "+err.Error())
	}
	a.finish("failed", strings.Join(lastErrs, " | "))
}

// checkMigrationBlockers 检查原理上不可热迁移的配置，返回人话原因。
// 覆盖：PCI 直通（hostdev）、快照链。
func checkMigrationBlockers(vm string) (string, bool) {
	xmlText, err := runVirsh(context.Background(), "dumpxml", vm)
	if err != nil {
		return "", false // 拿不到 XML 就交给后续步骤报错
	}
	if strings.Contains(xmlText, "mode='subsystem'") || strings.Contains(xmlText, `mode="subsystem"`) {
		return fmt.Sprintf("虚拟机 %s 挂载了 PCI 直通设备（<hostdev mode='subsystem'>）。"+
			"直通设备物理绑定在源宿主机上，libvirt 不支持带直通的虚拟机热迁移。"+
			"请先在虚拟机 XML 中移除该 hostdev 设备并重启虚拟机后再迁移；"+
			"若业务必须使用直通，则该虚拟机不适合热迁移。", vm), true
	}
	// 快照：磁盘变成 backing file 链，只搬最上层会在目标端断链。
	if out, err := runVirsh(context.Background(), "snapshot-list", vm); err == nil {
		cnt := 0
		for _, line := range strings.Split(out, "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "Name") || strings.HasPrefix(t, "---") {
				continue
			}
			cnt++
		}
		if cnt > 0 {
			return fmt.Sprintf("虚拟机 %s 存在 %d 个快照，磁盘为链式结构（qcow2 backing file），"+
				"libvirt 不支持迁移带快照的虚拟机。请先合并快照："+
				"virsh blockcommit %s <磁盘> --active --pivot", vm, cnt, vm), true
		}
	}
	return "", false
}

// runOne 执行一次 virsh migrate 并伴随采样，返回错误。
func (a *App) runOne(ctx context.Context, vm string, args []string) error {
	cmd := exec.CommandContext(ctx, "virsh", args...)
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	stdout, _ := cmd.StdoutPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 virsh 失败: %w", err)
	}

	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()
	go a.pollLoop(pollCtx, vm)
	go scanLines(stdout, func(l string) {
		if l = strings.TrimSpace(l); l != "" {
			a.appendLog(l)
		}
	})

	err := cmd.Wait()
	pollCancel()

	if err != nil {
		msg := strings.TrimSpace(stderrBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	a.finalSample(vm) // 补采最终统计（downtime 等）
	return nil
}

// pollLoop 每 interval 采样一次 domjobinfo，直到迁移结束。
func (a *App) pollLoop(ctx context.Context, vm string) {
	t := time.NewTicker(*samplePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			out, err := runVirsh(ctx, "domjobinfo", vm)
			if err != nil {
				return // 虚拟机已离开源端或无任务，由主流程收尾
			}
			s, ok := parseJobInfo(out)
			if !ok {
				return
			}
			a.mu.Lock()
			a.sample = s
			a.mu.Unlock()
		}
	}
}

// finalSample 在 virsh migrate 返回后补采最后一次任务统计。
// 迁移完成时虚拟机已从源端消失，domjobinfo 通常查不到；
// 但 libvirt 会在短时间内保留任务记录，多试几次能拿到最终的 Total time / Downtime。
func (a *App) finalSample(vm string) {
	for i := 0; i < 3; i++ {
		time.Sleep(300 * time.Millisecond)
		out, err := runVirsh(context.Background(), "domjobinfo", vm)
		if err != nil {
			continue
		}
		if s, ok := parseJobInfo(out); ok {
			a.mu.Lock()
			a.sample = s
			a.mu.Unlock()
			return
		}
	}
}

func (a *App) finish(status, errMsg string) {
	a.mu.Lock()
	a.running = false
	a.sample.Status = status
	a.lastErr = errMsg
	// 迁移结束：切换发生在最后一次采样之后，收尾时把进度补齐、速率归零，
	// 否则界面会停在最后一次采样值（典型现象：进度 93%、速率仍显示 100+MiB/s）。
	if status == "completed" {
		a.sample.DataProcessed = a.sample.DataTotal
		a.sample.DataRemaining = 0
		a.sample.MemBandwidth = 0
		a.sample.DiskBandwidth = 0
		a.sample.DirtyRate = 0
	}
	if status == "failed" || status == "cancelled" {
		a.sample.MemBandwidth = 0
		a.sample.DiskBandwidth = 0
	}
	a.mu.Unlock()
	if errMsg != "" {
		a.appendLog("[" + status + "] " + errMsg)
	} else {
		a.appendLog("[" + status + "] 迁移结束")
	}
}

// ---------- HTTP ----------

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (a *App) handleVMs(w http.ResponseWriter, r *http.Request) {
	names, err := listVMs()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"vms": names})
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.snapshot())
}

// handleStream 用 SSE 每秒推一次全量状态。
func (a *App) handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", 500)
		return
	}
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			b, _ := json.Marshal(a.snapshot())
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		VM        string `json:"vm"`
		Target    string `json:"target"`
		Copy      string `json:"copy"`
		Bandwidth int    `json:"bandwidth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if req.Copy == "" {
		req.Copy = "all"
	}
	if req.Copy == "none" {
		req.Copy = "none" // 共享存储：不加任何 copy 参数
	}
	if err := a.startMigration(req.VM, req.Target, req.Copy, req.Bandwidth); err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel == nil {
		writeJSON(w, 409, map[string]any{"error": "当前没有迁移任务"})
		return
	}
	cancel()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// handleDiag 是“自助诊断”接口：把页面报错最常见的原因一次性查完。
// 不需要你登录两台机器逐条敲，浏览器打开 /api/diag 就能看到结论。
func (a *App) handleDiag(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		a.mu.Lock()
		target = a.target
		a.mu.Unlock()
	}
	if target == "" {
		target = "qemu+ssh://root@10.19.207.34/system?port=55555"
	}
	host, port := parseSSHTarget(target)

	type item struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Msg  string `json:"msg"`
	}
	var items []item
	add := func(name string, ok bool, msg string) {
		items = append(items, item{name, ok, msg})
	}

	// 1. 本机 virsh 是否可用
	if out, err := runVirsh(context.Background(), "list", "--all", "--name"); err != nil {
		add("本机 libvirt", false, "virsh 执行失败: "+err.Error())
	} else {
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		add("本机 libvirt", true, fmt.Sprintf("正常，本机有 %d 台虚拟机", n))
	}

	// 2. 目标 SSH 免密（迁移前提）
	sshArgs := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=5", "-p", port, "root@" + host, "hostname"}
	if out, err := exec.Command("ssh", sshArgs...).Output(); err != nil {
		add("目标 SSH 免密", false, "不通："+err.Error()+"（需在源端执行 ssh-copy-id -p "+port+" root@"+host+"）")
	} else {
		add("目标 SSH 免密", true, "正常，目标主机名: "+strings.TrimSpace(string(out)))
	}

	// 3. 目标端 libvirtd 数据通道 16509
	//    注意：qemu+ssh 方式下，控制与数据都走 SSH 隧道，16509 并不是必需的。
	//    只有改用 tcp:// 直连时才需要，所以这里只做提示，不计入失败项。
	//    真正必需的是 virsh migrate 自己的数据端口（默认 49152-49215，或 --migrateuri 指定）。
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "16509"), 3*time.Second)
	if err != nil {
		add("目标数据通道 16509", true, "未监听（qemu+ssh 方式下正常，走 SSH 隧道即可）")
	} else {
		conn.Close()
		add("目标数据通道 16509", true, "端口可达（若改用 tcp:// 直连可用）")
	}

	// 4. 目标端 libvirt 是否认识这台机器（用 SSH 远程执行 virsh）
	verifyArgs := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
		"-p", port, "root@" + host, "virsh version"}
	if out, err := exec.Command("ssh", verifyArgs...).Output(); err != nil {
		detail := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		add("目标 libvirt", false, "远程 virsh 执行失败："+detail)
	} else {
		add("目标 libvirt", true, strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " / "))
	}

	okAll := true
	for _, it := range items {
		if !it.OK {
			okAll = false
		}
	}
	writeJSON(w, 200, map[string]any{"target": target, "all_ok": okAll, "items": items})
}

func scanLines(rc io.Reader, fn func(string)) {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fn(sc.Text())
	}
}

func main() {
	flag.Parse()
	app := newApp()
	app.vm = *defaultVM
	app.target = *defaultTgt

	http.HandleFunc("/", app.handleIndex)
	http.HandleFunc("/api/vms", app.handleVMs)
	http.HandleFunc("/api/state", app.handleState)
	http.HandleFunc("/api/stream", app.handleStream)
	http.HandleFunc("/api/migrate", app.handleMigrate)
	http.HandleFunc("/api/cancel", app.handleCancel)
	http.HandleFunc("/api/diag", app.handleDiag)

	log.Printf("热迁移演示服务启动: http://0.0.0.0%s", *listenAddr)
	log.Printf("默认目标: %s", *defaultTgt)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

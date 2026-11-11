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
	"net/netip"
	"net/url"
	"os"
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
	Measurement   Measurement `json:"-"`
	Time          string      `json:"time"`
	SourceCheck   string      `json:"source_check,omitempty"`
	TargetCheck   string      `json:"target_check,omitempty"`
	Verified      bool        `json:"verified"`
	Phase         string      `json:"phase"`
	Status        string      `json:"status"`
	ElapsedS      float64     `json:"elapsed_s"`
	DataProcessed float64     `json:"data_processed"` // MiB
	DataTotal     float64     `json:"data_total"`     // MiB
	DataRemaining float64     `json:"data_remaining"` // MiB
	MemBandwidth  float64     `json:"mem_bandwidth"`  // MiB/s
	DiskBandwidth float64     `json:"disk_bandwidth"` // MiB/s
	DirtyRate     float64     `json:"dirty_rate"`     // MiB/s
	Iteration     int         `json:"iteration"`
	DowntimeMs    float64     `json:"downtime_ms"`
}

// Measurement keeps values absent when the hypervisor did not report them.
type Measurement struct {
	JobElapsedMS     *float64  `json:"job_elapsed_ms"`
	SetupMS          *float64  `json:"setup_ms"`
	At               time.Time `json:"at"`
	MemoryMiB        *float64  `json:"memory_mib"`
	DiskMiB          *float64  `json:"disk_mib"`
	DiskTotalMiB     *float64  `json:"disk_total_mib"`
	DiskRemainingMiB *float64  `json:"disk_remaining_mib"`
	MemoryRate       *float64  `json:"memory_mib_per_s"`
	DiskRate         *float64  `json:"disk_mib_per_s"`
	DowntimeMS       *float64  `json:"downtime_ms"`
}

type BusinessProbe struct {
	Successes       int      `json:"successes"`
	Failures        int      `json:"failures"`
	FailureWindowMS *float64 `json:"failure_window_ms"`
	BaselineOK      bool     `json:"baseline_ok"`
	Recovered       bool     `json:"recovered"`
	FailedAtEnd     bool     `json:"failed_at_end"`
}

type MigrationRecord struct {
	Business              *BusinessProbe `json:"business"`
	Started               time.Time      `json:"started"`
	CopyMode              string         `json:"copy_mode"`
	Bandwidth             int            `json:"bandwidth_limit_mib_per_s"`
	CommandStarted        *time.Time     `json:"command_started"`
	CommandEnded          *time.Time     `json:"command_ended"`
	TargetRunningObserved *time.Time     `json:"target_running_observed"`
	QEMU                  QEMUTiming     `json:"qemu"`
	Final                 *Measurement   `json:"final"`
	Samples               []Measurement  `json:"samples"`
}

type QEMUTiming struct {
	TotalMS    *float64 `json:"total-time"`
	SetupMS    *float64 `json:"setup-time"`
	DowntimeMS *float64 `json:"downtime"`
	Status     string   `json:"status"`
}

// State 是推给前端的完整状态快照。
type State struct {
	DowntimeAvailable bool     `json:"downtime_available"`
	Sample            Sample   `json:"sample"`
	Running           bool     `json:"running"`
	VM                string   `json:"vm"`
	Target            string   `json:"target"`
	Error             string   `json:"error"`
	Logs              []string `json:"logs"`
}

// App 保存服务端内存状态。
type App struct {
	record  MigrationRecord
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
		DowntimeAvailable: a.sample.Measurement.DowntimeMS != nil,
		Sample:            a.sample,
		Running:           a.running,
		VM:                a.vm,
		Target:            a.target,
		Error:             a.lastErr,
		Logs:              logs,
	}
}

// ---------- virsh 调用 ----------

// runVirsh 执行一条 virsh 命令并返回 stdout。
func runVirsh(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "virsh", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
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

func measured(value string, parse func(string) float64) *float64 {
	if !reNum.MatchString(value) {
		return nil
	}
	result := parse(value)
	return &result
}

// parseJobInfo 解析 virsh domjobinfo 的输出。found=false 表示当前没有可取消的迁移任务。
func parseJobInfo(out string) (Sample, bool) {
	s := Sample{Time: time.Now().Format("15:04:05")}
	s.Measurement.At = time.Now()
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
			if strings.Contains(v, ":") {
				s.ElapsedS = parseHMS(v)
				elapsed := s.ElapsedS * 1000
				s.Measurement.JobElapsedMS = &elapsed
			} else {
				s.Measurement.JobElapsedMS = measured(v, parseMillis)
				s.ElapsedS = parseMillis(v) / 1000
			}
		case "Setup time":
			s.Measurement.SetupMS = measured(v, parseMillis)
		case "Data processed":
			s.DataProcessed = parseMiB(v)
		case "Data total":
			s.DataTotal = parseMiB(v)
		case "Data remaining":
			s.DataRemaining = parseMiB(v)
		case "Memory processed":
			s.Measurement.MemoryMiB = measured(v, parseMiB)
		case "File processed", "Disk processed":
			s.Measurement.DiskMiB = measured(v, parseMiB)
		case "File total", "Disk total":
			s.Measurement.DiskTotalMiB = measured(v, parseMiB)
		case "File remaining", "Disk remaining":
			s.Measurement.DiskRemainingMiB = measured(v, parseMiB)
		case "Memory bandwidth":
			s.MemBandwidth = parseMiB(v)
			s.Measurement.MemoryRate = measured(v, parseMiB)
		case "Disk bandwidth":
			s.DiskBandwidth = parseMiB(v)
			s.Measurement.DiskRate = measured(v, parseMiB)
		case "Dirty rate":
			s.DirtyRate = parseMiB(v)
		case "Iteration", "Memory iteration":
			s.Iteration, _ = strconv.Atoi(v)
		case "Total downtime", "Downtime":
			s.DowntimeMs = parseMillis(v)
			s.Measurement.DowntimeMS = measured(v, parseMillis)
		}
	}
	return s, found
}

// ---------- 迁移执行 ----------

// startMigration 在后台执行一次真实热迁移（三次尝试：copy-storage-* 旧参数 → --disks-uri → 共享存储）。
func (a *App) startMigration(vm, target, copyMode, dataURI, probeURL string, bandwidth int) error {
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
	a.sample = Sample{Phase: "checking", Status: "starting", Time: time.Now().Format("15:04:05")}
	a.startAt = time.Now()
	a.record = MigrationRecord{Started: a.startAt, CopyMode: copyMode, Bandwidth: bandwidth, Samples: []Measurement{}}
	a.mu.Unlock()

	go a.runMigration(ctx, vm, target, copyMode, dataURI, probeURL, bandwidth)
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
func (a *App) runMigration(ctx context.Context, vm, target, copyMode, dataURI, probeURL string, bandwidth int) {
	// 预检：PCI 直通设备在原理上不可迁移，先查出来给出人话解释。
	if reason, blocked := checkMigrationBlockers(ctx, vm); blocked {
		a.appendLog("预检不通过: " + reason)
		a.finish("failed", reason)
		return
	}

	a.mu.Lock()
	a.sample.Phase = "target_check"
	a.mu.Unlock()
	_, err := queryVirsh(ctx, "--readonly", "--connect", target, "hostname")
	if ctx.Err() != nil {
		a.finish("cancelled", "迁移已被取消")
		return
	}
	if err != nil {
		a.appendLog("目标控制连接预检失败：" + err.Error())
		a.finish("failed", "目标控制连接预检失败："+migrationError(err.Error()))
		return
	}
	a.appendLog("目标 libvirt 控制连接预检通过")
	if dataURI != "" {
		a.appendLog("QEMU 数据连接地址：" + dataURI)
	}

	base := []string{"migrate", "--live", "--persistent", "--verbose"}
	if dataURI != "" {
		base = append(base, "--migrateuri", dataURI)
	}
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

	var stopProbe func()
	if probeURL != "" {
		stopProbe = a.startBusinessProbe(ctx, probeURL)
		defer stopProbe()
	}

	lastErrs := make([]string, 0, len(attempts))
	for _, at := range attempts {
		if ctx.Err() != nil {
			if stopProbe != nil {
				stopProbe()
			}
			a.finish("cancelled", "迁移已被取消")
			return
		}
		a.appendLog("尝试 [" + at.name + "]: virsh " + strings.Join(at.args, " "))
		a.mu.Lock()
		a.sample.Phase = "connecting"
		a.mu.Unlock()
		err := a.runOne(ctx, vm, at.args)
		if err == nil {
			a.verifyPlacement(vm, target)
			if stopProbe != nil {
				stopProbe()
			}
			a.finish("completed", "")
			return
		}
		if ctx.Err() != nil {
			if stopProbe != nil {
				stopProbe()
			}
			a.finish("cancelled", "迁移已被取消")
			return
		}
		a.appendLog("该方式失败: " + err.Error())
		lastErrs = append(lastErrs, migrationError(err.Error()))
	}
	if stopProbe != nil {
		stopProbe()
	}
	a.finish("failed", strings.Join(lastErrs, " | "))
}

// startBusinessProbe takes a baseline before migration and samples until the result is ready.
func (a *App) startBusinessProbe(parent context.Context, endpoint string) func() {
	ctx, cancel := context.WithCancel(parent)
	client := &http.Client{
		Timeout:       500 * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	result := BusinessProbe{}
	var firstFailure time.Time
	probe := func() bool {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Cache-Control", "no-cache")
		res, err := client.Do(req)
		ok := false
		if err == nil {
			res.Body.Close()
			ok = res.StatusCode >= 200 && res.StatusCode < 300
		}
		if ctx.Err() != nil {
			return false
		}
		if ok {
			result.Successes++
			if !firstFailure.IsZero() {
				ms := float64(time.Since(firstFailure).Microseconds()) / 1000
				if result.FailureWindowMS == nil || ms > *result.FailureWindowMS {
					result.FailureWindowMS = &ms
				}
				result.Recovered = true
				firstFailure = time.Time{}
			}
		} else {
			result.Failures++
			if firstFailure.IsZero() {
				firstFailure = start
			}
		}
		return ok
	}
	result.BaselineOK = probe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				probe()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			client.CloseIdleConnections()
			result.FailedAtEnd = !firstFailure.IsZero()
			a.mu.Lock()
			a.record.Business = &result
			a.mu.Unlock()
		})
	}
}

// verifyPlacement checks both hosts after virsh migrate has returned successfully.
func (a *App) verifyPlacement(vm, target string) {
	a.mu.Lock()
	a.sample.Phase = "verifying"
	a.mu.Unlock()

	source, sourceErr := queryVirsh(context.Background(), "list", "--name")
	destination, targetErr := queryVirsh(context.Background(), "--connect", target, "domstate", "--domain", vm)
	sourceActive := false
	for _, name := range strings.Split(source, "\n") {
		if strings.TrimSpace(name) == vm {
			sourceActive = true
			break
		}
	}

	sourceCheck := "未发现该虚拟机在源端运行"
	switch {
	case sourceErr != nil:
		sourceCheck = "无法查询源端状态"
		a.appendLog("源端状态核验失败：" + sourceErr.Error())
	case sourceActive:
		sourceCheck = "虚拟机仍在源端活动"
	}
	targetRunning := targetErr == nil && strings.TrimSpace(destination) == "running"
	targetCheck := "已确认虚拟机在目标端运行"
	switch {
	case targetErr != nil:
		targetCheck = "无法查询目标端状态"
		a.appendLog("目标端状态核验失败：" + targetErr.Error())
	case !targetRunning:
		targetCheck = "目标端虚拟机未处于运行状态"
		a.appendLog("目标端虚拟机原始状态：" + strings.TrimSpace(destination))
	}

	a.mu.Lock()
	a.sample.SourceCheck = sourceCheck
	a.sample.TargetCheck = targetCheck
	if targetRunning {
		observed := time.Now()
		a.record.TargetRunningObserved = &observed
	}
	a.sample.Verified = sourceErr == nil && !sourceActive && targetRunning
	a.mu.Unlock()
}

func queryVirsh(parent context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "virsh", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("virsh 操作超时")
	}
	if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(exitErr.Stderr)))
	}
	return string(out), err
}

// migrationError returns a Chinese summary while the log keeps the original diagnostic.
func migrationError(raw string) string {
	message := strings.ToLower(raw)
	for _, rule := range []struct {
		terms   []string
		summary string
	}{
		{[]string{"host key verification", "remote host identification has changed"}, "目标主机的 SSH 密钥校验失败"},
		{[]string{"permission denied", "authentication failed", "authentication unavailable", "no supported authentication methods"}, "认证失败或权限不足"},
		{[]string{"connection refused"}, "目标服务拒绝连接"},
		{[]string{"timed out", "timeout", "操作超时"}, "迁移连接或操作超时"},
		{[]string{"no route to host", "network is unreachable"}, "目标主机网络不可达"},
		{[]string{"could not resolve", "name or service not known"}, "无法解析目标主机名"},
		{[]string{"domain not found", "failed to get domain"}, "找不到源虚拟机"},
		{[]string{"domain is not running", "domain is not active"}, "源虚拟机未运行"},
		{[]string{"already exists", "already defined"}, "目标端已有同名资源"},
		{[]string{"no space left"}, "存储空间不足"},
		{[]string{"cannot allocate memory", "out of memory"}, "宿主机内存不足"},
		{[]string{"incompatible", "cpu model"}, "源端与目标端配置不兼容"},
		{[]string{"启动 virsh 失败"}, "无法启动 virsh，请检查程序是否已安装"},
		{[]string{"no such file", "failed to open disk"}, "所需文件或磁盘不可用"},
		{[]string{"unsupported", "not supported"}, "当前环境不支持所请求的迁移操作"},
		{[]string{"failed to connect", "connection reset", "broken pipe"}, "无法建立或保持迁移连接"},
	} {
		for _, term := range rule.terms {
			if strings.Contains(message, term) {
				return rule.summary + "。原始诊断见迁移日志。"
			}
		}
	}
	return "迁移执行失败。原始诊断见迁移日志。"
}

// checkMigrationBlockers 检查原理上不可热迁移的配置，返回人话原因。
// 覆盖：PCI 直通（hostdev）、快照链。
func checkMigrationBlockers(ctx context.Context, vm string) (string, bool) {
	xmlText, err := runVirsh(ctx, "dumpxml", vm)
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
	if out, err := runVirsh(ctx, "snapshot-list", vm); err == nil {
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

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 virsh 失败: %w", err)
	}

	a.mu.Lock()
	a.record.CommandStarted = &started
	a.mu.Unlock()
	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()
	go a.pollLoop(pollCtx, vm)
	go scanLines(stdout, func(l string) {
		if l = strings.TrimSpace(l); l != "" {
			a.appendLog(l)
		}
	})

	err := cmd.Wait()
	ended := time.Now()
	pollCancel()
	a.mu.Lock()
	a.record.CommandEnded = &ended
	a.mu.Unlock()

	if err != nil {
		msg := strings.TrimSpace(stderrBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	a.mu.Lock()
	a.cancel = nil
	a.sample.Phase = "finalizing"
	a.mu.Unlock()
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
				continue
			}
			s, ok := parseJobInfo(out)
			if !ok {
				continue
			}
			a.mu.Lock()
			if ctx.Err() == nil && !strings.EqualFold(s.Status, "none") {
				s.Status = "active"
				s.Phase = "transferring"
				a.sample = s
				a.record.Samples = append(a.record.Samples, s.Measurement)
			}
			a.mu.Unlock()
		}
	}
}

// finalSample reads libvirt's completed job and QEMU's completed migration stats.
func (a *App) finalSample(vm string) {
	out, err := queryVirsh(context.Background(), "domjobinfo", "--completed", "--keep-completed", "--domain", vm)
	if s, ok := parseJobInfo(out); err == nil && ok && strings.EqualFold(s.Status, "completed") {
		a.mu.Lock()
		s.Phase, s.Status = a.sample.Phase, a.sample.Status
		a.sample = s
		a.record.Final = &s.Measurement
		a.mu.Unlock()
	}
	out, err = queryVirsh(context.Background(), "qemu-monitor-command", "--domain", vm, `{"execute":"query-migrate"}`)
	var response struct {
		Return QEMUTiming `json:"return"`
	}
	if err == nil && json.Unmarshal([]byte(out), &response) == nil && response.Return.Status == "completed" {
		a.mu.Lock()
		a.record.QEMU = response.Return
		if response.Return.DowntimeMS != nil && a.sample.Measurement.DowntimeMS == nil {
			a.sample.DowntimeMs = *response.Return.DowntimeMS
			a.sample.Measurement.DowntimeMS = response.Return.DowntimeMS
		}
		a.mu.Unlock()
	}
}

func (a *App) finish(status, errMsg string) {
	a.mu.Lock()
	a.running = false
	a.cancel = nil
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

func (a *App) handleRecord(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	record := a.record
	snapshot := struct {
		MigrationRecord
		VM     string `json:"vm"`
		Target string `json:"target"`
		Result string `json:"result"`
	}{record, a.vm, a.target, a.sample.Status}
	a.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, snapshot)
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

func migrationDataURI(target string) string {
	uri, err := url.Parse(target)
	if err != nil || uri.Scheme != "qemu+ssh" {
		return ""
	}
	ip, err := netip.ParseAddr(uri.Hostname())
	if err != nil {
		return ""
	}
	host := ip.String()
	if ip.Is6() {
		host = "[" + host + "]"
	}
	return (&url.URL{Scheme: "tcp", Host: host}).String()
}

func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "请求方法不受支持"})
		return
	}
	var req struct {
		ProbeURL string `json:"probe_url"`
		VM       string `json:"vm"`
		Target   string `json:"target"`

		Copy      string `json:"copy"`
		Bandwidth int    `json:"bandwidth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "迁移请求格式无效"})
		return
	}
	if req.Copy == "" {
		req.Copy = "all"
	}
	if req.Copy != "all" && req.Copy != "inc" && req.Copy != "none" {
		writeJSON(w, 400, map[string]any{"error": "存储复制方式无效"})
		return
	}
	if req.Bandwidth < 0 {
		writeJSON(w, 400, map[string]any{"error": "带宽上限必须为非负整数"})
		return
	}
	probeURL := strings.TrimSpace(req.ProbeURL)
	if probeURL != "" {
		endpoint, err := url.Parse(probeURL)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil {
			writeJSON(w, 400, map[string]any{"error": "请填写有效的 HTTP 或 HTTPS 业务健康检查地址"})
			return
		}
	}
	dataURI := migrationDataURI(req.Target)
	if err := a.startMigration(req.VM, req.Target, req.Copy, dataURI, probeURL, req.Bandwidth); err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "请求方法不受支持"})
		return
	}
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel == nil {
		writeJSON(w, 409, map[string]any{"error": "当前没有可取消的迁移任务"})
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
	http.HandleFunc("/api/record", app.handleRecord)
	http.HandleFunc("/api/stream", app.handleStream)
	http.HandleFunc("/api/migrate", app.handleMigrate)
	http.HandleFunc("/api/cancel", app.handleCancel)
	http.HandleFunc("/api/diag", app.handleDiag)

	log.Printf("热迁移演示服务启动: http://0.0.0.0%s", *listenAddr)
	log.Printf("默认目标: %s", *defaultTgt)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

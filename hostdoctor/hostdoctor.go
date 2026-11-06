// Package hostdoctor reads what the Linux kernel already says about a host and
// turns it into findings an engineer can act on: connection states that show a
// leak, an accept queue that is filling, file descriptors and inodes running
// out, memory and CPU pressure. It reads /proc directly, so it works on a box
// with nothing installed, and Collect takes the /proc path so the parsers can be
// tested against captured files.
package hostdoctor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Facts struct {
	CPUs      int
	Load1     float64
	MemTotal  int64 // kB
	MemAvail  int64 // kB
	SwapTotal int64
	SwapFree  int64

	FileAlloc int64 // system-wide open file handles
	FileMax   int64

	TCP           map[string]int // connection counts by state name
	Listeners     []Listener
	PortRangeLow  int
	PortRangeHigh int
	SomaxConn     int
	ListenDrops   int64 // since boot, from /proc/net/netstat
	ListenOverflw int64

	ConntrackCount int64 // -1 when the module is not loaded
	ConntrackMax   int64

	PSI map[string]float64 // "cpu", "memory", "io": share of the last 10s some tasks were stalled

	FS []FSUsage

	Process *ProcessFacts
}

type Listener struct {
	Port        int
	AcceptQueue int // connections completed by the kernel, waiting for the app to accept()
}

type FSUsage struct {
	Mount      string
	Type       string
	UsedPct    float64
	InodesPct  float64
	TotalBytes uint64
}

type ProcessFacts struct {
	PID     int
	Name    string
	Threads int
	OpenFDs int
	MaxFDs  int
}

type Options struct {
	PID int // 0 = skip the per-process checks
}

var tcpStates = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2",
	"06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING",
}

// Collect reads facts from a /proc tree.
func Collect(proc string, o Options) (*Facts, error) {
	f := &Facts{TCP: map[string]int{}, PSI: map[string]float64{}, ConntrackCount: -1}
	if err := f.readLoad(proc); err != nil {
		return nil, err
	}
	f.readMem(proc)
	f.CPUs = countCPUs(proc)
	f.FileAlloc, f.FileMax = readFileNr(proc)
	for _, name := range []string{"tcp", "tcp6"} {
		f.readTCP(filepath.Join(proc, "net", name))
	}
	f.PortRangeLow, f.PortRangeHigh = readPortRange(proc)
	f.SomaxConn = int(readInt(filepath.Join(proc, "sys/net/core/somaxconn"), 4096))
	f.ListenOverflw, f.ListenDrops = readListenCounters(proc)
	if n := readInt(filepath.Join(proc, "sys/net/netfilter/nf_conntrack_count"), -1); n >= 0 {
		f.ConntrackCount = n
		f.ConntrackMax = readInt(filepath.Join(proc, "sys/net/netfilter/nf_conntrack_max"), 0)
	}
	for _, r := range []string{"cpu", "memory", "io"} {
		if v, ok := readPSI(filepath.Join(proc, "pressure", r)); ok {
			f.PSI[r] = v
		}
	}
	if proc == "/proc" {
		f.FS = filesystems(filepath.Join(proc, "mounts"))
	}
	if o.PID > 0 {
		f.Process = readProcess(proc, o.PID)
	}
	return f, nil
}

func readInt(path string, def int64) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return def
	}
	return n
}

func (f *Facts) readLoad(proc string) error {
	b, err := os.ReadFile(filepath.Join(proc, "loadavg"))
	if err != nil {
		return fmt.Errorf("this does not look like /proc: %w", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 {
		return fmt.Errorf("empty loadavg")
	}
	f.Load1, _ = strconv.ParseFloat(fields[0], 64)
	return nil
}

func (f *Facts) readMem(proc string) {
	file, err := os.Open(filepath.Join(proc, "meminfo"))
	if err != nil {
		return
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(parts[1], 10, 64)
		switch strings.TrimSuffix(parts[0], ":") {
		case "MemTotal":
			f.MemTotal = n
		case "MemAvailable":
			f.MemAvail = n
		case "SwapTotal":
			f.SwapTotal = n
		case "SwapFree":
			f.SwapFree = n
		}
	}
}

func countCPUs(proc string) int {
	file, err := os.Open(filepath.Join(proc, "cpuinfo"))
	if err != nil {
		return 1
	}
	defer file.Close()
	n := 0
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

func readFileNr(proc string) (alloc, max int64) {
	b, err := os.ReadFile(filepath.Join(proc, "sys/fs/file-nr"))
	if err != nil {
		return 0, 0
	}
	p := strings.Fields(string(b))
	if len(p) < 3 {
		return 0, 0
	}
	alloc, _ = strconv.ParseInt(p[0], 10, 64)
	max, _ = strconv.ParseInt(p[2], 10, 64)
	return
}

func (f *Facts) readTCP(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Scan() // header
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 5 {
			continue
		}
		state := tcpStates[strings.ToUpper(p[3])]
		if state == "" {
			continue
		}
		f.TCP[state]++
		if state == "LISTEN" {
			port := hexPort(p[1])
			// For a listening socket the kernel puts the accept-queue length in rx_queue.
			q := strings.SplitN(p[4], ":", 2)
			depth := 0
			if len(q) == 2 {
				d, _ := strconv.ParseInt(q[1], 16, 64)
				depth = int(d)
			}
			f.Listeners = append(f.Listeners, Listener{Port: port, AcceptQueue: depth})
		}
	}
}

func hexPort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	n, _ := strconv.ParseInt(addr[i+1:], 16, 32)
	return int(n)
}

func readPortRange(proc string) (lo, hi int) {
	b, err := os.ReadFile(filepath.Join(proc, "sys/net/ipv4/ip_local_port_range"))
	if err != nil {
		return 32768, 60999
	}
	p := strings.Fields(string(b))
	if len(p) < 2 {
		return 32768, 60999
	}
	lo, _ = strconv.Atoi(p[0])
	hi, _ = strconv.Atoi(p[1])
	return
}

// readListenCounters reads ListenOverflows and ListenDrops, the counters that
// say the kernel threw away connection attempts because an accept queue was full.
func readListenCounters(proc string) (overflows, drops int64) {
	file, err := os.Open(filepath.Join(proc, "net/netstat"))
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	var header []string
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) == 0 || p[0] != "TcpExt:" {
			continue
		}
		if header == nil {
			header = p
			continue
		}
		for i := 1; i < len(p) && i < len(header); i++ {
			n, _ := strconv.ParseInt(p[i], 10, 64)
			switch header[i] {
			case "ListenOverflows":
				overflows = n
			case "ListenDrops":
				drops = n
			}
		}
		break
	}
	return
}

func readPSI(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, kv := range strings.Fields(line) {
			if strings.HasPrefix(kv, "avg10=") {
				v, err := strconv.ParseFloat(strings.TrimPrefix(kv, "avg10="), 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

func readProcess(proc string, pid int) *ProcessFacts {
	dir := filepath.Join(proc, strconv.Itoa(pid))
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	pf := &ProcessFacts{PID: pid}
	if b, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			switch {
			case strings.HasPrefix(line, "Name:"):
				pf.Name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			case strings.HasPrefix(line, "Threads:"):
				pf.Threads, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Threads:")))
			}
		}
	}
	if es, err := os.ReadDir(filepath.Join(dir, "fd")); err == nil {
		pf.OpenFDs = len(es)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "limits")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Max open files") {
				p := strings.Fields(strings.TrimPrefix(line, "Max open files"))
				if len(p) > 0 { // soft limit first
					if p[0] == "unlimited" {
						pf.MaxFDs = 1 << 30
					} else {
						pf.MaxFDs, _ = strconv.Atoi(p[0])
					}
				}
			}
		}
	}
	return pf
}

// ---- diagnosis ----

type Finding struct {
	Level string `json:"level"` // crit or warn
	Area  string `json:"area"`
	What  string `json:"what"`
	Next  string `json:"next"`
}

// Diagnose turns facts into findings. It is a pure function of the facts.
func Diagnose(f *Facts) []Finding {
	var out []Finding
	add := func(level, area, what, next string) { out = append(out, Finding{level, area, what, next}) }

	if f.CPUs > 0 {
		per := f.Load1 / float64(f.CPUs)
		switch {
		case per > 3:
			add("crit", "cpu", fmt.Sprintf("1-minute load %.1f on %d CPUs (%.1f per CPU)", f.Load1, f.CPUs, per), "top -H -b -n1 | head -20 to see which threads; high load with idle CPU means tasks stuck in disk wait, check iowait and PSI io")
		case per > 1.5:
			add("warn", "cpu", fmt.Sprintf("1-minute load %.1f on %d CPUs (%.1f per CPU)", f.Load1, f.CPUs, per), "top -H, and compare with vmstat 1 for run queue and iowait")
		}
	}
	if f.MemTotal > 0 {
		pct := 100 * float64(f.MemAvail) / float64(f.MemTotal)
		switch {
		case pct < 5:
			add("crit", "memory", fmt.Sprintf("only %.1f%% of memory is available", pct), "ps aux --sort=-rss | head; dmesg -T | grep -i 'out of memory' for OOM kills")
		case pct < 10:
			add("warn", "memory", fmt.Sprintf("only %.1f%% of memory is available", pct), "ps aux --sort=-rss | head; a steady fall points at a leak")
		}
		if f.SwapTotal > 0 {
			if used := 100 * float64(f.SwapTotal-f.SwapFree) / float64(f.SwapTotal); used > 50 {
				add("warn", "memory", fmt.Sprintf("%.0f%% of swap is in use", used), "vmstat 1: nonzero si/so means it is actively swapping, which hurts latency")
			}
		}
	}
	if f.FileMax > 0 {
		if pct := 100 * float64(f.FileAlloc) / float64(f.FileMax); pct > 80 {
			add("warn", "files", fmt.Sprintf("system file handles at %.0f%% of fs.file-max", pct), "lsof -n | awk '{print $1}' | sort | uniq -c | sort -rn | head to find the process holding them")
		}
	}
	if n := f.TCP["CLOSE_WAIT"]; n > 1000 {
		add("crit", "network", fmt.Sprintf("%d sockets in CLOSE_WAIT", n), "the remote side closed and our application never closed its end: a connection leak. ss -tanp state close-wait | awk '{print $NF}' | sort | uniq -c to find the process")
	} else if n > 100 {
		add("warn", "network", fmt.Sprintf("%d sockets in CLOSE_WAIT", n), "ss -tanp state close-wait; they should be near zero. A growing count is a leak")
	}
	if r := f.PortRangeHigh - f.PortRangeLow + 1; r > 0 {
		if pct := 100 * float64(f.TCP["TIME_WAIT"]) / float64(r); pct > 50 {
			add("crit", "network", fmt.Sprintf("%d TIME_WAIT sockets against %d ephemeral ports (%.0f%%)", f.TCP["TIME_WAIT"], r, pct), "new outbound connections will fail with 'cannot assign requested address'. Reuse connections (keep-alive, pooling) before widening net.ipv4.ip_local_port_range")
		} else if pct > 25 {
			add("warn", "network", fmt.Sprintf("%d TIME_WAIT sockets against %d ephemeral ports (%.0f%%)", f.TCP["TIME_WAIT"], r, pct), "a client opening a new connection per request; check connection reuse")
		}
	}
	if n := f.TCP["SYN_RECV"]; n > 100 {
		add("warn", "network", fmt.Sprintf("%d half-open connections (SYN_RECV)", n), "a SYN flood or a full accept queue; check net.ipv4.tcp_syncookies and the listen drops below")
	}
	for _, l := range f.Listeners {
		if f.SomaxConn > 0 && l.AcceptQueue*2 >= f.SomaxConn {
			add("crit", "network", fmt.Sprintf("port %d has %d connections waiting in its accept queue (somaxconn %d)", l.Port, l.AcceptQueue, f.SomaxConn), "the application is not calling accept() fast enough: all worker threads busy or blocked. Clients start seeing timeouts and resets. Take a thread dump")
		} else if l.AcceptQueue > 0 && f.SomaxConn > 0 && l.AcceptQueue*10 >= f.SomaxConn {
			add("warn", "network", fmt.Sprintf("port %d has %d connections waiting in its accept queue", l.Port, l.AcceptQueue), "ss -ltn shows Recv-Q for each listener; it should be near zero")
		}
	}
	if f.ListenDrops > 0 || f.ListenOverflw > 0 {
		add("warn", "network", fmt.Sprintf("the kernel has dropped %d connection attempts (ListenOverflows %d) since boot", f.ListenDrops, f.ListenOverflw), "nstat -az TcpExtListenDrops twice, a minute apart: if it grows, a listener's backlog is too small or the app is too slow to accept")
	}
	if f.ConntrackCount >= 0 && f.ConntrackMax > 0 {
		if pct := 100 * float64(f.ConntrackCount) / float64(f.ConntrackMax); pct > 80 {
			add("crit", "network", fmt.Sprintf("connection tracking table at %.0f%% (%d of %d)", pct, f.ConntrackCount, f.ConntrackMax), "when it fills, new connections are dropped ('nf_conntrack: table full' in dmesg). Raise net.netfilter.nf_conntrack_max or shorten the timeouts")
		}
	}
	for name, limit := range map[string]float64{"cpu": 40, "memory": 10, "io": 20} {
		if v, ok := f.PSI[name]; ok && v > limit {
			add("warn", name, fmt.Sprintf("pressure stall: tasks waited on %s for %.1f%% of the last 10s", name, v), "cat /proc/pressure/"+name+" for the 60s and 300s averages; this is lost time, not just usage")
		}
	}
	for _, fs := range f.FS {
		switch {
		case fs.UsedPct > 95:
			add("crit", "filesystem", fmt.Sprintf("%s is %.0f%% full", fs.Mount, fs.UsedPct), "du -xh --max-depth=2 "+fs.Mount+" | sort -h | tail; also check for deleted files still held open: lsof +L1")
		case fs.UsedPct > 85:
			add("warn", "filesystem", fmt.Sprintf("%s is %.0f%% full", fs.Mount, fs.UsedPct), "du -xh --max-depth=2 "+fs.Mount+" | sort -h | tail")
		}
		switch {
		case fs.InodesPct > 95:
			add("crit", "filesystem", fmt.Sprintf("%s has used %.0f%% of its inodes", fs.Mount, fs.InodesPct), "df -h looks fine but every create fails with 'No space left on device'. Find the directory with millions of small files: for d in "+fs.Mount+"/*; do echo $(find $d -xdev | wc -l) $d; done | sort -n | tail")
		case fs.InodesPct > 85:
			add("warn", "filesystem", fmt.Sprintf("%s has used %.0f%% of its inodes", fs.Mount, fs.InodesPct), "df -i; a directory full of tiny files (sessions, caches, mail) is the usual cause")
		}
	}
	if p := f.Process; p != nil && p.MaxFDs > 0 {
		if pct := 100 * float64(p.OpenFDs) / float64(p.MaxFDs); pct > 80 {
			add("crit", "process", fmt.Sprintf("%s (pid %d) has %d of %d file descriptors open", p.Name, p.PID, p.OpenFDs, p.MaxFDs), "ls -l /proc/"+strconv.Itoa(p.PID)+"/fd | awk '{print $NF}' | sed 's/[0-9]*$//' | sort | uniq -c | sort -rn | head shows what kind they are (sockets are a leak, files may be a missing close)")
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Level == "crit" && out[j].Level != "crit" })
	return out
}

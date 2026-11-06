package hostdoctor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func has(fs []Finding, level, area, contains string) bool {
	for _, f := range fs {
		if f.Level == level && f.Area == area && strings.Contains(f.What, contains) {
			return true
		}
	}
	return false
}

func TestParsersReadATroubledHost(t *testing.T) {
	f, err := Collect("testdata/proc", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.CPUs != 4 || f.Load1 != 14.2 || f.MemAvail != 600000 || f.FileAlloc != 1500 || f.FileMax != 1600 {
		t.Errorf("basics: %+v", f)
	}
	for state, want := range map[string]int{"LISTEN": 2, "ESTABLISHED": 50, "CLOSE_WAIT": 120, "TIME_WAIT": 300, "SYN_RECV": 3} {
		if f.TCP[state] != want {
			t.Errorf("%s = %d, want %d", state, f.TCP[state], want)
		}
	}
	if len(f.Listeners) != 2 || f.Listeners[0].Port != 8080 || f.Listeners[0].AcceptQueue != 2304 {
		t.Errorf("listeners %+v", f.Listeners)
	}
	if f.PortRangeLow != 32768 || f.PortRangeHigh != 33267 || f.SomaxConn != 4096 {
		t.Errorf("ports %d-%d somaxconn %d", f.PortRangeLow, f.PortRangeHigh, f.SomaxConn)
	}
	if f.ListenDrops != 17 || f.ListenOverflw != 17 {
		t.Errorf("listen counters %d %d", f.ListenDrops, f.ListenOverflw)
	}
	if f.ConntrackCount != 250000 || f.ConntrackMax != 262144 {
		t.Errorf("conntrack %d/%d", f.ConntrackCount, f.ConntrackMax)
	}
	if f.PSI["cpu"] != 55 || f.PSI["memory"] != 12.5 || f.PSI["io"] != 3 {
		t.Errorf("psi %v", f.PSI)
	}
}

func TestDiagnosisOfATroubledHost(t *testing.T) {
	f, _ := Collect("testdata/proc", Options{})
	got := Diagnose(f)
	for _, c := range []struct{ level, area, what string }{
		{"crit", "cpu", "load 14.2"},
		{"crit", "memory", "3.8% of memory"},
		{"warn", "memory", "80% of swap"},
		{"warn", "files", "94%"},
		{"warn", "network", "120 sockets in CLOSE_WAIT"},
		{"crit", "network", "300 TIME_WAIT"},
		{"crit", "network", "port 8080 has 2304"},
		{"warn", "network", "dropped 17"},
		{"crit", "network", "connection tracking table at 95%"},
		{"warn", "cpu", "pressure stall"},
		{"warn", "memory", "pressure stall"},
	} {
		if !has(got, c.level, c.area, c.what) {
			t.Errorf("missing %s %s %q", c.level, c.area, c.what)
		}
	}
	if has(got, "warn", "io", "pressure") {
		t.Error("io pressure of 3% should not be reported")
	}
	if got[0].Level != "crit" {
		t.Errorf("critical findings should come first: %+v", got[0])
	}
	for _, x := range got {
		if x.Next == "" {
			t.Errorf("%s has no next step", x.What)
		}
	}
}

func TestHealthyHostHasNoFindings(t *testing.T) {
	f, err := Collect("testdata/healthy", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := Diagnose(f); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestFilesystemAndProcessChecks(t *testing.T) {
	f := &Facts{CPUs: 4, FS: []FSUsage{
		{Mount: "/var", UsedPct: 97},
		{Mount: "/data", UsedPct: 40, InodesPct: 99},
		{Mount: "/", UsedPct: 50, InodesPct: 10},
	}, Process: &ProcessFacts{PID: 7, Name: "api", OpenFDs: 900, MaxFDs: 1024}}
	got := Diagnose(f)
	if !has(got, "crit", "filesystem", "/var is 97% full") || !has(got, "crit", "filesystem", "/data has used 99% of its inodes") || !has(got, "crit", "process", "900 of 1024") {
		t.Fatalf("%+v", got)
	}
	for _, x := range got {
		if x.Area == "filesystem" && strings.HasPrefix(x.What, "/ ") {
			t.Error("a healthy mount was reported")
		}
	}
}

func TestCloseWaitAndPortExhaustionEscalate(t *testing.T) {
	f := &Facts{CPUs: 1, TCP: map[string]int{"CLOSE_WAIT": 5000, "TIME_WAIT": 20000}, PortRangeLow: 32768, PortRangeHigh: 60999}
	got := Diagnose(f)
	if !has(got, "crit", "network", "5000 sockets in CLOSE_WAIT") || !has(got, "crit", "network", "20000 TIME_WAIT") {
		t.Fatalf("%+v", got)
	}
}

func TestProcessFactsFromProc(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "4242/fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"status", "limits"} {
		b, _ := os.ReadFile(filepath.Join("testdata/proc/4242", n))
		os.WriteFile(filepath.Join(dir, "4242", n), b, 0o644)
	}
	for i := 0; i < 900; i++ {
		os.WriteFile(filepath.Join(dir, "4242/fd", fmt.Sprint(i)), nil, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "loadavg"), []byte("0.1 0.1 0.1 1/1 1\n"), 0o644)
	f, err := Collect(dir, Options{PID: 4242})
	if err != nil {
		t.Fatal(err)
	}
	p := f.Process
	if p == nil || p.Name != "orders-api" || p.Threads != 64 || p.OpenFDs != 900 || p.MaxFDs != 1024 {
		t.Fatalf("%+v", p)
	}
	if !has(Diagnose(f), "crit", "process", "900 of 1024") {
		t.Error("88% of file descriptors should be critical")
	}
}

func TestNotProc(t *testing.T) {
	if _, err := Collect(t.TempDir(), Options{}); err == nil {
		t.Error("an empty directory is not /proc")
	}
}

func TestRealProcOnThisMachine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	f, err := Collect("/proc", Options{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if f.CPUs < 1 || f.MemTotal <= 0 || f.Process == nil || f.Process.OpenFDs < 3 || f.Process.Threads < 1 {
		t.Errorf("%+v process %+v", f, f.Process)
	}
	n := 0
	for _, v := range f.TCP {
		n += v
	}
	t.Logf("this machine: %d CPUs, load %.2f, %d MB available, %d TCP sockets %v, %d filesystems checked", f.CPUs, f.Load1, f.MemAvail/1024, n, f.TCP, len(f.FS))
	_ = Diagnose(f)
}

// A listener that never calls accept() fills its accept queue. This checks the
// parser against what the kernel really reports, not against a file I wrote.
func TestAcceptQueueOnARealListener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	var depth int
	for i := 0; i < 50 && depth == 0; i++ {
		f, err := Collect("/proc", Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range f.Listeners {
			if l.Port == port {
				depth = l.AcceptQueue
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if depth != 5 {
		t.Fatalf("accept queue depth %d, want the 5 connections nobody accepted", depth)
	}
}

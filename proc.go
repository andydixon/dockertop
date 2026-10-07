package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the kernel's clock tick for /proc/PID/stat times.
// ponytail: fixed at 100 on every mainstream Linux arch; read sysconf if a port needs otherwise.
const userHZ = 100

var pageSize = uint64(os.Getpagesize())

type Proc struct {
	PID, PPID, UID  int
	Tree            string // tree-view prefix, display only
	User, Comm, Cmd string
	State           byte
	Pri, Nice       int
	Threads         int
	Virt, RSS       uint64 // bytes
	Ticks           uint64 // utime+stime
	CPU             float64
	HasCPU          bool
}

// readProcs reads /proc for the given host PIDs. prev maps PID to its Ticks
// at time then, for the CPU column.
func readProcs(pids []int, prev map[int]uint64, dt float64) []Proc {
	out := make([]Proc, 0, len(pids))
	for _, pid := range pids {
		p, ok := readProc(pid)
		if !ok {
			continue // exited between docker top and now
		}
		if t, ok := prev[pid]; ok && dt > 0 && p.Ticks >= t {
			p.CPU, p.HasCPU = float64(p.Ticks-t)/userHZ/dt*100, true
		}
		out = append(out, p)
	}
	return out
}

func readProc(pid int) (Proc, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return Proc{}, false
	}
	p, ok := parseStat(b)
	if !ok {
		return Proc{}, false
	}
	p.PID = pid
	// stat's rss is the kernel's approximate counter (0 for small processes on
	// recent kernels); status has the exact VmRSS, and the UID.
	for _, line := range strings.Split(string(readFile(dir+"/status")), "\n") {
		k, v, _ := strings.Cut(line, ":")
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "Uid":
			p.UID, _ = strconv.Atoi(f[0])
		case "VmRSS":
			kb, _ := strconv.ParseUint(f[0], 10, 64)
			p.RSS = kb * 1024
		}
	}
	cmd := bytes.TrimRight(readFile(dir+"/cmdline"), "\x00")
	p.Cmd = string(bytes.ReplaceAll(cmd, []byte{0}, []byte{' '}))
	if p.Cmd == "" {
		p.Cmd = "[" + p.Comm + "]"
	}
	return p, true
}

// parseStat parses /proc/PID/stat. comm is in parentheses and may itself
// contain spaces and parentheses, so fields are counted from the last ')'.
func parseStat(b []byte) (Proc, bool) {
	s := string(b)
	l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if l < 0 || r < l {
		return Proc{}, false
	}
	f := strings.Fields(s[r+1:])
	if len(f) < 22 {
		return Proc{}, false
	}
	n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
	i := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
	return Proc{
		Comm: s[l+1 : r], State: f[0][0], PPID: i(1),
		Ticks: n(11) + n(12), Pri: i(15), Nice: i(16), Threads: i(17),
		Virt: n(20), RSS: n(21) * pageSize,
	}, true
}

// isLocal says whether pid is in this host's /proc and belongs to container id,
// i.e. docker runs on this machine and we can read process details ourselves.
func isLocal(pid int, id string) bool {
	return bytes.Contains(readFile("/proc/"+strconv.Itoa(pid)+"/cgroup"), []byte(id))
}

// containerUsers maps UIDs to names from the container's own /etc/passwd,
// reached through /proc/PID/root (needs root). Empty if unreadable.
func containerUsers(pid int) map[int]string {
	m := map[int]string{}
	for _, line := range strings.Split(string(readFile("/proc/"+strconv.Itoa(pid)+"/root/etc/passwd")), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 2 {
			if uid, err := strconv.Atoi(f[2]); err == nil {
				m[uid] = f[0]
			}
		}
	}
	return m
}

func readFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// cpuTime formats ticks as htop's TIME+: m:ss.cc, or h:mm:ss past an hour.
func cpuTime(ticks uint64) string {
	d := time.Duration(ticks) * time.Second / userHZ
	if d >= time.Hour {
		return strconv.Itoa(int(d.Hours())) + "h" + pad2(int(d.Minutes())%60) + ":" + pad2(int(d.Seconds())%60)
	}
	return strconv.Itoa(int(d.Minutes())) + ":" + pad2(int(d.Seconds())%60) + "." + pad2(int(ticks%userHZ))
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

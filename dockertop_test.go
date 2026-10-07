package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	// comm with spaces and a ')' must not shift the fields
	in := "42 (my (odd) proc) S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 3 0 1000 123456789 1500 18446744073709551615"
	p, ok := parseStat([]byte(in))
	if !ok || p.Comm != "my (odd) proc" || p.State != 'S' || p.PPID != 1 || p.Ticks != 300 ||
		p.Pri != 20 || p.Nice != 0 || p.Threads != 3 || p.Virt != 123456789 {
		t.Fatalf("bad parse: %+v", p)
	}
	if cpuTime(300) != "0:03.00" || cpuTime(100*3725) != "1h02:05" {
		t.Fatalf("cpuTime: %s %s", cpuTime(300), cpuTime(100*3725))
	}
}

func TestDemux(t *testing.T) {
	frame := func(stream byte, s string) string {
		n := len(s)
		return string([]byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}) + s
	}
	in := frame(1, "out line\n") + frame(2, "err line\n")
	if got := string(demux([]byte(in))); got != "out line\nerr line\n" {
		t.Fatalf("demux: %q", got)
	}
	if got := ansi.ReplaceAllString("\x1b[31mred\x1b[0m", ""); got != "red" {
		t.Fatalf("ansi: %q", got)
	}
}

func TestStatsRates(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s stats
	s.Read = t0
	s.CPU.Usage.Total = 1e9
	s.Mem.Usage, s.Mem.Limit = 300, 1000
	s.Mem.Stats = map[string]uint64{"inactive_file": 100}
	var prev Container
	applyStats(&prev, s, Container{})
	if prev.HasRate || prev.Mem != 200 {
		t.Fatalf("first sample: %+v", prev)
	}
	s.Read = t0.Add(2 * time.Second)
	s.CPU.Usage.Total = 2e9 // 1 CPU-second over 2s = 50%
	var c Container
	applyStats(&c, s, prev)
	if !c.HasRate || c.CPU != 50 || c.MemPct() != 20 {
		t.Fatalf("rates: %+v", c)
	}
	if rate(5, 10, 1) != 0 {
		t.Fatal("counter reset must give 0")
	}
}

func TestGroupRows(t *testing.T) {
	cs := []Container{
		{ID: "a", Name: "web-1", Project: "web", CPU: 30, State: "running"},
		{ID: "b", Name: "solo", CPU: 20, State: "running"},
		{ID: "c", Name: "web-db", Project: "web", CPU: 5, State: "exited"},
	}
	byCPU := func(x, y Container) bool { return x.CPU > y.CPU }
	r := groupRows(cs, byCPU)
	var got []string
	for _, c := range r {
		got = append(got, c.Tree+c.Name)
	}
	if strings.Join(got, ",") != "web,├─ web-1,└─ web-db,solo" || r[0].CPU != 35 || r[0].Status != "1/2 running" {
		t.Fatalf("bad grouping: %q %+v", got, r[0])
	}
}

func TestConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := defaults
	c.Sort, c.ProcSort, c.Tree, c.All, c.Refresh = "MEM", "TIME", true, false, 0.5
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(); got != c {
		t.Fatalf("round trip: %+v != %+v", got, c)
	}
}

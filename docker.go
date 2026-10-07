package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Docker is a minimal Engine API client: plain HTTP over the daemon socket.
type Docker struct {
	c    *http.Client
	base string
	Host string
}

func newDocker() (*Docker, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	scheme, addr, _ := strings.Cut(host, "://")
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	d := &Docker{c: &http.Client{Transport: tr, Timeout: 60 * time.Second}, Host: host}
	switch scheme {
	case "unix":
		d.base = "http://docker"
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", addr)
		}
	case "tcp":
		// ponytail: no TLS; use a unix socket or an ssh tunnel if the daemon needs it.
		if os.Getenv("DOCKER_TLS_VERIFY") != "" {
			return nil, errors.New("DOCKER_TLS_VERIFY is set but TLS is not supported; tunnel the socket instead")
		}
		d.base = "http://" + addr
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST %q (unix:// or tcp:// only)", host)
	}
	return d, nil
}

// do sends a request and decodes a JSON reply into out (*[]byte takes the raw body).
func (d *Docker) do(method, path string, out any) error {
	req, err := http.NewRequest(method, d.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 { // 304 (already started/stopped) is fine
		var e struct{ Message string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return errors.New(e.Message)
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o, err = io.ReadAll(resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- containers and their stats ----

type Container struct {
	ID, Name, Image, Command string
	State, Status            string // "running", "Up 3 hours (healthy)"
	Project                  string // compose project, "" if none
	Created                  int64
	Tree                     string // tree-view prefix, display only
	Group                    bool   // pseudo row: a compose project with its containers summed

	HasStats           bool    // running or paused, stats read
	HasRate            bool    // a previous sample exists, so CPU and rates are valid
	CPU                float64 // percent of one core
	Mem, MemLimit      uint64  // bytes
	Pids, PidsLimit    uint64
	NetRx, NetTx       float64 // bytes/s
	BlkR, BlkW         float64 // bytes/s
	NetRxTot, NetTxTot uint64
	BlkRTot, BlkWTot   uint64
	cpuNs              uint64
	at                 time.Time
	Online             int
}

func (c Container) MemPct() float64 {
	if c.MemLimit == 0 {
		return 0
	}
	return float64(c.Mem) * 100 / float64(c.MemLimit)
}

type Info struct {
	Name, ServerVersion, OperatingSystem, CgroupDriver, CgroupVersion string
	NCPU                                                              int
	MemTotal                                                          uint64
	Images                                                            int
}

type stats struct {
	Read time.Time
	CPU  struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		Online int `json:"online_cpus"`
	} `json:"cpu_stats"`
	Mem struct {
		Usage, Limit uint64
		Stats        map[string]uint64
	} `json:"memory_stats"`
	Networks map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	}
	Pids  struct{ Current, Limit uint64 } `json:"pids_stats"`
	Blkio struct {
		IO []struct {
			Op    string
			Value uint64
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

// collect lists every container and reads stats for the running ones in
// parallel. prev (the last result, by ID) is only read, to compute rates.
func (d *Docker) collect(prev map[string]Container) ([]Container, error) {
	var list []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		Command string
		Created int64
		Labels  map[string]string
		State   string
		Status  string
	}
	if err := d.do("GET", "/containers/json?all=1", &list); err != nil {
		return nil, err
	}
	cs := make([]Container, len(list))
	var wg sync.WaitGroup
	for i, l := range list {
		name := l.ID[:12]
		if len(l.Names) > 0 {
			name = strings.TrimPrefix(l.Names[0], "/")
		}
		cs[i] = Container{ID: l.ID, Name: name, Image: l.Image, Command: l.Command, State: l.State, Status: l.Status,
			Project: l.Labels["com.docker.compose.project"], Created: l.Created}
		if l.State != "running" && l.State != "paused" {
			continue
		}
		wg.Add(1)
		go func(c *Container) {
			defer wg.Done()
			var s stats
			if d.do("GET", "/containers/"+c.ID+"/stats?stream=false&one-shot=true", &s) == nil {
				applyStats(c, s, prev[c.ID])
			}
		}(&cs[i])
	}
	wg.Wait()
	return cs, nil
}

func applyStats(c *Container, s stats, p Container) {
	c.HasStats = true
	c.at = s.Read
	if c.at.IsZero() {
		c.at = time.Now()
	}
	c.cpuNs, c.Online = s.CPU.Usage.Total, s.CPU.Online
	// Same as `docker stats`: page cache that can be dropped doesn't count.
	inactive := s.Mem.Stats["inactive_file"] // cgroup v2
	if v, ok := s.Mem.Stats["total_inactive_file"]; ok {
		inactive = v // cgroup v1
	}
	c.Mem, c.MemLimit = s.Mem.Usage, s.Mem.Limit
	if inactive < c.Mem {
		c.Mem -= inactive
	}
	c.Pids, c.PidsLimit = s.Pids.Current, s.Pids.Limit
	for _, n := range s.Networks {
		c.NetRxTot += n.Rx
		c.NetTxTot += n.Tx
	}
	for _, b := range s.Blkio.IO {
		switch strings.ToLower(b.Op) {
		case "read":
			c.BlkRTot += b.Value
		case "write":
			c.BlkWTot += b.Value
		}
	}
	if !p.HasStats || p.at.IsZero() {
		return
	}
	dt := c.at.Sub(p.at).Seconds()
	if dt <= 0 {
		return
	}
	c.HasRate = true
	c.CPU = rate(c.cpuNs, p.cpuNs, dt) / 1e7 // ns/s -> percent of a core
	c.NetRx, c.NetTx = rate(c.NetRxTot, p.NetRxTot, dt), rate(c.NetTxTot, p.NetTxTot, dt)
	c.BlkR, c.BlkW = rate(c.BlkRTot, p.BlkRTot, dt), rate(c.BlkWTot, p.BlkWTot, dt)
}

// rate is (cur-old)/dt, or 0 when the counter went backwards (container restarted).
func rate(cur, old uint64, dt float64) float64 {
	if cur < old {
		return 0
	}
	return float64(cur-old) / dt
}

func (d *Docker) info() (Info, error) {
	var i Info
	err := d.do("GET", "/info", &i)
	return i, err
}

// ---- inspect ----

type Inspect struct {
	ID      string `json:"Id"`
	Name    string
	Created time.Time
	Path    string
	Args    []string
	Image   string
	State   struct {
		Status                string
		Pid                   int
		ExitCode              int
		OOMKilled             bool
		StartedAt, FinishedAt time.Time
		Error                 string
		Health                *struct {
			Status        string
			FailingStreak int
			Log           []struct {
				ExitCode int
				Output   string
			}
		}
	}
	RestartCount int
	Config       struct {
		Image, User, WorkingDir, Hostname string
		Tty                               bool
		Env, Entrypoint, Cmd              []string
		Labels                            map[string]string
	}
	HostConfig struct {
		NanoCpus, CpuQuota, CpuPeriod, Memory, MemorySwap int64
		PidsLimit                                         *int64
		CpusetCpus, NetworkMode                           string
		Privileged, ReadonlyRootfs                        bool
		RestartPolicy                                     struct{ Name string }
	}
	Mounts []struct {
		Type, Source, Destination, Mode string
		RW                              bool
	}
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIp, HostPort string }
		Networks map[string]struct{ IPAddress, Gateway, MacAddress string }
	}
}

func (d *Docker) inspect(id string) (*Inspect, error) {
	var i Inspect
	if err := d.do("GET", "/containers/"+id+"/json", &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// CPULimit is the container's CPU allowance in percent of one core, 0 if unlimited.
func (i *Inspect) CPULimit() float64 {
	h := i.HostConfig
	switch {
	case h.NanoCpus > 0:
		return float64(h.NanoCpus) / 1e7
	case h.CpuQuota > 0 && h.CpuPeriod > 0:
		return float64(h.CpuQuota) * 100 / float64(h.CpuPeriod)
	}
	return 0
}

// details renders inspect output as the lines of the details view.
func (i *Inspect) details(c Container, showEnv bool) []string {
	l := []string{}
	add := func(f string, a ...any) { l = append(l, fmt.Sprintf(f, a...)) }
	when := func(t time.Time) string {
		if t.IsZero() || t.Year() < 2000 {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04:05") + " (" + ago(time.Since(t)) + " ago)"
	}
	add("Name        %s", strings.TrimPrefix(i.Name, "/"))
	add("ID          %s", i.ID)
	add("Image       %s  (%s)", i.Config.Image, shortID(i.Image))
	add("Command     %s", strings.TrimSpace(i.Path+" "+strings.Join(i.Args, " ")))
	if i.Config.User != "" || i.Config.WorkingDir != "" {
		add("User        %s   workdir %s", or(i.Config.User, "root"), or(i.Config.WorkingDir, "/"))
	}
	add("Created     %s", when(i.Created))
	st := i.State
	add("State       %s   pid %d   restarts %d   policy %s", st.Status, st.Pid, i.RestartCount, or(i.HostConfig.RestartPolicy.Name, "no"))
	add("Started     %s", when(st.StartedAt))
	if st.Status != "running" {
		add("Finished    %s   exit code %d   OOM killed %t", when(st.FinishedAt), st.ExitCode, st.OOMKilled)
	}
	if st.Error != "" {
		add("Error       %s", st.Error)
	}
	if h := st.Health; h != nil {
		add("Health      %s   failing streak %d", h.Status, h.FailingStreak)
		if n := len(h.Log); n > 0 {
			add("  last check: exit %d  %s", h.Log[n-1].ExitCode, oneLine(h.Log[n-1].Output))
		}
	}
	add("")
	add("Limits")
	hc := i.HostConfig
	cpu := "unlimited"
	if lim := i.CPULimit(); lim > 0 {
		cpu = fmt.Sprintf("%.2f CPUs", lim/100)
	}
	if hc.CpusetCpus != "" {
		cpu += "  on cpus " + hc.CpusetCpus
	}
	mem := "unlimited"
	if hc.Memory > 0 {
		mem = human(uint64(hc.Memory))
	}
	pids := "unlimited"
	if hc.PidsLimit != nil && *hc.PidsLimit > 0 {
		pids = fmt.Sprint(*hc.PidsLimit)
	}
	add("  CPU %s   memory %s   pids %s   privileged %t   read-only root %t", cpu, mem, pids, hc.Privileged, hc.ReadonlyRootfs)
	if c.HasStats {
		add("Usage")
		add("  CPU %.1f%%   memory %s / %s   pids %d", c.CPU, human(c.Mem), human(c.MemLimit), c.Pids)
		add("  net rx %s  tx %s   block read %s  write %s   (since start)", human(c.NetRxTot), human(c.NetTxTot), human(c.BlkRTot), human(c.BlkWTot))
	}
	add("")
	add("Network     mode %s   hostname %s", hc.NetworkMode, i.Config.Hostname)
	for _, n := range sortedKeys(i.NetworkSettings.Networks) {
		v := i.NetworkSettings.Networks[n]
		add("  %-24s ip %-16s gw %-16s mac %s", n, or(v.IPAddress, "-"), or(v.Gateway, "-"), or(v.MacAddress, "-"))
	}
	for _, p := range sortedKeys(i.NetworkSettings.Ports) {
		b := i.NetworkSettings.Ports[p]
		if len(b) == 0 {
			add("  port %-12s (not published)", p)
		}
		for _, h := range b {
			add("  port %-12s -> %s:%s", p, or(h.HostIp, "*"), h.HostPort)
		}
	}
	if len(i.Mounts) > 0 {
		add("")
		add("Mounts")
		for _, m := range i.Mounts {
			rw := "ro"
			if m.RW {
				rw = "rw"
			}
			add("  %-6s %s  %s -> %s", m.Type, rw, m.Source, m.Destination)
		}
	}
	if len(i.Config.Labels) > 0 {
		add("")
		add("Labels")
		for _, k := range sortedKeys(i.Config.Labels) {
			add("  %s = %s", k, i.Config.Labels[k])
		}
	}
	add("")
	if showEnv {
		add("Environment  (e to hide)")
		for _, e := range i.Config.Env {
			add("  %s", e)
		}
	} else {
		add("Environment  %d variables hidden, they may hold secrets (e to show)", len(i.Config.Env))
	}
	return l
}

// ---- processes, logs, actions ----

type Top struct {
	Titles    []string
	Processes [][]string
}

func (d *Docker) top(id string) (*Top, error) {
	var t Top
	err := d.do("GET", "/containers/"+id+"/top", &t)
	return &t, err
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)

func (d *Docker) logs(id string, tty bool, tail int) ([]string, error) {
	var b []byte
	if err := d.do("GET", fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&tail=%d", id, tail), &b); err != nil {
		return nil, err
	}
	if !tty {
		b = demux(b)
	}
	s := strings.ReplaceAll(ansi.ReplaceAllString(string(b), ""), "\r", "")
	s = strings.ReplaceAll(s, "\t", "    ")
	return strings.Split(strings.TrimRight(s, "\n"), "\n"), nil
}

// demux strips the 8-byte stream headers docker puts on non-TTY log output.
func demux(b []byte) []byte {
	var out bytes.Buffer
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		n = min(n, len(b))
		out.Write(b[:n])
		b = b[n:]
	}
	return out.Bytes()
}

// action POSTs a lifecycle call: start, stop, restart, pause, unpause, kill?signal=X.
func (d *Docker) action(id, what string) error {
	return d.do("POST", "/containers/"+id+"/"+what, nil)
}

// ---- small helpers ----

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func shortID(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

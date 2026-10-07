package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
)

var version = "dev"

type view int

const (
	viewList view = iota
	viewProcs
	viewText
	viewHelp
)

var sortNames = []string{"CPU", "MEM", "NET", "IO", "PIDS", "NAME", "IMAGE", "STATE"}
var psortNames = []string{"CPU", "MEM", "TIME", "PID", "USER", "COMMAND"}

var signals = []struct {
	name string
	sig  syscall.Signal
}{
	{"SIGTERM", syscall.SIGTERM}, {"SIGKILL", syscall.SIGKILL}, {"SIGINT", syscall.SIGINT}, {"SIGHUP", syscall.SIGHUP},
	{"SIGQUIT", syscall.SIGQUIT}, {"SIGUSR1", syscall.SIGUSR1}, {"SIGUSR2", syscall.SIGUSR2},
	{"SIGSTOP", syscall.SIGSTOP}, {"SIGCONT", syscall.SIGCONT},
}

// modal is a yes/no question (onYes), an info box (neither) or a picker (items/onPick).
type modal struct {
	title  string
	lines  []string
	onYes  func()
	items  []string
	pick   int
	onPick func(int)
}

// snapMsg carries a finished background collection to the UI goroutine.
type snapMsg struct {
	cs  []Container
	err error
}

type app struct {
	scr  tcell.Screen
	cfg  config
	dk   *Docker
	info Info

	// container list
	snap     []Container
	byID     map[string]Container
	err      error
	loading  bool
	rows     []Container
	sel, top int
	sortBy   int
	filter   string
	all      bool
	tree     bool
	fullCmd  bool
	openName string // container to open once the first snapshot arrives (command-line argument)

	// process view of a.cur
	cur        Container
	insp       *Inspect
	procs      []Proc
	prows      []Proc
	remote     *Top // daemon is not on this host: docker top's own table
	procErr    error
	pprev      map[int]uint64
	pat        time.Time
	users      map[int]string
	psel, ptop int
	psortBy    int
	pfilter    string
	ptree      bool

	// text view: details or logs of a.cur
	text      []string
	textErr   error
	textTitle string
	textLoad  func() ([]string, error)
	textFrom  view
	ttop      int
	tleft     int
	follow    bool
	showEnv   bool

	view      view
	helpFrom  view
	searching bool
	modal     *modal
	busy      bool

	mu     sync.Mutex
	status string
}

var (
	stDefault = tcell.StyleDefault
	stHdr     = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorGreen)
	stSel     = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorTeal)
	stKey     = tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
	stFn      = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorTeal)
	stLabel   = tcell.StyleDefault.Foreground(tcell.ColorTeal).Bold(true)
	stDim     = tcell.StyleDefault.Foreground(tcell.ColorGray)
	stWarn    = tcell.StyleDefault.Foreground(tcell.ColorYellow).Bold(true)
	stErr     = tcell.StyleDefault.Foreground(tcell.ColorRed).Bold(true)
	stGreen   = tcell.StyleDefault.Foreground(tcell.ColorGreen)
	stYellow  = tcell.StyleDefault.Foreground(tcell.ColorYellow)
	stMagenta = tcell.StyleDefault.Foreground(tcell.ColorFuchsia)
	stRed     = tcell.StyleDefault.Foreground(tcell.ColorRed)
)

func indexOf(names []string, name string) int {
	for i, n := range names {
		if n == strings.ToUpper(name) {
			return i
		}
	}
	return 0
}

func main() {
	cfg := loadConfig()
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: dockertop [-d SECONDS] [-s COLUMN] [-t] [-r] [--version] [CONTAINER]\n\nhtop-style view of Docker containers and the processes inside them.\nCONTAINER (name or ID prefix) opens straight into its process view. Press h inside for keys.\n\n")
		flag.PrintDefaults()
	}
	delay := flag.Float64("d", cfg.Refresh, "refresh interval in seconds")
	sortFlag := flag.String("s", cfg.Sort, "sort column: "+strings.Join(sortNames, ", "))
	tree := flag.Bool("t", cfg.Tree, "group containers by compose project")
	running := flag.Bool("r", !cfg.All, "show running containers only")
	ver := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *ver {
		fmt.Println("dockertop", version)
		return
	}
	if *delay <= 0 || flag.NArg() > 1 {
		flag.Usage()
		os.Exit(2)
	}
	cfg.Refresh = *delay

	dk, err := newDocker()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dockertop:", err)
		os.Exit(1)
	}
	info, err := dk.info()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dockertop: cannot reach the Docker daemon at %s: %v%s\n", dk.Host, err, permHint(err))
		os.Exit(1)
	}

	scr, err := tcell.NewScreen()
	if err == nil {
		err = scr.Init()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dockertop:", err)
		os.Exit(1)
	}
	a := &app{scr: scr, cfg: cfg, dk: dk, info: info, sortBy: indexOf(sortNames, *sortFlag), psortBy: indexOf(psortNames, cfg.ProcSort),
		tree: *tree, all: !*running, ptree: cfg.ProcTree, fullCmd: cfg.FullCmd, openName: flag.Arg(0)}
	a.refresh()
	events := make(chan tcell.Event, 64)
	go func() {
		for {
			ev := scr.PollEvent()
			if ev == nil {
				return
			}
			events <- ev
		}
	}()
	tick := time.NewTicker(time.Duration(cfg.Refresh * float64(time.Second)))
loop:
	for {
		a.draw()
		select {
		case ev := <-events:
			if a.handle(ev) {
				break loop
			}
		case <-tick.C:
			a.tick()
		}
	}
	scr.Fini()
	cfg.Sort, cfg.ProcSort, cfg.All, cfg.Tree, cfg.ProcTree, cfg.FullCmd = sortNames[a.sortBy], psortNames[a.psortBy], a.all, a.tree, a.ptree, a.fullCmd
	if err := cfg.save(); err != nil {
		fmt.Fprintln(os.Stderr, "dockertop: saving config:", err)
	}
}

func (a *app) tick() {
	a.refresh()
	switch a.view {
	case viewProcs:
		a.sampleProcs()
	case viewText:
		a.loadText()
	}
}

// refresh starts a background collection unless one is in flight; the
// result comes back as a snapMsg event.
func (a *app) refresh() {
	if a.loading {
		return
	}
	a.loading = true
	prev := a.byID
	go func() {
		cs, err := a.dk.collect(prev)
		a.scr.PostEvent(tcell.NewEventInterrupt(snapMsg{cs, err}))
	}()
}

func (a *app) apply(m snapMsg) {
	a.loading = false
	a.err = m.err
	if m.err != nil {
		return
	}
	first := a.snap == nil
	a.snap = m.cs
	a.byID = make(map[string]Container, len(m.cs))
	for _, c := range m.cs {
		a.byID[c.ID] = c
	}
	if c, ok := a.byID[a.cur.ID]; ok {
		a.cur = c
	}
	a.rebuild()
	if first {
		a.refresh() // CPU and rates need a second sample; don't make the user wait a whole interval
		if a.openName != "" {
			a.openByName()
		}
	}
}

func (a *app) openByName() {
	n := a.openName
	a.openName = ""
	for _, c := range a.snap {
		if c.Name == n || strings.HasPrefix(c.ID, n) {
			a.openProcs(c)
			return
		}
	}
	a.notice("No such container", fmt.Sprintf("No container is named %q or has an ID starting with it.", n))
}

// ---- container list ----

func stateRank(s string) int {
	return strings.Index("running restarting paused created exited dead", s)
}

func (a *app) less(x, y Container) bool {
	var d float64
	switch sortNames[a.sortBy] {
	case "CPU":
		d = x.CPU - y.CPU
	case "MEM":
		d = float64(x.Mem) - float64(y.Mem)
	case "NET":
		d = x.NetRx + x.NetTx - y.NetRx - y.NetTx
	case "IO":
		d = x.BlkR + x.BlkW - y.BlkR - y.BlkW
	case "PIDS":
		d = float64(x.Pids) - float64(y.Pids)
	case "IMAGE":
		if x.Image != y.Image {
			return x.Image < y.Image
		}
	case "STATE":
		if x.State != y.State {
			return stateRank(x.State) < stateRank(y.State)
		}
	}
	if d != 0 {
		return d > 0
	}
	return x.Name < y.Name
}

func (a *app) visible(c Container) bool {
	if !a.all && c.State != "running" && c.State != "paused" && c.State != "restarting" {
		return false
	}
	f := strings.ToLower(a.filter)
	return f == "" || strings.Contains(strings.ToLower(c.Name+" "+c.ID[:12]+" "+c.Image+" "+c.Project+" "+c.Status), f)
}

func (a *app) rebuild() {
	selID := ""
	if a.sel < len(a.rows) {
		selID = a.rows[a.sel].ID
	}
	var vis []Container
	for _, c := range a.snap {
		if a.visible(c) {
			vis = append(vis, c)
		}
	}
	sort.SliceStable(vis, func(i, j int) bool { return a.less(vis[i], vis[j]) })
	a.rows = vis
	if a.tree {
		a.rows = groupRows(vis, a.less)
	}
	a.sel = 0
	for i, c := range a.rows {
		if c.ID == selID {
			a.sel = i
		}
	}
}

// groupRows puts containers under one summary row per compose project.
// cs is already sorted; containers outside any project stay top level.
func groupRows(cs []Container, less func(x, y Container) bool) []Container {
	kids := map[string][]Container{}
	var tops []Container
	for _, c := range cs {
		if c.Project == "" {
			tops = append(tops, c)
			continue
		}
		if kids[c.Project] == nil {
			tops = append(tops, Container{ID: "project:" + c.Project, Name: c.Project, Project: c.Project, Group: true, State: "exited"})
		}
		kids[c.Project] = append(kids[c.Project], c)
	}
	for i := range tops {
		g := &tops[i]
		if !g.Group {
			continue
		}
		up := 0
		for _, c := range kids[g.Project] {
			g.CPU += c.CPU
			g.Mem += c.Mem
			g.Pids += c.Pids
			g.NetRx, g.NetTx, g.BlkR, g.BlkW = g.NetRx+c.NetRx, g.NetTx+c.NetTx, g.BlkR+c.BlkR, g.BlkW+c.BlkW
			g.HasStats, g.HasRate = g.HasStats || c.HasStats, g.HasRate || c.HasRate
			if c.State == "running" {
				up++
				g.State = "running"
			}
		}
		g.Status = fmt.Sprintf("%d/%d running", up, len(kids[g.Project]))
	}
	sort.SliceStable(tops, func(i, j int) bool { return less(tops[i], tops[j]) })
	var out []Container
	for _, t := range tops {
		out = append(out, t)
		k := kids[t.Project]
		if !t.Group {
			continue
		}
		for i, c := range k {
			c.Tree = "├─ "
			if i == len(k)-1 {
				c.Tree = "└─ "
			}
			out = append(out, c)
		}
	}
	return out
}

// ---- process view ----

func (a *app) openProcs(c Container) {
	if c.Group {
		return
	}
	a.cur, a.view = c, viewProcs
	a.psel, a.ptop, a.pfilter, a.pprev, a.procs, a.remote = 0, 0, "", nil, nil, nil
	a.insp, _ = a.dk.inspect(c.ID)
	a.users = nil
	if a.insp != nil {
		a.users = containerUsers(a.insp.State.Pid)
	}
	a.sampleProcs()
	// CPU% needs a second reading: take one shortly instead of a whole interval later.
	time.AfterFunc(700*time.Millisecond, func() { a.scr.PostEvent(tcell.NewEventInterrupt("procs")) })
}

func (a *app) sampleProcs() {
	if a.cur.State != "running" && a.cur.State != "paused" {
		a.procs, a.remote, a.procErr = nil, nil, fmt.Errorf("container is %s", a.cur.State)
		a.rebuildProcs()
		return
	}
	t, err := a.dk.top(a.cur.ID)
	a.procErr = err
	if err != nil {
		a.procs, a.remote = nil, nil
		a.rebuildProcs()
		return
	}
	col := -1
	for i, h := range t.Titles {
		if h == "PID" {
			col = i
		}
	}
	var pids []int
	for _, row := range t.Processes {
		if col >= 0 && col < len(row) {
			if pid, err := strconv.Atoi(row[col]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	if len(pids) > 0 && !isLocal(pids[0], a.cur.ID) {
		a.procs, a.remote = nil, t
		return
	}
	a.remote = nil
	now := time.Now()
	a.procs = readProcs(pids, a.pprev, now.Sub(a.pat).Seconds())
	a.pprev, a.pat = make(map[int]uint64, len(a.procs)), now
	for i := range a.procs {
		p := &a.procs[i]
		a.pprev[p.PID] = p.Ticks
		p.User = a.users[p.UID]
		switch {
		case p.User != "":
		case p.UID == 0:
			p.User = "root"
		default:
			p.User = strconv.Itoa(p.UID)
		}
	}
	a.rebuildProcs()
}

func (a *app) memPct(p Proc) float64 {
	if a.cur.MemLimit == 0 {
		return 0
	}
	return float64(p.RSS) * 100 / float64(a.cur.MemLimit)
}

func (a *app) pless(x, y Proc) bool {
	switch psortNames[a.psortBy] {
	case "MEM":
		return x.RSS > y.RSS
	case "TIME":
		return x.Ticks > y.Ticks
	case "PID":
		return x.PID < y.PID
	case "USER":
		return x.User < y.User
	case "COMMAND":
		return x.Comm < y.Comm
	}
	if x.CPU != y.CPU {
		return x.CPU > y.CPU
	}
	return x.PID < y.PID
}

func (a *app) pvisible(p Proc) bool {
	f := strings.ToLower(a.pfilter)
	return f == "" || strings.Contains(strings.ToLower(p.Cmd+" "+p.Comm+" "+p.User+" "+strconv.Itoa(p.PID)), f)
}

func (a *app) rebuildProcs() {
	selPID := -1
	if a.psel < len(a.prows) {
		selPID = a.prows[a.psel].PID
	}
	a.prows = a.prows[:0]
	if a.ptree {
		a.buildTree()
	} else {
		for _, p := range a.procs {
			if a.pvisible(p) {
				a.prows = append(a.prows, p)
			}
		}
		sort.SliceStable(a.prows, func(i, j int) bool { return a.pless(a.prows[i], a.prows[j]) })
	}
	a.psel = 0
	for i, p := range a.prows {
		if p.PID == selPID {
			a.psel = i
		}
	}
}

// buildTree lays processes out by parent, htop style. A process is shown if it
// or any descendant passes the filter, so ancestors stay for context.
func (a *app) buildTree() {
	kids := map[int][]Proc{}
	present := map[int]bool{}
	for _, p := range a.procs {
		present[p.PID] = true
		kids[p.PPID] = append(kids[p.PPID], p)
	}
	for _, k := range kids {
		sort.SliceStable(k, func(i, j int) bool { return a.pless(k[i], k[j]) })
	}
	keep := map[int]bool{}
	var mark func(p Proc) bool
	mark = func(p Proc) bool {
		ok := a.pvisible(p)
		for _, c := range kids[p.PID] {
			if mark(c) {
				ok = true
			}
		}
		keep[p.PID] = ok
		return ok
	}
	var walk func(p Proc, prefix, branch string)
	walk = func(p Proc, prefix, branch string) {
		p.Tree = prefix + branch
		a.prows = append(a.prows, p)
		var shown []Proc
		for _, c := range kids[p.PID] {
			if keep[c.PID] {
				shown = append(shown, c)
			}
		}
		childPrefix := prefix
		if branch == "├─ " {
			childPrefix += "│  "
		} else if branch == "└─ " {
			childPrefix += "   "
		}
		for i, c := range shown {
			b := "├─ "
			if i == len(shown)-1 {
				b = "└─ "
			}
			walk(c, childPrefix, b)
		}
	}
	var roots []Proc
	for _, p := range a.procs {
		if !present[p.PPID] || p.PPID == p.PID {
			roots = append(roots, p)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return a.pless(roots[i], roots[j]) })
	for _, r := range roots {
		if mark(r) {
			walk(r, "", "")
		}
	}
}

// ---- text views: details and logs ----

func (a *app) openText(c Container, title string, logs bool) {
	if c.Group {
		return
	}
	a.cur, a.textTitle, a.textFrom, a.view = c, title, a.view, viewText
	a.ttop, a.tleft, a.follow, a.showEnv = 0, 0, logs, false
	insp, err := a.dk.inspect(c.ID)
	if err != nil {
		a.text, a.textErr, a.textLoad = nil, err, nil
		return
	}
	if logs {
		a.textLoad = func() ([]string, error) { return a.dk.logs(c.ID, insp.Config.Tty, 2000) }
	} else {
		a.textLoad = func() ([]string, error) {
			i, err := a.dk.inspect(c.ID)
			if err != nil {
				return nil, err
			}
			return i.details(a.cur, a.showEnv), nil
		}
	}
	a.loadText()
}

func (a *app) loadText() {
	if a.textLoad == nil {
		return
	}
	t, err := a.textLoad()
	a.textErr = err
	if err == nil {
		a.text = t
	}
}

func (a *app) textAvail() int {
	_, h := a.scr.Size()
	return max(h-3, 1)
}

// ---- input ----

func (a *app) handle(ev tcell.Event) (quit bool) {
	switch ev := ev.(type) {
	case *tcell.EventResize:
		a.scr.Sync()
	case *tcell.EventInterrupt:
		switch d := ev.Data().(type) {
		case snapMsg:
			a.apply(d)
		case string:
			if d == "done" {
				a.busy = false
				a.refresh()
				if a.view == viewProcs {
					a.sampleProcs()
				}
			} else if d == "procs" && a.view == viewProcs {
				a.sampleProcs()
			}
		}
	case *tcell.EventKey:
		return a.key(ev)
	}
	return false
}

func (a *app) modalKey(ev *tcell.EventKey) {
	m := a.modal
	switch {
	case m.items != nil:
		switch ev.Key() {
		case tcell.KeyUp:
			m.pick = max(m.pick-1, 0)
		case tcell.KeyDown:
			m.pick = min(m.pick+1, len(m.items)-1)
		case tcell.KeyEnter:
			a.modal = nil
			m.onPick(m.pick)
		case tcell.KeyEscape:
			a.modal = nil
		case tcell.KeyRune:
			if ev.Rune() == 'q' {
				a.modal = nil
			}
		}
	case m.onYes != nil && (ev.Rune() == 'y' || ev.Rune() == 'Y'):
		a.modal = nil
		m.onYes()
	case m.onYes == nil || ev.Key() == tcell.KeyEscape || ev.Rune() == 'n' || ev.Rune() == 'N' || ev.Rune() == 'q':
		a.modal = nil
	}
}

func (a *app) search(ev *tcell.EventKey) {
	f, rebuild := &a.filter, a.rebuild
	if a.view == viewProcs {
		f, rebuild = &a.pfilter, a.rebuildProcs
	}
	switch ev.Key() {
	case tcell.KeyEscape:
		*f, a.searching = "", false
	case tcell.KeyEnter:
		a.searching = false
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if r := []rune(*f); len(r) > 0 {
			*f = string(r[:len(r)-1])
		}
	case tcell.KeyRune:
		*f += string(ev.Rune())
	}
	rebuild()
}

func (a *app) back() {
	switch a.view {
	case viewText:
		a.view = a.textFrom
	case viewHelp:
		a.view = a.helpFrom
	default:
		a.view = viewList
	}
}

func (a *app) key(ev *tcell.EventKey) bool {
	if ev.Key() == tcell.KeyCtrlC {
		return true
	}
	if a.modal != nil {
		a.modalKey(ev)
		return false
	}
	if a.searching {
		a.search(ev)
		return false
	}
	if a.view == viewHelp {
		a.back()
		return false
	}
	v, n, pos := a.view, len(a.rows), &a.sel
	switch v {
	case viewProcs:
		n, pos = len(a.prows), &a.psel
		if a.remote != nil {
			n = len(a.remote.Processes)
		}
	case viewText:
		n, pos = len(a.text)-a.textAvail()+1, &a.ttop
	}
	_, h := a.scr.Size()
	pg := max(h-8, 1)
	switch ev.Key() {
	case tcell.KeyUp:
		*pos--
	case tcell.KeyDown:
		*pos++
	case tcell.KeyPgUp:
		*pos -= pg
	case tcell.KeyPgDn:
		*pos += pg
	case tcell.KeyHome:
		*pos = 0
	case tcell.KeyEnd:
		*pos = n - 1
	case tcell.KeyLeft:
		a.tleft = max(a.tleft-8, 0)
	case tcell.KeyRight:
		a.tleft += 8
	case tcell.KeyEnter:
		if a.view == viewList && a.sel < len(a.rows) {
			a.openProcs(a.rows[a.sel])
			return false
		}
	case tcell.KeyEscape:
		a.back()
		return false
	case tcell.KeyRune:
		if a.rune(ev.Rune()) {
			return true
		}
	}
	if v == viewText && a.view == viewText {
		n = len(a.text) - a.textAvail() + 1
		a.follow = *pos >= n-1
	}
	*pos = max(0, min(*pos, n-1))
	return false
}

func (a *app) rune(r rune) (quit bool) {
	c, ok := a.current()
	switch r {
	case 'q':
		if a.view == viewList {
			return true
		}
		a.back()
	case '?', 'h':
		a.helpFrom, a.view = a.view, viewHelp
	case 'c':
		a.fullCmd = !a.fullCmd
	case 'e':
		if a.view == viewText && a.textTitle == "Details" {
			a.showEnv = !a.showEnv
			a.loadText()
		}
	}
	if a.view == viewText {
		return false
	}
	switch r {
	case '/':
		a.searching = true
	case 's':
		if a.view == viewProcs {
			a.psortBy = (a.psortBy + 1) % len(psortNames)
			a.rebuildProcs()
		} else {
			a.sortBy = (a.sortBy + 1) % len(sortNames)
			a.rebuild()
		}
	case 't':
		if a.view == viewProcs {
			a.ptree = !a.ptree
			a.rebuildProcs()
		} else {
			a.tree = !a.tree
			a.rebuild()
		}
	case 'a':
		if a.view == viewList {
			a.all = !a.all
			a.rebuild()
		}
	}
	if !ok {
		return false
	}
	switch r {
	case 'd':
		a.openText(c, "Details", false)
	case 'l':
		a.openText(c, "Logs", true)
	case 'x':
		a.askStartStop(c)
	case 'r':
		a.askAction(c, "Restart", "restart", "Stop and start "+c.Name+" again?")
	case 'p':
		if c.State == "paused" {
			a.askAction(c, "Unpause", "unpause", "Resume every process in "+c.Name+"?")
		} else {
			a.askAction(c, "Pause", "pause", "Freeze every process in "+c.Name+" (cgroup freezer)?")
		}
	case 'k':
		if a.view == viewProcs {
			a.askSignalProc()
		} else {
			a.askKill(c)
		}
	}
	return false
}

// current is the container actions apply to: the selected row, or the one being viewed.
func (a *app) current() (Container, bool) {
	if a.view != viewList {
		return a.cur, true
	}
	if a.sel < len(a.rows) && !a.rows[a.sel].Group {
		return a.rows[a.sel], true
	}
	return Container{}, false
}

func (a *app) notice(title string, lines ...string) { a.modal = &modal{title: title, lines: lines} }

func (a *app) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
	a.scr.PostEvent(tcell.NewEventInterrupt(nil))
}

func (a *app) runJob(fn func() error) {
	if a.busy {
		a.notice("Busy", "Another action is still running.")
		return
	}
	a.busy = true
	go func() {
		if err := fn(); err != nil {
			a.setStatus("Error: " + err.Error())
		}
		a.scr.PostEvent(tcell.NewEventInterrupt("done"))
	}()
}

func (a *app) askAction(c Container, title, call, question string) {
	a.modal = &modal{title: title, lines: []string{question}, onYes: func() {
		a.runJob(func() error {
			a.setStatus(title + " " + c.Name + "...")
			start := time.Now()
			if err := a.dk.action(c.ID, call); err != nil {
				return err
			}
			a.setStatus(fmt.Sprintf("%s %s: done in %s.", title, c.Name, time.Since(start).Round(100*time.Millisecond)))
			return nil
		})
	}}
}

func (a *app) askStartStop(c Container) {
	switch c.State {
	case "running", "paused", "restarting":
		a.askAction(c, "Stop", "stop", "Stop "+c.Name+"? (SIGTERM, then SIGKILL after the stop timeout, 10s by default)")
	default:
		a.askAction(c, "Start", "start", "Start "+c.Name+"?")
	}
}

func signalItems() []string {
	items := make([]string, len(signals))
	for i, s := range signals {
		items[i] = fmt.Sprintf("%2d %s", int(s.sig), s.name)
	}
	return items
}

func (a *app) askKill(c Container) {
	a.modal = &modal{title: "Send signal to container " + c.Name + " (its main process)", items: signalItems(), onPick: func(i int) {
		a.runJob(func() error {
			if err := a.dk.action(c.ID, "kill?signal="+signals[i].name); err != nil {
				return err
			}
			a.setStatus(fmt.Sprintf("Sent %s to %s.", signals[i].name, c.Name))
			return nil
		})
	}}
}

func (a *app) askSignalProc() {
	if a.remote != nil || a.psel >= len(a.prows) {
		a.notice("Not available", "Processes can only be signalled when the Docker daemon runs on this machine.")
		return
	}
	p := a.prows[a.psel]
	a.modal = &modal{title: fmt.Sprintf("Send signal to %s (host PID %d)", p.Comm, p.PID), items: signalItems(), onPick: func(i int) {
		if err := syscall.Kill(p.PID, signals[i].sig); err != nil {
			a.notice("Signal failed", err.Error()+permHint(err))
			return
		}
		a.setStatus(fmt.Sprintf("Sent %s to %s (%d).", signals[i].name, p.Comm, p.PID))
	}}
}

func permHint(err error) string {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return " (try: sudo dockertop, or join the docker group)"
	}
	return ""
}

// ---- drawing ----

func (a *app) put(x, y int, s string, st tcell.Style) int {
	w, _ := a.scr.Size()
	for _, r := range s {
		if x >= w {
			break
		}
		a.scr.SetContent(x, y, r, nil, st)
		x++
	}
	return x
}

func (a *app) fill(y int, st tcell.Style) {
	w, _ := a.scr.Size()
	for x := 0; x < w; x++ {
		a.scr.SetContent(x, y, ' ', nil, st)
	}
}

type seg struct {
	frac float64
	st   tcell.Style
}

// bar draws an htop style  Label[|||||      text]  of the given total width.
func (a *app) bar(x, y, width int, label string, segs []seg, text string) {
	x = a.put(x, y, label, stLabel)
	x = a.put(x, y, "[", stDefault)
	inner := width - len(label) - 2
	if inner < 4 {
		return
	}
	pos := 0
	for _, s := range segs {
		n := min(int(s.frac*float64(inner)+0.5), inner-pos)
		for i := 0; i < n; i++ {
			a.scr.SetContent(x+pos, y, '|', nil, s.st)
			pos++
		}
	}
	if len(text) < inner {
		a.put(x+inner-len(text), y, text, stDim)
	}
	a.put(x+inner, y, "]", stDefault)
}

// bars draws two bars side by side, or stacked on a narrow terminal. Returns the next line.
func (a *app) bars(y, w int, l1 string, s1 []seg, t1, l2 string, s2 []seg, t2 string) int {
	if w >= 100 {
		bw := (w - 2) / 2
		a.bar(1, y, bw, l1, s1, t1)
		a.bar(1+bw+1, y, bw, l2, s2, t2)
		return y + 1
	}
	a.bar(1, y, w-2, l1, s1, t1)
	a.bar(1, y+1, w-2, l2, s2, t2)
	return y + 2
}

func cpuStyle(p float64) tcell.Style {
	switch {
	case p >= 90:
		return stRed
	case p >= 50:
		return stYellow
	}
	return stGreen
}

func (a *app) drawStatus(y int) {
	a.mu.Lock()
	status := a.status
	a.mu.Unlock()
	st := stWarn
	if a.err != nil {
		status, st = "Error: "+a.err.Error(), stErr
	} else if strings.HasPrefix(status, "Error") {
		st = stErr
	}
	a.put(1, y, status, st)
}

func (a *app) drawHeader(w int) int {
	var cpu float64
	var mem uint64
	var rx, tx, br, bw float64
	count := map[string]int{}
	for _, c := range a.snap {
		cpu += c.CPU
		mem += c.Mem
		rx, tx, br, bw = rx+c.NetRx, tx+c.NetTx, br+c.BlkR, bw+c.BlkW
		count[c.State]++
	}
	ncpu := float64(max(a.info.NCPU, 1))
	y := a.bars(0, w,
		"CPU", []seg{{cpu / ncpu / 100, cpuStyle(cpu / ncpu)}}, fmt.Sprintf("%.1f%% of %d cores", cpu, a.info.NCPU),
		"Mem", []seg{{float64(mem) / float64(max(a.info.MemTotal, 1)), stGreen}}, human(mem)+"/"+human(a.info.MemTotal))
	x := a.put(1, y, fmt.Sprintf("Containers %d:  ", len(a.snap)), stDefault)
	x = a.put(x, y, fmt.Sprintf("%d running  ", count["running"]), stGreen)
	x = a.put(x, y, fmt.Sprintf("%d paused  ", count["paused"]), stYellow)
	x = a.put(x, y, fmt.Sprintf("%d stopped  ", count["exited"]+count["created"]+count["dead"]), stDim)
	if n := count["restarting"]; n > 0 {
		x = a.put(x, y, fmt.Sprintf("%d restarting  ", n), stMagenta)
	}
	a.put(x+1, y, fmt.Sprintf("Docker %s on %s (%s, cgroup v%s)", a.info.ServerVersion, a.info.Name, a.info.OperatingSystem, a.info.CgroupVersion), stDim)
	y++
	line := fmt.Sprintf("Net rx %s/s tx %s/s   Block read %s/s write %s/s   sort: %s", human(uint64(rx)), human(uint64(tx)), human(uint64(br)), human(uint64(bw)), sortNames[a.sortBy])
	if a.tree {
		line += "   tree"
	}
	if !a.all {
		line += "   running only (a: show all)"
	}
	if a.filter != "" {
		line += "   filter: " + a.filter
	}
	a.put(1, y, line, stDefault)
	a.drawStatus(y + 1)
	return y + 3
}

func (a *app) draw() {
	a.scr.Clear()
	w, h := a.scr.Size()
	switch a.view {
	case viewHelp:
		a.drawHelp()
	case viewProcs:
		a.drawProcs(w, h)
	case viewText:
		a.drawText(w, h)
	default:
		a.drawList(w, h)
	}
	a.drawBottom(w, h)
	if a.modal != nil {
		a.drawModal(w, h)
	}
	a.scr.Show()
}

func statusStyle(c Container) tcell.Style {
	switch {
	case c.Group && c.State == "running":
		return stLabel
	case c.Group:
		return stDim
	case strings.Contains(c.Status, "unhealthy"), c.State == "dead",
		c.State == "exited" && !strings.HasPrefix(c.Status, "Exited (0)"):
		return stRed
	case strings.Contains(c.Status, "starting"), c.State == "paused":
		return stYellow
	case c.State == "restarting":
		return stMagenta
	case c.State != "running":
		return stDim
	}
	return stDefault
}

// cell formats a stats value: blank without stats, "-" before the first rate.
func cell(c Container, rate bool, s string) string {
	switch {
	case !c.HasStats:
		return ""
	case rate && !c.HasRate:
		return "-"
	}
	return s
}

func (a *app) drawList(w, h int) {
	if a.snap == nil {
		a.put(1, 0, "Reading containers from "+a.dk.Host+"...", stDim)
		a.drawStatus(1)
		return
	}
	y := a.drawHeader(w)
	last := "IMAGE"
	if a.fullCmd {
		last = "COMMAND"
	}
	a.fill(y, stHdr)
	a.put(0, y, fmt.Sprintf(" %-30s %6s %7s %5s %7s %7s %7s %7s %5s  %-28s %s", "NAME", "CPU%", "MEM", "MEM%", "NET RX", "NET TX", "READ", "WRITE", "PIDS", "STATUS", last), stHdr)
	y++
	avail := h - 1 - y
	if avail < 1 {
		return
	}
	if a.sel < a.top {
		a.top = a.sel
	}
	if a.sel >= a.top+avail {
		a.top = a.sel - avail + 1
	}
	a.top = max(0, min(a.top, len(a.rows)-avail))
	if len(a.rows) == 0 {
		a.put(1, y, "No containers match.", stDim)
	}
	for i := a.top; i < len(a.rows) && i < a.top+avail; i++ {
		c := a.rows[i]
		sel := i == a.sel
		st := stDefault
		if c.Group {
			st = stLabel
		}
		pick := func(s tcell.Style) tcell.Style {
			if sel {
				return stSel
			}
			return s
		}
		if sel {
			a.fill(y, stSel)
		}
		x := a.put(0, y, " ", pick(st))
		x = a.put(x, y, c.Tree, pick(stDim))
		x = a.put(x, y, fmt.Sprintf("%-*.*s ", 30-len([]rune(c.Tree)), 30-len([]rune(c.Tree)), c.Name), pick(st))
		x = a.put(x, y, fmt.Sprintf("%6s ", cell(c, true, fmt.Sprintf("%.1f", c.CPU))), pick(cpuStyle(c.CPU)))
		x = a.put(x, y, fmt.Sprintf("%7s ", cell(c, false, human(c.Mem))), pick(st))
		mp := ""
		if c.MemLimit > 0 {
			mp = fmt.Sprintf("%.1f", c.MemPct())
		}
		x = a.put(x, y, fmt.Sprintf("%5s ", cell(c, false, mp)), pick(cpuStyle(c.MemPct())))
		x = a.put(x, y, fmt.Sprintf("%7s %7s %7s %7s %5s  ",
			cell(c, true, human(uint64(c.NetRx))), cell(c, true, human(uint64(c.NetTx))),
			cell(c, true, human(uint64(c.BlkR))), cell(c, true, human(uint64(c.BlkW))),
			cell(c, false, strconv.FormatUint(c.Pids, 10))), pick(st))
		x = a.put(x, y, fmt.Sprintf("%-28.28s ", c.Status), pick(statusStyle(c)))
		lastv := c.Image
		if a.fullCmd {
			lastv = c.Command
		}
		a.put(x, y, lastv, pick(stDim))
		y++
	}
}

func (a *app) drawProcs(w, h int) {
	c := a.cur
	x := a.put(1, 0, c.Name, stLabel)
	x = a.put(x+2, 0, c.Image, stDim)
	a.put(x+2, 0, c.Status, statusStyle(c))
	online := c.Online
	if online == 0 {
		online = a.info.NCPU
	}
	limit, limTxt := float64(max(online, 1))*100, fmt.Sprintf("%d cores", online)
	if a.insp != nil && a.insp.CPULimit() > 0 {
		limit, limTxt = a.insp.CPULimit(), fmt.Sprintf("limit %.2f CPUs", a.insp.CPULimit()/100)
	}
	memFrac := 0.0
	if c.MemLimit > 0 {
		memFrac = float64(c.Mem) / float64(c.MemLimit)
	}
	y := a.bars(1, w,
		"CPU", []seg{{c.CPU / limit, cpuStyle(c.CPU * 100 / limit)}}, fmt.Sprintf("%.1f%% of %s", c.CPU, limTxt),
		"Mem", []seg{{memFrac, cpuStyle(memFrac * 100)}}, human(c.Mem)+"/"+human(c.MemLimit))
	thr, run := 0, 0
	for _, p := range a.procs {
		thr += p.Threads
		if p.State == 'R' {
			run++
		}
	}
	pl := "unlimited"
	if c.PidsLimit > 0 && c.PidsLimit < 1<<62 {
		pl = strconv.FormatUint(c.PidsLimit, 10)
	}
	line := fmt.Sprintf("Tasks %d, %d thr; %d running   pids %d/%s   Net rx %s/s tx %s/s   Block read %s/s write %s/s",
		len(a.procs), thr, run, c.Pids, pl, human(uint64(c.NetRx)), human(uint64(c.NetTx)), human(uint64(c.BlkR)), human(uint64(c.BlkW)))
	if a.insp != nil && !a.insp.State.StartedAt.IsZero() && c.State == "running" {
		line += "   Uptime " + ago(time.Since(a.insp.State.StartedAt))
	}
	a.put(1, y, line, stDefault)
	y++
	line = "sort: " + psortNames[a.psortBy]
	if a.ptree {
		line += "   tree"
	}
	if a.pfilter != "" {
		line += "   filter: " + a.pfilter
	}
	x = a.put(1, y, line, stDim)
	if a.procErr != nil {
		a.put(x+3, y, "cannot list processes: "+a.procErr.Error(), stErr)
	} else if a.remote != nil {
		a.put(x+3, y, "Docker runs on another host: showing its ps output (CPU% is a lifetime average)", stWarn)
	}
	y++
	a.drawStatus(y)
	y += 2
	if a.remote != nil {
		a.drawRemote(y, h)
		return
	}
	a.fill(y, stHdr)
	a.put(0, y, fmt.Sprintf("%8s %-9s %3s %3s %6s %6s S %5s %5s %9s  %s", "PID", "USER", "PRI", "NI", "VIRT", "RES", "CPU%", "MEM%", "TIME+", "Command"), stHdr)
	y++
	avail := h - 1 - y
	if avail < 1 {
		return
	}
	if a.psel < a.ptop {
		a.ptop = a.psel
	}
	if a.psel >= a.ptop+avail {
		a.ptop = a.psel - avail + 1
	}
	a.ptop = max(0, min(a.ptop, len(a.prows)-avail))
	for i := a.ptop; i < len(a.prows) && i < a.ptop+avail; i++ {
		p := a.prows[i]
		sel := i == a.psel
		pick := func(s tcell.Style) tcell.Style {
			if sel {
				return stSel
			}
			return s
		}
		if sel {
			a.fill(y, stSel)
		}
		cpu := "-"
		if p.HasCPU {
			cpu = fmt.Sprintf("%.1f", p.CPU)
		}
		sst := stDefault
		switch p.State {
		case 'R':
			sst = stGreen
		case 'D':
			sst = stRed
		case 'Z', 'T', 't':
			sst = stYellow
		}
		x := a.put(0, y, fmt.Sprintf("%8d %-9.9s %3d %3d %6s %6s ", p.PID, p.User, p.Pri, p.Nice, human(p.Virt), human(p.RSS)), pick(stDefault))
		x = a.put(x, y, string(p.State)+" ", pick(sst))
		x = a.put(x, y, fmt.Sprintf("%5s ", cpu), pick(cpuStyle(p.CPU)))
		x = a.put(x, y, fmt.Sprintf("%5.1f %9s  ", a.memPct(p), cpuTime(p.Ticks)), pick(stDefault))
		x = a.put(x, y, p.Tree, pick(stDim))
		cmd := p.Comm
		if a.fullCmd {
			cmd = p.Cmd
		}
		a.put(x, y, cmd, pick(stDefault))
		y++
	}
}

// drawRemote shows docker top's table as-is, columns padded to fit.
func (a *app) drawRemote(y, h int) {
	t := a.remote
	wid := make([]int, len(t.Titles))
	for i, s := range t.Titles {
		wid[i] = len(s)
	}
	for _, r := range t.Processes {
		for i := range min(len(r), len(wid)) {
			wid[i] = max(wid[i], len(r[i]))
		}
	}
	row := func(cols []string) string {
		var b strings.Builder
		for i, s := range cols {
			if i < len(wid)-1 {
				fmt.Fprintf(&b, "%-*s ", wid[i], s)
			} else {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	a.fill(y, stHdr)
	a.put(0, y, row(t.Titles), stHdr)
	y++
	avail := h - 1 - y
	a.ptop = max(0, min(a.ptop, a.psel, len(t.Processes)-avail))
	if a.psel >= a.ptop+avail {
		a.ptop = a.psel - avail + 1
	}
	for i := a.ptop; i < len(t.Processes) && i < a.ptop+avail; i++ {
		st := stDefault
		if i == a.psel {
			a.fill(y, stSel)
			st = stSel
		}
		a.put(0, y, row(t.Processes[i]), st)
		y++
	}
}

func (a *app) drawText(w, h int) {
	x := a.put(1, 0, a.textTitle, stLabel)
	x = a.put(x+2, 0, a.cur.Name, stDefault)
	if a.textTitle == "Logs" {
		hint := "last 2000 lines, refreshed every tick"
		if a.follow {
			hint += ", following"
		}
		a.put(x+2, 0, hint, stDim)
	}
	if a.textErr != nil {
		a.put(1, 1, "Error: "+a.textErr.Error(), stErr)
	} else {
		a.drawStatus(1)
	}
	avail := a.textAvail()
	if a.follow {
		a.ttop = len(a.text)
	}
	a.ttop = max(0, min(a.ttop, len(a.text)-avail))
	for i := 0; i < avail && a.ttop+i < len(a.text); i++ {
		l := []rune(a.text[a.ttop+i])
		st := stDefault
		if len(l) > 0 && l[0] != ' ' && a.textTitle == "Details" {
			st = stLabel
		}
		if a.tleft < len(l) {
			a.put(0, 2+i, string(l[a.tleft:]), st)
		}
	}
}

func (a *app) drawHelp() {
	lines := []string{
		"dockertop " + version + " - containers and the processes inside them",
		"",
		"Container list",
		"  CPU%      share of one core (100 = one full core), like docker stats and htop",
		"  MEM/MEM%  memory in use, page cache that can be dropped excluded; percent of the container's limit (or host RAM)",
		"  NET RX/TX received and sent per second, all networks;  READ/WRITE  block I/O per second",
		"  t         tree: group containers under their compose project, with the project's totals",
		"",
		"Process view (Enter)",
		"  htop for one container: every process in its cgroup, with host PIDs.",
		"  CPU% is per core; MEM% is resident memory against the container's memory limit.",
		"  USER comes from the container's own /etc/passwd when readable (root), else the numeric UID.",
		"",
		"Actions (each asks for confirmation)",
		"  x         stop a running container, start a stopped one",
		"  r         restart          p  pause / unpause",
		"  k         send a signal: to the container's main process in the list, to the selected process in the process view",
		"",
		"Keys",
		"  Up/Down PgUp/PgDn Home/End  move     Enter  processes     d  details (e: environment)     l  logs (Left/Right scroll)",
		"  /  search     s  sort     t  tree     a  show stopped containers     c  command line     h  help     q  quit / back",
		"",
		"Settings (sorts, trees, a, c, refresh) are saved to " + configPath() + " on exit.",
		"Needs access to the Docker socket (docker group or root). Signalling processes and container user names need root.",
		"",
		"Press any key to return.",
	}
	for i, l := range lines {
		st := stDefault
		if i == 0 || (l != "" && l[0] != ' ') {
			st = stLabel
		}
		a.put(1, i, l, st)
	}
}

func (a *app) drawBottom(w, h int) {
	y := h - 1
	a.fill(y, stDefault)
	if a.searching {
		f := a.filter
		if a.view == viewProcs {
			f = a.pfilter
		}
		a.put(0, y, "Search: "+f+"_", stWarn)
		return
	}
	var keys [][2]string
	switch a.view {
	case viewList:
		keys = [][2]string{{"h", "Help"}, {"/", "Search"}, {"s", "Sort"}, {"t", "Tree"}, {"a", "All"}, {"c", "Cmd"}, {"⏎", "Procs"}, {"d", "Details"}, {"l", "Logs"}, {"x", "Stop"}, {"r", "Restart"}, {"p", "Pause"}, {"k", "Kill"}, {"q", "Quit"}}
	case viewProcs:
		keys = [][2]string{{"q", "Back"}, {"/", "Search"}, {"s", "Sort"}, {"t", "Tree"}, {"c", "Cmd"}, {"k", "Signal"}, {"d", "Details"}, {"l", "Logs"}, {"x", "Stop"}, {"r", "Restart"}, {"p", "Pause"}}
	case viewText:
		keys = [][2]string{{"q", "Back"}, {"↑↓", "Scroll"}, {"←→", "Pan"}, {"End", "Follow"}}
		if a.textTitle == "Details" {
			keys = append(keys, [2]string{"e", "Env"})
		}
	}
	if c, ok := a.current(); ok && a.view != viewText {
		for i := range keys {
			switch {
			case keys[i][0] == "x" && c.State != "running" && c.State != "paused" && c.State != "restarting":
				keys[i][1] = "Start"
			case keys[i][0] == "p" && c.State == "paused":
				keys[i][1] = "Unpause"
			}
		}
	}
	x := 0
	for _, k := range keys {
		x = a.put(x, y, " "+k[0], stKey)
		x = a.put(x, y, fmt.Sprintf("%-7s", k[1]), stFn)
	}
}

func (a *app) drawModal(w, h int) {
	m := a.modal
	lines := append([]string{}, m.lines...)
	switch {
	case m.items != nil:
		lines = append(lines, m.items...)
		lines = append(lines, "", "[Enter] send    [Esc] cancel")
	case m.onYes != nil:
		lines = append(lines, "", "[y] yes    [n] no")
	default:
		lines = append(lines, "", "[Enter] ok")
	}
	bw := len(m.title)
	for _, l := range lines {
		bw = max(bw, len(l))
	}
	bw = min(bw+4, w)
	bh := len(lines) + 2
	x0, y0 := (w-bw)/2, (h-bh)/2
	st := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorSilver)
	for y := y0; y < y0+bh; y++ {
		for x := x0; x < x0+bw; x++ {
			a.scr.SetContent(x, y, ' ', nil, st)
		}
	}
	a.put(x0+2, y0, m.title, st.Bold(true))
	for i, l := range lines {
		ls := st
		if m.items != nil && i >= len(m.lines) && i-len(m.lines) == m.pick {
			ls = stSel
			for x := x0 + 1; x < x0+bw-1; x++ {
				a.scr.SetContent(x, y0+1+i, ' ', nil, ls)
			}
		}
		a.put(x0+2, y0+1+i, l, ls)
	}
}

// human formats a byte count: 512B, 1.5K, 3.2M, 1.25G.
func human(b uint64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%dB", b)
	case b < 1<<20:
		return fmt.Sprintf("%.1fK", float64(b)/(1<<10))
	case b < 1<<30:
		return fmt.Sprintf("%.1fM", float64(b)/(1<<20))
	case b < 1<<40:
		return fmt.Sprintf("%.2fG", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.2fT", float64(b)/(1<<40))
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// config is ~/.config/dockertop/config: "key = value" lines, rewritten on exit.
type config struct {
	Sort     string // container list
	ProcSort string // process view
	All      bool   // show stopped containers
	Tree     bool   // group containers by compose project
	ProcTree bool
	FullCmd  bool
	Refresh  float64 // seconds
}

var defaults = config{Sort: "CPU", ProcSort: "CPU", All: true, Refresh: 2}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "dockertop", "config")
}

func loadConfig() config {
	c := defaults
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "sort":
			c.Sort = strings.ToUpper(v)
		case "proc_sort":
			c.ProcSort = strings.ToUpper(v)
		case "all":
			c.All = v == "true"
		case "tree":
			c.Tree = v == "true"
		case "proc_tree":
			c.ProcTree = v == "true"
		case "full_cmd":
			c.FullCmd = v == "true"
		case "refresh":
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				c.Refresh = f
			}
		}
	}
	return c
}

func (c config) save() error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(fmt.Sprintf(
		"# dockertop configuration; rewritten when dockertop exits. See dockertop(1).\nsort = %s\nproc_sort = %s\nall = %t\ntree = %t\nproc_tree = %t\nfull_cmd = %t\nrefresh = %g\n",
		c.Sort, c.ProcSort, c.All, c.Tree, c.ProcTree, c.FullCmd, c.Refresh)), 0o644)
}

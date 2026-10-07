# Changelog

## 1.0.0 — 2026-10-07

First release.

- Container list: CPU, memory and share of limit, network and block I/O
  rates, pids, status and image, with totals against the host in the header.
- Tree view grouping containers by compose project, with project totals.
- Process view per container, laid out like htop: host PID, user from the
  container's passwd, PRI, NI, VIRT, RES, state, CPU%, MEM%, TIME+,
  command; flat or as a tree.
- Details (inspect summary, environment hidden by default) and followed logs.
- Actions: stop/start, restart, pause/unpause, signal to the container or to
  one process.
- Search, sort, settings persisted in `~/.config/dockertop/config`.

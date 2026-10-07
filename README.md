# dockertop

htop-style view of Docker containers, and of the processes inside each one.
Linux only.

![dockertop](https://img.shields.io/badge/platform-linux-blue)

The main screen lists every container with CPU (100% = one core), memory and
its share of the container's limit, network and block I/O per second, process
count, status and image. The header totals CPU and memory across all
containers against the host. `t` groups containers under their compose
project with the project's totals.

`Enter` on a container opens an htop for it: every process in its cgroup with
host PID, user, priority, nice, VIRT, RES, state, CPU%, MEM% (of the
container's limit), TIME+ and command, flat or as a tree. The header shows the
container's CPU against its limit, memory against its limit, tasks, threads,
pids limit, I/O rates and uptime.

`d` shows details (image, command, state, health check, limits, networks,
ports, mounts, labels; environment hidden until you press `e`) and `l` follows
the logs. Actions, each confirmed first: stop/start (`x`), restart (`r`),
pause/unpause (`p`) and send a signal (`k`), to the container or, in the
process view, to the selected process. Search (`/`), sort (`s` to cycle, `N`/`C`/`M` for name, CPU, memory), show stopped
(`a`), command line (`c`), help (`h`). Settings persist in
`~/.config/dockertop/config`. See `man dockertop`.

It talks to the Engine API over the Docker socket directly, so you need to be
in the `docker` group or root. `dockertop NAME` opens straight into a
container's processes.

```sh
dockertop
dockertop -t -r          # grouped by compose project, running only
dockertop my-container   # straight into its process view
```

## Install

```sh
# Homebrew (Linux)
brew install andydixon/tap/dockertop

# Debian, Ubuntu
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://repo.dixon.cx/dixon.gpg | sudo tee /etc/apt/keyrings/dixon.gpg >/dev/null
echo "deb [signed-by=/etc/apt/keyrings/dixon.gpg] https://repo.dixon.cx/apt stable main" |
    sudo tee /etc/apt/sources.list.d/dixon.list
sudo apt update && sudo apt install dockertop

# Fedora, RHEL, AlmaLinux, Rocky
sudo curl -fsSL -o /etc/yum.repos.d/dixon.repo https://repo.dixon.cx/rpm/dixon.repo
sudo dnf install dockertop

# openSUSE
sudo zypper addrepo https://repo.dixon.cx/rpm/dixon.repo
sudo zypper install dockertop

# Arch Linux
curl -fsSL https://repo.dixon.cx/dixon.asc | sudo pacman-key --add -
sudo pacman-key --lsign-key C22FF7330668417C62C7A304CBC7951D7F2234D1
printf '\n[dixon]\nServer = https://repo.dixon.cx/arch/$arch\n' | sudo tee -a /etc/pacman.conf
sudo pacman -Sy dockertop

# From source
go install github.com/andydixon/dockertop@latest
# or
git clone https://github.com/andydixon/dockertop && cd dockertop && make && sudo make install
```

More at [repo.dixon.cx](https://repo.dixon.cx).

## Development

```sh
make            # build bin/dockertop
make test vet   # tests and go vet (CI runs these plus gofmt on every push)
make man        # preview the manpage
make install    # binary and manpage under /usr/local (PREFIX, DESTDIR)
```

Releases: tag `vX.Y.Z` and push it, then `make brew-formula` writes the
Homebrew formula for the tap and `make repo` builds and publishes the .deb,
.rpm and Arch packages to repo.dixon.cx.

## Licence

GPL-3.0-or-later. Andy Dixon, [www.dixon.cx](https://www.dixon.cx).

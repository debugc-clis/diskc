# diskc

`diskc` is a read-only Linux and macOS CLI for answering the first questions in a disk incident:

- Which filesystem is under pressure?
- Which files and directories consume real disk blocks?
- Which files are growing, and how quickly?
- Which Linux processes are writing them?
- Are deleted-but-open files retaining space?
- Is the pressure related to a database, Docker, mounts, inodes, I/O, or RAID?

It runs without `sudo` or root access. Permissions still determine which files and Linux processes it can see.

## Quick start

```bash
go build -o diskc ./cmd/diskc
./diskc
```

Most incidents only need one command:

| Command | Purpose |
| --- | --- |
| `diskc` | Investigate `/` with useful SRE defaults |
| `diskc light` | Take a fast, low-impact snapshot |
| `diskc list` | List mounted filesystems in a risk-first table |
| `diskc /var /data` | Inspect one or several explicit paths |
| `diskc -watch` | Refresh continuously |
| `diskc db` | Inspect database configuration and disk paths |
| `diskc docker` | Inspect Docker/containerd host storage |
| `diskc doc` | Diagnose diskc defaults, mount access, capacity, and process visibility |
| `diskc mac system` | Inspect macOS system storage safely |
| `diskc version` | Show build and platform information |

## What `diskc` does

Running `diskc` with no arguments:

1. Reads byte, reserved-space, and inode pressure for `/`.
2. Scans regular files on that filesystem to the default depth of four.
3. Ranks files and directories by allocated disk blocks, not apparent length.
4. Samples all candidate files and device activity during one shared three-second window.
5. Separately reports the largest and fastest-growing files.
6. Matches writable Linux file descriptors to the last observed device and inode of each reported file.
7. Detects deleted-but-open Linux files that still retain allocated blocks.
8. Estimates time-to-full from net filesystem available-byte loss during the sampling window; per-file growth identifies contributors. JSON labels the basis as `filesystem_available_bytes`.
9. Adds Linux mount, I/O pressure, RAID, overlay, and copy-on-write findings when available.

Hard links are counted once per scan. Files are ranked by allocated blocks. In normal, light, database, Docker, and macOS system text reports, file size rows add `[N apparent]` when the positive apparent length differs from allocated bytes. Existing directory size rows use the same annotation. JSON retains both `allocated_bytes` and `apparent_bytes`.

For example, `4.0 KB  /data/sparse.img  [1.0 GB apparent]` means the file has a 1 GB logical length but currently consumes 4 KB of allocated blocks. The annotation can also show a smaller apparent length, such as a small file occupying a full block; it does not identify the cause of the difference. Use allocated bytes when investigating current disk consumption.

## Default example

```text
$ diskc
Disk pressure: WARNING  92.4% unavailable (38.0 GB available, 42.0 GB free)
Filesystem usage: 91.6% used  |  4.0 GB reserved
Inodes: 41.0% used  |  /
Filesystem net growth: +65.5 GB/hour
Estimated full at net growth: ~35 minutes

Scan scope: / | max directory depth 4 | 12438 unique regular file(s) observed
Skipped directory boundaries: depth=87, other filesystem=2, pseudo filesystem=4 | symlinks=16 | inspection errors=0
Directory totals cover observed files only; excluded subtrees and inaccessible files are not included.
Next: increase --depth or inspect a specific subtree as a scan root.
Next: inspect linked or mounted data paths explicitly as separate scan roots.

Largest files
   74.8 GB  /var/log/payment/app.log  (log)  ↑ 18.2 MB/s  [1 writer(s)]
   42.1 GB  /tmp/upload-883120.bin  (temporary/cache)

Largest scanned directories
  128.4 GB  /var/log
   67.2 GB  /tmp

Fastest growth
   18.2 MB/s  /var/log/payment/app.log  (log)

Active writers
/var/log/payment/app.log
└─ PID 21819  payment-api
   ├─ exe      /opt/payment/bin/payment-api
   └─ service  payment.service

Deleted but still open (38.2 GB retained)
   38.2 GB  /var/log/payment/app-old.log  ← payment-api (PID 21819)
```

Directory totals cover only files observed within the selected depth and current user's permissions. If a very large scan exceeds the bounded directory tracker, affected directory rows are labeled `(estimated)` in normal, light, Docker, and macOS system text reports. JSON directory objects include `estimated: true` for these rows; the field is omitted for non-estimated rows. A scan warning also identifies bounded heavy-hitter estimates, though detailed warnings can be capped.

An estimated row can include counts carried over from an evicted directory, so it is an investigation lead rather than a measured total for that path. Inspect the reported subtree as an explicit scan root with an appropriate `--depth` to narrow the investigation; a large subtree can still exceed the tracker. Rows without the estimate marker still have the scan-scope limits described below.

Every successful scan reports its scope before the file rankings: the resolved root, maximum directory depth, unique regular files observed, and counts of directories skipped by depth, another filesystem, or pseudo-filesystem exclusions, plus skipped symbolic links and inspection errors. Depth is relative to the root: `--depth 0` includes files directly in the root; `--depth 1` also includes files in its immediate child directories. The scope describes the initial traversal, before candidate selection and growth sampling, so the observed file count can exceed the number of reported or sampled files.

Skip counts describe boundaries actually encountered, not the contents hidden behind them. A subtree is counted under the first applicable exclusion: pseudo-filesystem, depth, then filesystem device. Symbolic links inside skipped directories are not counted. Inspection errors are counted even when detailed warnings are capped. Zero counts do not establish complete filesystem visibility, and directory totals do not include directory metadata, other filesystems, deleted-open files, or unobserved contents. This scope is shown for normal, light, database, Docker, and macOS system scans.

When depth hides subtrees, increase `--depth` or inspect a relevant subtree explicitly, for example `diskc light /data/service --depth 4`. Inspect linked data directories or mounted filesystems as explicit roots when needed; an explicit remote path can block on a stale mount. Review warnings and access when inspection errors occur. These steps only collect evidence; no cleanup is performed.

Scan roots that are symbolic links are resolved before scanning. For example, `diskc light /var` on macOS scans `/private/var`. File and directory paths use the resolved target; the filesystem summary keeps the requested path. Symbolic links encountered inside the scanned tree are skipped, so linked data directories must be passed as separate scan roots. Broken or looping root links produce an inspection error.

File growth compares allocated blocks only when the scan and both sample observations identify the same regular file by device and inode. If a log is rotated or replaced, its current regular-file size is refreshed but its growth is marked `file replaced; resample`. A missing, unreadable, or non-regular path has unmeasured growth; the report retains its last observed regular-file size. Neither case enters the fastest-growth ranking. Run another sample to measure the current file. This applies to normal, database, Docker, and macOS system scans when sampling is enabled.

Linux writer attribution uses that file's last valid observation, even with `--sample 0`. If `/var/log/app.log` is renamed after observation and another file takes its place, the original file's writable descriptors remain the matches; the replacement's processes are not attached to the earlier evidence. Paths are historical labels and may have moved. Overlapping scans keep each observation's writers separate, and each process is listed once per file even when it holds several writable descriptors. A writable descriptor is an investigation lead, not proof that the process caused the sampled growth. Verify the PID, descriptor, and service before taking action; permissions and process exits can hide matches.

Deleted-open inspection confirms that each descriptor with a Linux ` (deleted)` label refers to a regular file with zero remaining hard links. A linked file whose name merely ends in ` (deleted)`, or an unlinked pathname whose inode still has another hard link, is excluded from retained-space results. Device, inode, allocated bytes, and apparent length come from one stat observation, and each inode is counted once. Read-only descriptors can retain space too; processes listed in this section are holders, not necessarily writers. Text and JSON preserve a literal ` (deleted)` in the original filename by removing only the kernel's final marker. Paths are historical labels; verify the PID, descriptor, and service before planning any action.

When Linux process inspection encounters an unreadable `/proc`, inaccessible process descriptor directories, or descriptor records it cannot inspect, text and JSON `warnings` identify writer attribution or deleted-open inspection as `unavailable` or `partial`. Visible matches and retained files are still reported; an empty writer result does not establish that no process writes the files, and retained-space results may be incomplete. Database and Docker reports include writer-attribution warnings on inspected paths. Normal process and descriptor disappearance is skipped without a warning. Run as the affected service user when practical and verify process visibility. Processes hidden entirely by `/proc` policy cannot be counted, so the absence of warnings does not establish full visibility. Light mode skips these checks, and macOS does not perform Linux process inspection.

## List filesystems

Use `diskc list` for an instant host view without traversing directory contents:

```text
$ diskc list
STATUS    MOUNT    SOURCE          TYPE/KIND     SIZE      USED      AVAIL    PRESS%  INODE%  MODE  CAPACITY
CRITICAL  /var     /dev/nvme0n1p3  ext4/local   180.0 GB  168.0 GB  7.8 GB   95.7%   42.1%   rw    dedicated
WARNING   /data    /dev/nvme1n1    xfs/local    1.8 TB    1.6 TB    136.0 GB  92.6%   18.4%   rw    dedicated
REMOTE    /backup  host:/backup    nfs4/remote  -         -         -         -       -       ro    -
```

Remote and unknown filesystem types are listed but not probed, which avoids blocking on a stale NFS, CIFS, or similar mount. `diskc --all` also excludes remote mounts. Inspecting an explicit path such as `diskc /backup` remains an intentional opt-in and can block if that mount is stale.

APFS rows that expose the same shared container capacity are marked `shared`; do not add those capacities together.

## Light mode

Use light mode for a quick first look on a busy production host:

```bash
diskc light
diskc light /var
diskc light --all
```

Light mode uses `--top 10 --depth 2 --sample 0`. It keeps capacity, inode, file, directory, and basic health checks while skipping growth waiting, Linux process descriptor inspection, and deleted-open-file discovery.

## Multiple filesystems

Explicit paths share one growth-sampling window, so three filesystems do not add three separate three-second waits:

```bash
diskc / /var /data
diskc --all
```

Host-wide PSI and RAID findings are printed once. Deleted-open files are printed once per device even when several requested paths reside on the same filesystem.

## Watch mode

```bash
diskc -watch
diskc -watch /var --interval 2s
diskc light -watch /var
```

Each refresh starts after the configured delay from the previous completed scan, preventing an expensive scan from turning into a continuous loop. Interactive terminals are redrawn; redirected text receives timestamped refresh separators and no terminal escape sequences.

JSON watch output is newline-delimited JSON (NDJSON), one complete compact JSON value per refresh:

```bash
diskc -watch --json --sample 0 >> diskc.ndjson
```

## Database storage

```bash
diskc db
diskc db --top 10 --sample 5s
diskc db --json
```

`diskc db` discovers common PostgreSQL, MySQL/MariaDB, Redis, ClickHouse, and MongoDB processes, configuration files, data directories, log paths, and disk settings. It does not connect to a database or modify data. All discovered paths share one sampling window, and unavailable paths and partial-scan warnings remain visible.

## Docker storage

```bash
diskc docker
diskc docker --top 10 --sample 5s
diskc docker --json
```

`diskc docker` checks configured and conventional rootful/rootless Docker data roots, containerd storage, Docker logs, BuildKit cache, sockets, and runtime processes. It does not call the Docker API and does not delete container data. All storage paths share one sampling window.

Directory output excerpt from a scan that exceeded the bounded tracker:

```text
  Largest directories
       81.0 GB  /var/lib/docker/overlay2  (estimated)
```

Use an explicit subtree scan such as `diskc light /var/lib/docker/overlay2 --depth 4` to investigate that lead. The same `(estimated)` marker appears beside affected directories in `diskc mac system`.

## Doctor

```bash
diskc doc
diskc doc /data
diskc doc --json
```

`diskc doc` is a quick health check similar to `brew doctor`. It shows the build and all built-in scan defaults, then checks mount discovery, access to the selected filesystem, capacity and inode pressure, and Linux `/proc` availability. The default target is `/`; pass a path to diagnose another filesystem. It does not walk files or wait for growth samples. Issues are findings in the report, while usage and runtime failures keep the normal exit codes. `diskc` has no configuration file; per-run flags override the defaults shown here.

Example:

```text
$ diskc doc /data
diskc doctor
Build: dev (none)  Go: go1.22.2  Platform: linux/amd64
Built: unknown
Target: /data

Configuration (built-in defaults; no configuration file)
  full: top=20 depth=4 sample=3s
  light: top=10 depth=2 sample=0s
  db: top=20 depth=4 sample=3s
  docker: top=20 depth=4 sample=3s
  mac system: top=20 depth=5 sample=0s
  all mounts: false  watch: false  json: false
  watch interval: 3s
  Linux process checks: true  deleted-open checks: true

Mount: /data  source=/dev/nvme1n1  type=xfs  kind=local

Checks
  [OK] mount inventory: 6 mounted filesystems discovered
  [WARNING] capacity: 92.6% unavailable; 136.0 GB available
       next: investigate large and growing files with diskc on this path
  [OK] inodes: 18.4% used
  [OK] process inspection: /proc is readable; other users' processes may still be hidden by permissions

1 issue(s) found.
```

## macOS system storage

```bash
diskc mac system
diskc mac system --top 10
diskc mac system --json
```

The command checks `/private/var`, `/Library`, `/Users`, `/Applications`, and `/System`. Large files receive conservative guidance such as `review before cleaning` or `do not delete manually`. Nothing is deleted automatically.

If a root's metadata, filesystem usage, or scan cannot be inspected, the report keeps that root with `Skipped:` and the failure reason, plus a warning. Review the error and access to that root before treating the report as incident evidence. JSON marks the root with `skipped: true` and `skip_reason`, without `scan_scope` or file rankings. Optional roots that are absent at the initial metadata check are omitted quietly; a root that disappears at a later stage is reported as skipped.

Traversal errors within an otherwise successful scan keep its observed files, `scan_scope.inspection_errors`, and warnings. If no regular files were observed and inspection errors occurred, text says `No regular files observed; inspection errors may hide files.` Other roots continue to be reported normally. These results describe what was visible to the current user, not complete macOS storage usage.

## JSON output

Use JSON for automation or incident evidence:

```bash
diskc /data --json > disk-report.json
diskc list --json > filesystem-report.json
diskc db --json > database-report.json
diskc docker --json > docker-report.json
diskc doc --json > doctor-report.json
diskc mac system --json > mac-report.json
```

Multiple root filesystems produce an array. File and directory objects include `allocated_bytes` and `apparent_bytes`. Filesystem objects distinguish:

- `used_bytes`: blocks consumed on the filesystem.
- `free_bytes`: all free blocks, including blocks unavailable to unprivileged users.
- `available_bytes`: blocks currently available to an unprivileged process.
- `reserved_bytes`: free blocks not available to an unprivileged process.
- `used_percent`: actual consumed capacity.
- `pressure_percent`: capacity unavailable to an unprivileged process; alert status uses this value.

Each successful scan includes a `scan_scope` object. Normal and light reports place it beside `filesystem`; database reports place it on each inspected `filesystems` entry, Docker on each inspected `paths` entry, and macOS system reports on each inspected `roots` entry. Failed scans do not claim a successful scope. All scope counters are present, including zeros:

| Field | Meaning |
| --- | --- |
| `root` | Absolute scan root after resolving symbolic links. |
| `max_depth` | Selected directory depth relative to that root; zero includes root files. |
| `regular_files` | Regular files observed on the selected device, with hard links counted once, before candidate pruning. |
| `skipped_depth_directories` | Encountered directories beyond the selected depth. |
| `skipped_filesystem_directories` | Encountered directories on another device. |
| `skipped_pseudo_directories` | Encountered directories excluded by the scanner's pseudo-filesystem path rules. |
| `skipped_symlinks` | Symbolic links encountered inside the scanned scope and left untraversed. |
| `inspection_errors` | Encountered traversal, stat, or metadata errors, independent of the warning limit. |

These are observations from a live, bounded scan. They are not an inventory of every excluded descendant or a completeness guarantee.

Sampled file objects also include `growth_status`:

| Value | Meaning |
| --- | --- |
| `measured` | The same regular file was identified at the scan and both sample observations; its rate may be positive, zero, or negative. |
| `replaced` | Device or inode changed since the scan or between sample observations; no rate was measured. |
| `unavailable` | A sample observation failed or the path was no longer a regular file; no rate was measured. |

The status is omitted when sampling is disabled. For `replaced` and `unavailable`, a numeric zero in `growth_bytes_per_second` is a placeholder, not evidence of zero growth. macOS system JSON omits zero rate values. File sampling never follows a replacement symlink.

Normal, database, and Docker file objects can include `writers` for visible Linux processes holding writable descriptors to the file's last observed identity. Identity metadata stays internal and is not added to JSON. An omitted `writers` field means no matching process was visible during inspection; it does not establish that no process writes the file.

## Options

The defaults are designed for incident response; tune them only when needed:

```text
--top N              largest and fastest-growing files to report (default: 20; max: 1000)
--depth N            maximum directory depth (default: 4)
--sample DURATION    growth window (default: 3s; 0 disables sampling)
--all                inspect automatically discovered local filesystems
--watch              refresh continuously
--interval DURATION  delay after each completed watch scan (default: 3s)
--json               emit JSON; watch mode emits NDJSON
```

Paths and flags can be interspersed, for example:

```bash
diskc --top 5 /var --json --sample 0
```

Use `--` before a path beginning with `-`. The former `--deleted` flag is accepted for compatibility but is no longer required.

## Exit status

| Code | Meaning |
| --- | --- |
| `0` | Success, help, or a downstream pipe that closed normally |
| `1` | Runtime inspection or output failure |
| `2` | Invalid flags, values, or command usage |

Help is written to stdout. Diagnostics are written to stderr. Text output escapes terminal control characters found in paths, process names, configuration values, or kernel-provided labels.

## Installation

Requires Go 1.22 or newer:

```bash
go install github.com/debugc-clis/diskc/cmd/diskc@latest
```

Or build from a clone:

```bash
go build -o diskc ./cmd/diskc
./diskc version
```

Tagged releases publish Linux and macOS archives for `amd64` and `arm64`, SHA-256 checksums, and archive SBOMs. Homebrew packaging is planned; do not rely on `brew install diskc` until a formula is published.

Install the included manual page locally if desired:

```bash
man ./docs/diskc.1
```

## Safety and limitations

- `diskc` is diagnostic and read-only. It does not delete, truncate, restart, kill, or modify anything.
- Run it as the affected service user when practical. Filesystem and Linux `/proc` permissions may hide files or processes owned by other users.
- Time-to-full uses net filesystem available-byte loss during the short sample, including activity outside scanned candidates. Recovery or deletion can offset growth; zero or negative net growth has no time-to-full estimate. Bursty workloads and block allocation granularity can change the result.
- File growth is a net change between observations. Truncation and regrowth of the same inode, inode reuse, or a replacement that is restored between observations can be missed; the rate does not measure total write throughput. Directory totals remain the initial scan snapshot even when file sizes are refreshed after sampling.
- Database files, Docker data, macOS system files, and application support data must not be deleted solely because they appear in a report.
- Copy-on-write, snapshots, compression, reflinks, quotas, and thin provisioning can make filesystem totals differ from ordinary file sums.
- `diskc` does not replace filesystem-specific snapshot, quota, SMART, NVMe, or vendor database tools.

## Disclaimer

Parts of this project may be generated or assisted by artificial intelligence. The project is provided “AS IS” without warranties. Use it at your own risk. The authors, maintainers, contributors, and distributors are not liable for damage, data loss, corruption, downtime, outages, security incidents, or other losses arising from use of all or any part of the project.

Review, test, and validate the code and its output before production use. See [DISCLAIMER.md](DISCLAIMER.md) for the full notice.

## Contributing

The runtime uses only the Go standard library.

```bash
gofmt -w cmd internal
go test ./...
go test -race ./...
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/diskc-linux ./cmd/diskc
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/diskc-macos ./cmd/diskc
```

Issues and pull requests are welcome at [github.com/debugc-clis/diskc](https://github.com/debugc-clis/diskc).

## License

MIT. See [LICENSE](LICENSE).

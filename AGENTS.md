# diskc iteration guide

The product goal is a simple, read-only disk incident CLI that helps an SRE identify pressure, fast growth, the responsible writer, and a safe next investigation step without root access.

For each iteration:

1. Act as principal SRE and product manager first. Check the current CLI behavior, help, README, recent changes, tests, and available user feedback. Pick one concrete operator problem with the greatest expected value. State its scenario and acceptance criteria before implementation.
2. Delegate bounded implementation or review work to staff-level Go agents when available. Keep write scopes separate. Prefer correctness and clear output over new flags or broad feature sets.
3. Preserve user changes. Inspect Git status in both this repository and the separate landing repository before editing. Never reset, force push, or commit unrelated work.
4. Add focused tests for behavior changes. Run relevant tests, then `go test ./...`, `go vet ./...`, and a Linux build when the change affects Linux behavior. Treat estimates and partial visibility honestly in text and JSON.
5. Update README, the manual page, and the landing page only for verified behavior. The landing page is a separate repository at `github.com/debugc-clis/diskc_landing`; its command list and examples should match `diskc --help` and actual report semantics.
6. Commit and push meaningful validated changes to `main` when available. Report the PM finding, implementation, tests, commit, landing update status, and the next likely improvement. If a run cannot safely publish, explain the blocker and leave reviewable work.

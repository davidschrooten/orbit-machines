# Workflow

How work gets done in `orbit-machines`. Read this before starting any task.

## Where we are

Phase 0 (Architecture & specification) is complete. See [`project-overview.md`](project-overview.md)
for the 8 build phases and their entry/completion criteria.

Keep the "Current state" section of [`../AGENTS.md`](../AGENTS.md) accurate whenever a phase
completes.

## Branching

`master` is the trunk. Every commit on `master` must pass `go test -race ./...`,
`golangci-lint run`, and `helm lint charts/orbit-machines`.

Cut a branch from `master` for every unit of work:

| Prefix | For |
|---|---|
| `feat/` | New capability (`feat/dispatcher-pod-reconciler`) |
| `fix/` | Bug fix (`fix/vt-resize-deadlock`) |
| `docs/` | Documentation only (`docs/architecture-spec`) |
| `chore/` | Tooling, dependencies, flake updates (`chore/bump-bubbletea`) |
| `refactor/` | Behaviour-preserving restructuring |
| `perf/` | Performance improvement with benchmark proof (`perf/frame-diff-allocs`) |
| `test/` | Tests only (`test/s3-sync-integration`) |

One concern per branch. Merge back with `--no-ff` and delete the feature branch:

```sh
git checkout master
git merge --no-ff feat/dispatcher-pod-reconciler -m "Merge branch 'feat/dispatcher-pod-reconciler': reconcile ephemeral machine pods"
git branch -d feat/dispatcher-pod-reconciler
```

## Commits

- **Imperative subject**, under 72 characters: `"Add S3 WIP bundle uploader on idle timeout"`.
- **Body explains why**, wrapped at 80 columns.
- **No `Co-authored-by` trailers.**
- Never commit secrets, kubeconfigs, or S3 credentials.

## The TDD loop

1. **Read first.** Consult the relevant doc in [`agent-docs/`](.) before writing code.
2. **Write a failing test (`_test.go`).**
   - Table-driven tests with `t.Run` and `t.Parallel()` where state is isolated.
   - Use consumer-defined interfaces and in-memory fakes over mocking frameworks.
   - For Bubble Tea components, test model transitions (`Update` + `View`) deterministically
     using `charmbracelet/x/exp/teatest`.
3. **Implement** the minimal Go code to pass the test.
4. **Run with the race detector:** `go test -race ./...`.
5. **Lint:** `golangci-lint run`.
6. **Update [`../AGENTS.md`](../AGENTS.md)** when completing a build phase.
7. **Commit, merge `--no-ff` to `master`, delete the branch.**

## Definition of done for a phase

A build phase is complete only when:
- All package tests pass under `go test -race ./...`.
- `golangci-lint run` reports zero issues.
- Any Helm chart changes pass `helm lint charts/orbit-machines` and `helm template`.
- `AGENTS.md` is updated to reflect the completed phase.

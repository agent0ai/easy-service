# Purpose

Select exact Git revisions and produce detached application checkouts.

## Ownership

- `revision.go` owns the Git mirror, commit/tag/release selection, wildcard matching and checkout.
- Git subprocesses use `internal/process`; GitHub HTTP is a replaceable boundary.

## Local Contracts

- Cache mirrors by source URL identity under DATA_DIR/git-mirrors so candidate repositories cannot overwrite the accepted source. Prune other owned mirror directories only after selection/return to the accepted deployment, with old-source polling stopped.
- Return only the selected lowercase, 40-character hexadecimal commit SHA; remove checkout Git metadata. Tag/release names and timestamps remain local selection inputs, not downstream deployment fields.
- Local working and bare repositories support commit/tag modes; only committed content is deployed.
- For local sources mounted across UIDs, pass exact repository and `.git` trust to Git's local `upload-pack` through its native fetch option. Git clears inherited command configuration before spawning that helper. Do not use wildcard/global trust or forward GitHub authentication to local helpers.
- Tag order is creator time, then tag name descending; releases exclude drafts/prereleases and order by publication time/name.
- Let Git sort tag metadata and resolve only through the first matching commit; non-commit tags are skipped without spawning a resolver for every older match.
- Bound Git selection/checkout operations to two minutes and GitHub calls to thirty seconds.
- Restrict authenticated release pagination/redirects to HTTPS `api.github.com`; reject malformed/repeated pages.
- Never persist tokens in Git URLs/config or expose raw/encoded credentials in diagnostics.
- Remote Git authentication uses one environment-scoped `http.extraHeader` setting; local helpers receive none.

## Work Guidance

- Keep credential environment scoped to Git subprocesses.
- Git/GitHub fakes must preserve command outputs, pagination and exact checkout verification.

## Verification

- `go test -race ./internal/revision`.
- Native Git configuration parsing verifies that the single scoped authentication header reaches Git without putting it in command arguments.
- Real-Git checks cover timestamp ties, annotated/lightweight tags, non-commit tags, source changes and metadata-free exact checkouts.
- When tests run as root, `TestLocalRepositoryAcrossUIDs` checks readable working/bare/linked-worktree repositories owned by root through an unprivileged UID, including shell syntax in paths and matching container mount ownership.
- `go test -race ./internal/supervisor -run TestLocalGit` verifies real local Git through deployment.

## Child DOX Index

No child docs.

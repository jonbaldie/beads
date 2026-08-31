# Thorny, apparently unclaimed upstream Beads issues

**Checked at:** 2026-08-13T15:58:16Z
**Upstream:** [`gastownhall/beads`](https://github.com/gastownhall/beads) at [`d1e725d9f35ba307518551b4e61b3d504fb41ec5`](https://github.com/gastownhall/beads/commit/d1e725d9f35ba307518551b4e61b3d504fb41ec5)

## Answer

The five thorniest open issues that met the inactivity test at the checked-at time are:

| Rank | Issue | AB | RA | D/S/C | CP | TD | Total |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | [#5269 — Migration marks recorded without verifying DDL took effect](https://github.com/gastownhall/beads/issues/5269) | 2 | 2 | 2 | 2 | 2 | **10** |
| 2 | [#4483 — stale dependency queries → Dolt connection exhaustion/hang](https://github.com/gastownhall/beads/issues/4483) | 2 | 2 | 2 | 1 | 2 | **9** |
| 3 | [#3613 — reliable embedded locking on 9P/virtio-fs/CIFS/NFS](https://github.com/gastownhall/beads/issues/3613) | 2 | 1 | 2 | 2 | 2 | **9** |
| 4 | [#4637 — shared-server connection can adopt a foreign WSL2/Windows Dolt](https://github.com/gastownhall/beads/issues/4637) | 2 | 1 | 2 | 2 | 2 | **9** |
| 5 | [#4356 — tracked-but-ignored migration ledger permanently blocks pulls](https://github.com/gastownhall/beads/issues/4356) | 2 | 1 | 2 | 2 | 2 | **9** |

Ties are ordered by blast radius, diagnostic uncertainty, and how much state must be manufactured to test safely.

## Method

### Candidate screen

GitHub's REST issue feed showed **478 open issues, 474 unassigned and 4 assigned** after excluding pull requests ([API endpoint](https://api.github.com/repos/gastownhall/beads/issues?state=open&per_page=100)). I screened the open-issue metadata and searched titles for data loss, silent failure, races, corruption, split-brain, deadlock, hangs, Windows, locking, sync, and migrations. I then read the body, comments, assignment/project metadata, and timeline cross-references for more than twenty high-risk candidates. Only GitHub, linked pull requests/comments, and upstream source were used.

### Inactivity criterion

An issue qualified only when all of these were true at the checked-at time:

1. **Unassigned** and in no GitHub project item visible through the issue API.
2. **No open linked or body-referencing pull request**, checked through issue timeline cross-references and open-PR search.
3. **No explicit active claim.** A statement such as “I will implement/rewrite this” excludes the issue until withdrawn. A work-in-progress statement in the preceding 30 days also excludes it. “Happy to test a patch” does not claim implementation.
4. **Not merely blocked or awaiting information.** `blocked`, `status/needs-info`, and `status/needs-repro` are treated as waiting states, not inactivity. None of the five selected issues carries one of those labels. Where evidence is intrinsically ambiguous, that is scored under reproduction ambiguity rather than silently treated as inactivity.

This is necessarily a point-in-time inference: private forks, local branches, and unposted work are not observable.

### Thorniness rubric

Each dimension is scored 0–2:

- **AB — architectural breadth:** 0 = local function; 1 = several components; 2 = storage/runtime contract or several execution modes.
- **RA — reproduction ambiguity:** 0 = deterministic small repro; 1 = special topology/state; 2 = intermittent, state-dependent, or multiple plausible causes.
- **D/S/C — data, sync, or concurrency risk:** 0 = presentation; 1 = recoverable availability/metadata; 2 = silent divergence, corruption, invalid history, or unsafe concurrency.
- **CP — cross-platform surface:** 0 = platform-neutral/local; 1 = multiple storage modes or environments; 2 = OS/filesystem/network matrix.
- **TD — test difficulty:** 0 = unit test; 1 = integration fixture; 2 = multi-process, fault-injection, soak, legacy database, or platform-specific test infrastructure.

## Findings

### 1. #5269 — Migration marks can lie about applied DDL (10/10)

The reporter corrected the original flatten hypothesis after a controlled run. The surviving defect is worse for migration safety: a database can contain v50–v53 migration marks while only part of those migrations' DDL exists. Plain index creation succeeded while dynamically prepared `ALTER` statements did not; no error surfaced, and recording the marks prevents a retry. The same lineage and versions applied correctly on macOS but incompletely on another environment, leaving no isolated causal repro ([correction and measured evidence](https://github.com/gastownhall/beads/issues/5269#issuecomment-5155803235)).

Why it is thorny:

- Repair requires defining schema “reality” independently of the migration cursor, not merely rerunning SQL.
- A verifier must cover historical migration signatures without making old migration bytes mutable or producing false positives on valid schema variants.
- The causal failure is environment/state-dependent, and safe regression fixtures need deliberately half-applied historical schemas.
- The cursor can permanently bless a broken database, so later commands fail far from the original migration.

Inactivity evidence: [the issue](https://github.com/gastownhall/beads/issues/5269) is open, unassigned, has no project item, no assignment event or PR cross-reference in its [timeline API](https://api.github.com/repos/gastownhall/beads/issues/5269/timeline), and no open PR mentioning `#5269` in the upstream PR search. Its only comment is the reporter's correction, not a claim.

### 2. #4483 — connection-pool exhaustion has more than one plausible mechanism (9/10)

The original report links stale post-migration `depends_on_id` queries to repeated plan-time errors, connection churn, and eventual server-wide hangs. Independent field evidence then found a second candidate: a schema-correct `JSON_ARRAYAGG` query occasionally wedged in `DateFormat.Eval`, while many idle connections accumulated ([independent goroutine/process-list evidence](https://github.com/gastownhall/beads/issues/4483#issuecomment-4832549143)). A later deployment reported millions of questions, tens of thousands of packet timeouts, and continuing stale-column errors ([second corroboration](https://github.com/gastownhall/beads/issues/4483#issuecomment-5194771002)). Current source still builds the `DATE_FORMAT` dependency aggregation in the counts query ([source](https://github.com/gastownhall/beads/blob/d1e725d9f35ba307518551b4e61b3d504fb41ec5/internal/storage/sqlbuild/counts.go#L15-L26)).

Why it is thorny:

- It spans schema migration leftovers, query generation, connection lifecycle, and go-mysql-server behavior.
- The symptom appears after long-lived load; the faithful health probe is itself the expensive/wedging query.
- At least three mechanisms may compound: plan failures, aggregation lock contention, and idle/error connection retention.
- A credible fix needs both narrow query tests and a bounded concurrent soak that proves connections return under failures.

Inactivity evidence: [#4483](https://github.com/gastownhall/beads/issues/4483) is open, unassigned, has no project item and no linked PR in its [timeline](https://api.github.com/repos/gastownhall/beads/issues/4483/timeline). The comments provide diagnostics and mitigation choices, but nobody claims implementation; open-PR search found no PR mentioning the issue.

### 3. #3613 — embedded locking across unreliable filesystems (9/10)

Embedded mode relies on a process-lifetime exclusive lock, but the report documents 9P, virtio-fs, CIFS/SMB, and some NFS configurations where the assumed `flock` semantics error or silently do not coordinate. Removing the lock reopens an embedded-engine initialization race, while an atomic-directory fallback introduces stale-owner recovery, host/PID identity, heartbeat, crash, and clock-skew policy ([issue and proposed design](https://github.com/gastownhall/beads/issues/3613)). Upstream's Unix lock implementation still directly uses non-blocking `unix.Flock` ([source](https://github.com/gastownhall/beads/blob/d1e725d9f35ba307518551b4e61b3d504fb41ec5/internal/lockfile/lock_shared_unix.go#L11-L29)).

Why it is thorny:

- The lock is a correctness boundary, not optional resilience; false success risks concurrent engine access, while false contention makes the workspace unusable.
- Filesystem-type detection is insufficient by itself because NFS/CIFS behavior depends on mount/server configuration.
- Distributed stale-lock reclamation cannot safely rely only on a local PID.
- Testing requires real or faithfully emulated filesystems and cross-host/process crash cases; ordinary temporary-directory tests cannot validate the contract.

Inactivity evidence: [#3613](https://github.com/gastownhall/beads/issues/3613) is open, unassigned, has no project item or comments, and its [timeline](https://api.github.com/repos/gastownhall/beads/issues/3613/timeline) contains no PR. The recent cross-reference from [#5717](https://github.com/gastownhall/beads/issues/5717) is another issue, not a claim or implementation.

### 4. #4637 — server identity across Windows/WSL2 boundaries (9/10)

A successful TCP connection is not proof that the answering Dolt serves the expected data directory. The reported Windows/WSL2 topology can mirror a WSL-bound port onto Windows loopback, causing native Windows `bd` to read and write the wrong server without warning ([field repro and code trace](https://github.com/gastownhall/beads/issues/4637)). The platform problem is structural: current Windows code can find a listener PID but explicitly cannot verify a process working directory and returns false from `isProcessInDir` ([source](https://github.com/gastownhall/beads/blob/d1e725d9f35ba307518551b4e61b3d504fb41ec5/internal/doltserver/doltserver_windows.go#L31-L98)).

Why it is thorny:

- PID/CWD checks cannot identify processes across the Windows/WSL kernel boundary and are vulnerable to lifecycle/reuse gaps.
- A robust marker likely needs an end-to-end server/database identity protocol, persistence semantics, and upgrade behavior for existing servers.
- Failure is silent cross-store mutation, so warn-only behavior may be inadequate, but fail-closed behavior risks breaking legitimate externally managed servers.
- Regression coverage needs native Windows plus WSL2 networking, not only cross-compilation.

Inactivity evidence: [#4637](https://github.com/gastownhall/beads/issues/4637) is open, unassigned, and has no project item or open linked PR. Related identity work [#5013](https://github.com/gastownhall/beads/pull/5013) and [#5143](https://github.com/gastownhall/beads/pull/5143) was merged, but neither closed this direct-connect/WSL2 issue. The maintainer handoff index explicitly says its listed handoffs are **not assigned to anyone** ([#5711](https://github.com/gastownhall/beads/issues/5711)); no comment claims this issue.

### 5. #4356 — tracked-but-ignored migration state deadlocks sync (9/10)

Older databases can have `ignored_schema_migrations` tracked in `HEAD` while it is also listed in `dolt_ignore`. Migration bookkeeping then dirties the tracked table, and every pull fails. Force-adding is a treadmill: it preserves the topology that makes the next migration fail. Independent fleet evidence produced a minimal repro and showed that every sync endpoint must untrack the table or a remaining clone can repollute the shared remote ([minimal repro and recovery correction](https://github.com/gastownhall/beads/issues/4356#issuecomment-5082772270)). Earlier fleet evidence also found that status surfaces can hide the relevant delta and that merge recovery can leave real issue/event rows unstaged ([field details](https://github.com/gastownhall/beads/issues/4356#issuecomment-4673374672)).

Why it is thorny:

- Repair crosses migration bookkeeping, Dolt ignore semantics, committed history, working-set preservation, and multi-clone convergence.
- Detection must distinguish a harmless untracked ignored table from a tracked ignored table whose rows must be preserved.
- A one-clone repair is insufficient; mixed repaired/unrepaired fleets can reintroduce the state.
- Tests need old-lineage databases, true merges/pulls, multiple endpoints, subsequent migrations, and assertions that user rows survive.

Inactivity evidence: [#4356](https://github.com/gastownhall/beads/issues/4356) is open, unassigned, has no project item and no open linked PR in its [timeline](https://api.github.com/repos/gastownhall/beads/issues/4356/timeline). Reporters offer to test a patch but do not claim to write one. Related [#4641](https://github.com/gastownhall/beads/issues/4641), [#5111](https://github.com/gastownhall/beads/issues/5111), and [#5257](https://github.com/gastownhall/beads/issues/5257) remain issues rather than active PRs.

## Rejected near-misses

| Issue | Why it was not shortlisted |
|---|---|
| [#5433 — embedded commits stall while push reports success](https://github.com/gastownhall/beads/issues/5433) | Very thorny, but a contributor explicitly said “Happy to open that PR,” and the reporter accepted the offer ([claim/diagnosis](https://github.com/gastownhall/beads/issues/5433#issuecomment-5231225777)); fails the inactivity criterion. |
| [#4380 — aux row re-key panic on encoding drift](https://github.com/gastownhall/beads/issues/4380) | Has open linked fix [PR #5064](https://github.com/gastownhall/beads/pull/5064). |
| [#3894 — sync audit-log storage design](https://github.com/gastownhall/beads/issues/3894) | Has open [PR #3717](https://github.com/gastownhall/beads/pull/3717), and the author explicitly offers to rewrite after a placement decision ([comment](https://github.com/gastownhall/beads/issues/3894#issuecomment-5228976246)). |
| [#4379 — offline write spool](https://github.com/gastownhall/beads/issues/4379) | A complete implementation was submitted as [PR #4520](https://github.com/gastownhall/beads/pull/4520) and closed only recently; this is declined/recent implementation work, not an untouched issue. |
| [#4132 — Windows Dolt crashes/probe storms](https://github.com/gastownhall/beads/issues/4132) | Multiple linked fixes exist, and open [PR #5630](https://github.com/gastownhall/beads/pull/5630) still addresses a remaining probe path. |
| [#3406 — destructive JSONL watcher race](https://github.com/gastownhall/beads/issues/3406) | Maintainer reproduction says current main no longer reproduces and lists merged fixes; issue was intentionally left open only until a post-v1.0.4 release ([triage comment](https://github.com/gastownhall/beads/issues/3406#issuecomment-4548422532)). |
| [#3926 — managed handoff rewrites port and creates split-brain](https://github.com/gastownhall/beads/issues/3926) | Merged [PR #4217](https://github.com/gastownhall/beads/pull/4217) directly addresses the handoff conflict; likely stale-open rather than unworked. |
| [#4052 — writes report success against unreachable shared server](https://github.com/gastownhall/beads/issues/4052) | Merged [PR #5163](https://github.com/gastownhall/beads/pull/5163) directly references and fixes the retargeting path. |
| [#3905 — one row refuses to close while events record success](https://github.com/gastownhall/beads/issues/3905) | Unclaimed and potentially severe, but only one historical v1.0.4 row was affected and direct SQL destroyed the state. It is best classified as **needs reproduction/artifact**, not as actionable inactivity. |
| [#5442 — label mutations do not bump `updated_at`](https://github.com/gastownhall/beads/issues/5442) | Qualifies as inactive and is P1, but the report identifies a narrow transaction-local remedy; it scored below the selected architecture/topology failures. |
| [#5657 — concurrent label mutations lose/duplicate labels](https://github.com/gastownhall/beads/issues/5657) | Strong, current, unclaimed sixth-place candidate. The deterministic same-issue repro and clear transactional remedy reduce RA/AB relative to the selected migration, filesystem, and identity problems. |
| [#5061 — compat migration sweeps concurrent writes into mislabeled commits](https://github.com/gastownhall/beads/issues/5061) | Reported against v1.0.2; the named `RunCompatMigrations` path is absent from checked main ([current code search](https://github.com/gastownhall/beads/search?q=RunCompatMigrations&type=code)), so current applicability needs revalidation first. |

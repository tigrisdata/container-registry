# Flaky test ownership and remediation

## Purpose

Reduce flaky tests continuously by assigning ownership each milestone.

### Rotation and ownership

- One engineer per milestone owns flaky-test remediation.
- Assignment rotates round-robin and is recorded in the milestone planning doc.

## How to choose what to fix during your turn

- Start by searching for existing flaky-test issues created by automation:
  - Filter by labels: `flaky::test`, `flaky::auto`.
  - Prioritize by occurrence buckets: `flaky-occurrences::>25`, `flaky-occurrences::11-25`, `flaky-occurrences::4-10`, `flaky-occurrences::1-3`.
- Pick a candidate to work on:
  - Highest occurrence count first (sort by Weight), then most recently updated. See: [Flaky issues sorted by Weight](https://gitlab.com/gitlab-org/container-registry/-/issues?sort=weight&state=opened&label_name%5B%5D=flaky%3A%3Aauto&label_name%5B%5D=group%3A%3Acontainer%20registry&label_name%5B%5D=failure%3A%3Aflaky-test&first_page_size=20)
  - Prefer unassigned issues.
- Fix or mitigate:
  - Open an MR with the fix and reference the issue.
  - Add notes to the issue about root cause and remediation.
- Wrap-up:
  - Close the issue once fixed; automation will reopen if it flakes again.
  - If not resolved within the milestone, document findings and hand off to the next rotation during the next milestone planning.

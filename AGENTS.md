# Harbor development workflow

Make implementation changes on a `codex/` feature branch. Preserve unrelated local changes and untracked files.

Keep at most one open pull request in this repository by default. Check for existing open pull requests before creating another; finish the current pull request first unless the user explicitly requests parallel pull requests.

Before merging any changes into `main`:

1. Obtain independent reviews from at least two agents covering the complete final diff, including code, tests, configuration, and documentation. Reviewers must challenge assumptions and actively search for correctness bugs, regressions, concurrency and recovery failures, security problems, misleading measurements, and missing failure-path coverage. Passing tests are evidence, not a substitute for review.
2. Address every actionable finding. Record any unresolved finding or accepted limitation explicitly, with its impact and rationale; do not silently dismiss findings.
3. Run the relevant automated checks and realistic integration tests. Match testing to the risk, including cancellation, interruption, retry, and restart when transfer behavior changes.
4. Have independent agents re-review fixes and all subsequent changes. New edits after review require review of those edits before merging.
5. Report review findings, resolutions, validation results, and remaining limitations to the user before merging. Do not merge while a blocking finding remains or a required review/check is pending.

Keep work on the feature branch until this gate is satisfied. A request to commit or push does not by itself authorize bypassing the review gate or merging into `main`.

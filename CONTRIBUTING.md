# Contributing to octo-agent

Thanks for taking the time to contribute. Every PR is reviewed by a human; bots may also leave comments.

## Before you start

- **Read `.octorules` and `CLAUDE.md`** — they cover the layering, conventions, and common pitfalls. Most "is this PR going to land" questions are answered there.
- **Check the roadmap** — `dev-docs/go-rewrite-roadmap.md`. If your change advances a planned milestone, mention it in the PR. If it goes outside the roadmap, the PR description should justify why.
- **Open an issue first for substantial work.** Small fixes go straight to PR; new providers, new tools, or anything touching the agent loop benefit from a short upfront discussion.

## Workflow

1. Fork or branch off the latest `main`. Never commit directly on `main`.
2. One concept per PR. Mass mechanical changes (renames, file moves) can ride together but should be self-contained.
3. Run before pushing:
   ```bash
   make test       # go test -race ./...
   make vet
   make fmt-check
   ```
4. Push and open a PR. Squash-and-merge is the default merge style.
5. Commit messages and PR descriptions in English.

## What we look for

- **Smallest possible diff.** A bug fix shouldn't surround itself with unrelated cleanup. A refactor PR shouldn't bundle a new feature.
- **Tests next to the code.** New behavior gets coverage; bug fixes get a regression test that fails before the fix.
- **No live network in tests.** Use `httptest.NewServer` for HTTP. Real-API smoke tests are run by hand with a personal key, not in CI.
- **No new third-party dependencies without justification.** If you must add one, explain why the stdlib won't do.
- **Comments in English, the *why* not the *what*.** Names should already explain *what*. Only write a comment when removing it would lose information (a non-obvious constraint, a workaround for a known bug, a tradeoff that matters).

## License

By contributing, you agree your code is released under the project's [MIT license](LICENSE.txt).

---
name: product-help
description: Answer questions about how to use octo by reading the product documentation. Use when the user asks "how do I...", "what is...", "where is the docs for...", or any usage/help question about octo itself.
---

# Product Help — answer octo usage questions from documentation

You have access to octo's product documentation. Use it to answer user questions accurately. Do not guess or hallucinate features.

## Documentation sources

1. **Bundled docs** — check the skill directory for reference files:
   - `README.md` — quick start, installation, basic usage
   - `CLI.md` — command reference (`octo`, `octo config`, `octo skills`, etc.)
   - `CONFIG.md` — configuration file format and all supported fields
   - `SKILLS.md` — how to write custom skills
   - `WORKFLOW.md` — development workflow, git conventions, PR process

   Read the relevant file(s) with `read_file` before answering.

2. **Online docs** (fallback) — if the bundled docs don't cover the question, fetch from the official documentation:
   - Base URL: `https://github.com/Leihb/octo-agent/blob/main/`
   - Append the relevant doc filename (e.g. `README.md`, `dev-docs/go-rewrite-roadmap.md`)
   - Use `web_fetch` to retrieve the raw markdown content

## How to answer

1. **Identify the topic.** Map the user's question to the most relevant doc file(s).
2. **Read the source.** Use `read_file` for bundled docs or `web_fetch` for online docs.
3. **Quote selectively.** Cite specific sections or code blocks from the docs to back up your answer.
4. **Be concise.** Give the direct answer first, then optionally link to the full doc for deeper reading.
5. **Say when you don't know.** If neither bundled nor online docs cover the question, say so honestly — don't make up an answer.

## Example interactions

User: "how do I add a custom skill?"
→ Read `SKILLS.md` from the skill directory
→ Answer with the directory layout, frontmatter format, and how to trigger it

User: "what does permission_mode do?"
→ Read `CONFIG.md` from the skill directory
→ Explain the three modes with their behavior

User: "how do I switch to OpenAI?"
→ Read `CONFIG.md` and `CLI.md`
→ Explain `octo config` wizard vs. editing `~/.octo/config.yaml` directly

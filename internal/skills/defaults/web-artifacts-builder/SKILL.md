---
name: web-artifacts-builder
description: Suite of tools for creating elaborate, multi-component single-file HTML web apps using modern frontend technologies (React, Tailwind CSS, shadcn/ui). Use for complex artifacts requiring state management, routing, or shadcn/ui components — not for simple single-file HTML/JSX pages you can write directly.
license: Apache-2.0 (adapted from anthropics/skills; complete terms in LICENSE.txt)
---

# Web Artifacts Builder

To build a powerful frontend artifact — a self-contained single HTML file that opens in any browser — follow these steps:
1. Initialize the frontend repo using `scripts/init-artifact.sh`
2. Develop your artifact by editing the generated code
3. Bundle all code into a single HTML file using `scripts/bundle-artifact.sh`
4. Deliver the artifact to the user
5. (Optional) Test the artifact

**Stack**: React 18 + TypeScript + Vite + Parcel (bundling) + Tailwind CSS + shadcn/ui

**Requirements**: `bash` and Node.js 18+ on PATH. Verify both before starting (`bash --version`, `node -v`). If either is missing, tell the user what to install and wait for their go-ahead — never install unasked:

- Node.js — Windows: `winget install OpenJS.NodeJS.LTS`; macOS: `brew install node`; Linux: distro package manager. **Novice users (or no winget/Homebrew)**: hand them the https://nodejs.org LTS installer link and let them click through the GUI installer with defaults, then verify. Expect an elevation/permission dialog they must approve at the screen.
- bash on Windows — the scripts are bash; Git for Windows provides it (`winget install Git.Git`, or the https://git-scm.com GUI installer). The default install does **not** put `bash` on PATH — invoke it by full path: `& "$env:ProgramFiles\Git\bin\bash.exe" <skill-dir>/scripts/init-artifact.sh <name>`. Without Git Bash or WSL this skill cannot run on Windows — say so plainly; do not attempt to translate the scripts to PowerShell.
- After any mid-session install, the new tool may be missing from this session's PATH — refresh PATH or use full paths per the environment notes; if problems persist, have the user restart octo.

Note: the `scripts/` paths are relative to this skill's directory (its absolute path is in the header above) — invoke them as `bash <skill-dir>/scripts/init-artifact.sh`. Run the project itself in the user's working directory, not inside the skill directory.

## Design & Style Guidelines

VERY IMPORTANT: To avoid what is often referred to as "AI slop", avoid using excessive centered layouts, purple gradients, uniform rounded corners, and Inter font.

## Quick Start

### Step 1: Initialize Project

Run the initialization script to create a new React project:
```bash
bash <skill-dir>/scripts/init-artifact.sh <project-name>
cd <project-name>
```

This creates a fully configured project with:
- ✅ React + TypeScript (via Vite)
- ✅ Tailwind CSS 3.4.1 with shadcn/ui theming system
- ✅ Path aliases (`@/`) configured
- ✅ 40+ shadcn/ui components pre-installed
- ✅ All Radix UI dependencies included
- ✅ Parcel configured for bundling (via .parcelrc)
- ✅ Node 18+ compatibility (auto-detects and pins Vite version)

### Step 2: Develop Your Artifact

To build the artifact, edit the generated files. Remember there is no persistent working directory across terminal calls — chain `cd <project> && …` inside each command, and pass absolute paths to the file tools.

### Step 3: Bundle to Single HTML File

To bundle the React app into a single HTML artifact:
```bash
cd <project-name> && bash <skill-dir>/scripts/bundle-artifact.sh
```

This creates `bundle.html` — a self-contained file with all JavaScript, CSS, and dependencies inlined.

**Requirements**: Your project must have an `index.html` in the root directory.

**What the script does**:
- Installs bundling dependencies (parcel, @parcel/config-default, parcel-resolver-tspaths, html-inline)
- Creates `.parcelrc` config with path alias support
- Builds with Parcel (no source maps)
- Inlines all assets into single HTML using html-inline

### Step 4: Deliver the Artifact

Call the `show_artifact` tool with the absolute path of `bundle.html` — in the web UI this renders it in the Artifacts panel. Then report the path to the user. The file is fully self-contained — it opens directly in any browser with no server and no network.

### Step 5: Testing/Visualizing the Artifact (Optional)

Note: This is a completely optional step. Only perform if necessary or requested.

To test/visualize the artifact, use available tools (a browser-automation skill or MCP tool if one is connected). In general, avoid testing the artifact upfront as it adds latency between the request and when the finished artifact can be seen. Test later, after presenting the artifact, if requested or if issues arise.

## Reference

- **shadcn/ui components**: https://ui.shadcn.com/docs/components

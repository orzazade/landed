---
name: landed
description: List the lasting decisions the user made in this session that have no landing yet (no commit, no edited file, no note). Use when the user types /landed, asks "did my decisions land?", before a session ends, or before work moves to another session.
---

Run this and read its output:

```sh
"${CLAUDE_PLUGIN_ROOT}/scripts/landed-hook" check --session ${CLAUDE_SESSION_ID}
```

Each row is a decision the user made in this session that landed could not find written down or carried out. For each one, say in one line what it decided, then land it where it belongs (the project's AGENTS.md or CLAUDE.md for a rule, a commit for code, the task list for a plan). Ask the user only when it is unclear where it belongs. If the output says "0 with no landing yet", say that every decision landed.

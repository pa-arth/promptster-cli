# .agents — shared agent tooling

`skills/` holds the real content. `.claude/skills/`, `.cursor/skills/` and
`.codex/skills/` are symlinks into it, so Claude Code, Cursor and Codex all read
one source of truth and a fix lands in all three at once.

Add a skill: put it in `.agents/skills/<name>/`, then

```bash
mkdir -p .claude/skills .cursor/skills .codex/skills
for t in claude cursor codex; do
  ln -sfn ../../.agents/skills/<name> .$t/skills/<name>
done
```

## Skills here

- **`verify-cli`** — drive the real promptster binary in a sandbox and capture
  proof. Builds on `scripts/qa-e2e.sh` (the repo's own harness) and the global
  `promptster-qa-e2e` skill rather than replacing them.

Verified: all three symlinks resolve to the same `SKILL.md`. Whether each tool
actually *discovers* a skill at its path was not tested here.

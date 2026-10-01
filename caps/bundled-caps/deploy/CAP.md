---
name: deploy
description: "Run `make deploy` in a yesmem worktree — builds binary, atomic-replaces ~/.local/bin/yesmem, restarts daemon+proxy services. Returns {version: \"vX.Y.Z-N-gHASH\", output} on success, {error, output} on fail. Via execute_cap you MUST pass dir (e.g. {\"dir\": \"/home/<user>/memory/yesmem\"}) — sandbox cwd has no Makefile, then make exits 2. Gotcha: the execute_cap call itself always dies with \"signal: terminated\" — at restart-services the daemon restarts yesmem.service and the systemd cgroup kill takes down its own sandbox. Binary build+install DO apply, but proxy restart, reap-stale-mcp and the final \"deployed vX\" echo are skipped. Verify afterwards: `yesmem version` + systemctl --user status yesmem yesmem-proxy. via REPL sh() the deploy runs as session child and survives the daemon restart."
version: 544
tags: [yesmem, build, deploy, local]
scope: user
tested: true
auto_active: true
---

## Purpose

Run `make deploy` in a yesmem worktree — builds binary, atomic-replaces ~/.local/bin/yesmem, restarts daemon+proxy services. Returns {version: "vX.Y.Z-N-gHASH", output} on success, {error, output} on fail. Via execute_cap you MUST pass dir (e.g. {"dir": "/home/<user>/memory/yesmem"}) — sandbox cwd has no Makefile, then make exits 2. Gotcha: the execute_cap call itself always dies with "signal: terminated" — at restart-services the daemon restarts yesmem.service and the systemd cgroup kill takes down its own sandbox. Binary build+install DO apply, but proxy restart, reap-stale-mcp and the final "deployed vX" echo are skipped. Verify afterwards: `yesmem version` + systemctl --user status yesmem yesmem-proxy. via REPL sh() the deploy runs as session child and survives the daemon restart.

## Scripts

### deploy
kind: tool

```javascript
async ({ dir } = {}) => {
    const cmd = dir ? `cd ${shQuote(dir)} && make deploy 2>&1` : "make deploy 2>&1";
    const output = await sh(cmd, 20000);
    const m = output.match(/deployed\s+(v\S+)\s+→/);
    if (!m)
      return { error: 'deploy output missing "deployed vX.Y.Z" line', output };
    return { version: m[1], output };
  }
```

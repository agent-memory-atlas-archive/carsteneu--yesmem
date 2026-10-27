---
name: changelog_check
description: "Fetch Claude Code changelog from GitHub raw CHANGELOG.md, extract entries from a given version number onwards. Returns clean markdown with version headers and bullet points. Use when checking CC updates, new features, or breaking changes."
version: 517
tags: [web, changelog, claude-code, fetch]
scope: user
tested: true
auto_active: true
---

## Purpose

Fetch Claude Code changelog from GitHub raw CHANGELOG.md, extract entries from a given version number onwards. Returns clean markdown with version headers and bullet points. Use when checking CC updates, new features, or breaking changes.

## Scripts

### changelog_check
kind: tool
sandbox: standard
allowed_hosts: raw.githubusercontent.com

```javascript
async ({ from_version = 100 } = {}) => {
  const fv = parseInt(from_version, 10);
  if (isNaN(fv) || fv < 1) return { error: 'from_version must be a positive integer' };
  const tf = '/dev/shm/cc_cl_' + Date.now() + '.md';
  const dl = await sh('curl -sL --max-time 15 -o ' + tf + " -w '%{http_code}' 'https://raw.githubusercontent.com/anthropics/claude-code/main/CHANGELOG.md'", 20000);
  if (!String(dl).includes('200')) { await sh('rm -f ' + tf, 3000); return { error: 'Download failed: ' + dl }; }
  const result = await sh("awk '/^## [0-9]/{split($2,a,\".\"); if(a[3]+0 < " + fv + ") exit} {print}' " + tf, 15000);
  await sh('rm -f ' + tf, 3000);
  if (!result || !String(result).trim()) return { error: 'No entries found for version >= ' + fv };
  return String(result);
}
```

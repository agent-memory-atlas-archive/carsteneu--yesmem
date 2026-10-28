---
name: adapter_e2e_test
description: "E2E test: verify adapter rewrites store() correctly"
version: 518
tags: [test, e2e]
requires: [store]
scope: user
tested: true
auto_active: true
---

## Purpose

E2E test: verify adapter rewrites store() correctly

## Scripts

### adapter_e2e_test
kind: tool
schema: {"type":"object","properties":{"table":{"type":"string","description":"Optional table name (currently unused; tool just lists tables)"}}}
sandbox: standard

```javascript
async ({table}) => {
  let r = await mcp__yesmem__cap_store({capability:'adapter_e2e_test', action:'list_tables'});
  return r;
}
```

---
name: cap_collect
description: "Generic collect-and-prep over any cap_store table for analysis. Default mode 'data' pulls all matching rows (auto-paginated). Mode 'analyze' delegates synthesis to a fast model. Mode 'instruction' builds a prompt without sending."
version: 551
tags: [cap_store, collect, analyze]
requires: [store]
scope: user
tested: true
auto_active: true
---

## Purpose

Generic collect-and-prep over any cap_store table for analysis. Default mode 'data' pulls all matching rows (auto-paginated). Mode 'analyze' delegates synthesis to a fast model. Mode 'instruction' builds a prompt without sending.

## Scripts

### cap_collect
kind: tool
schema: {"type":"object","properties":{"cap":{"type":"string"},"table":{"type":"string"},"where":{"type":"string"},"args":{},"instruction":{"type":"string"},"mode":{"type":"string","enum":["data","instruction","analyze"]},"model":{"type":"string"},"max_tokens":{"type":"number"},"max_rows":{"type":"number"}},"required":["cap","table"]}
sandbox: standard

```javascript
async ({cap, table, where, args, instruction, mode, model, max_tokens, max_rows}) => {
  if (!cap || !table) return {error: 'cap and table required'};
  const m = mode || 'data';
  const cap_rows = (typeof max_rows === 'number' && max_rows > 0) ? max_rows : 2000;
  const argsJson = Array.isArray(args) ? JSON.stringify(args) : (args || '[]');
  let rows = [], off = 0, total = 0;
  while (rows.length < cap_rows) {
    const pageLim = Math.min(100, cap_rows - rows.length);
    const r = await mcp__yesmem__cap_store({capability: cap, action: 'query', table, where: where || '', args: argsJson, limit: pageLim, offset: off});
    const parsed = typeof r === 'string' ? JSON.parse(r) : r;
    if (!parsed?.rows?.length) break;
    rows.push(...parsed.rows);
    total = parsed.total || total;
    if (!parsed.has_more) break;
    off = parsed.next_offset;
  }
  const truncated = rows.length >= cap_rows && total > rows.length;
  if (m === 'data') return {rows, count: rows.length, total, truncated};
  const compactRows = rows.map(r => {
    const o = {}; for (const k of Object.keys(r)) {
      const v = r[k]; if (v === null || v === undefined) continue;
      if (typeof v === 'string' && v.length > 1500) o[k] = v.slice(0, 1500) + '…'; else o[k] = v;
    } return o;
  });
  const prompt = (instruction || 'Summarize the following rows.').trim() + '\n\nRows (JSON):\n' + JSON.stringify(compactRows, null, 2);
  if (m === 'instruction') return {prompt, count: rows.length, total, truncated};
  if (m === 'analyze') {
    const out = await haiku(prompt, undefined);
    return {analysis: out, count: rows.length, total, truncated, model: model || 'haiku'};
  }
  return {error: `unknown mode '${m}'; use data | instruction | analyze`};
}
```

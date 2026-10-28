---
name: cap_search
description: "Generic search over any cap_store table. Wraps cap_store query with sane defaults + optional auto-pagination. Returns {error} when daemon response exceeds MCP token limit."
version: 549
tags: [cap_store, query, reusable]
requires: [store]
scope: user
tested: true
auto_active: true
---

## Purpose

Generic search over any cap_store table. Wraps cap_store query with sane defaults + optional auto-pagination. Returns {error} when daemon response exceeds MCP token limit.

## Scripts

### cap_search
kind: tool
schema: {"type":"object","properties":{"cap":{"type":"string"},"table":{"type":"string"},"where":{"type":"string"},"args":{},"limit":{"type":"number"},"offset":{"type":"number"},"all":{"type":"boolean"},"max_rows":{"type":"number"}},"required":["cap","table"]}
sandbox: standard

```javascript
async ({cap, table, where, args, limit, offset, all, max_rows}) => {
  if (!cap || !table) return {error: 'cap and table required'};
  const lim = (typeof limit === 'number' && limit > 0) ? limit : 100;
  const argsJson = Array.isArray(args) ? JSON.stringify(args) : (args || '[]');
  const callOnce = async (l, o) => {
    const r = await mcp__yesmem__cap_store({capability: cap, action: 'query', table, where: where || '', args: argsJson, limit: l, offset: o});
    if (typeof r === 'string') return {error: r.slice(0, 500)};
    try { return typeof r === 'object' ? r : JSON.parse(r); }
    catch (e) { return {error: 'parse failed: ' + String(e).slice(0,200)}; }
  };
  if (!all) return await callOnce(lim, offset || 0);
  const cap_rows = (typeof max_rows === 'number' && max_rows > 0) ? max_rows : 2000;
  let rows = [], off = 0, total = 0;
  while (rows.length < cap_rows) {
    const pageLim = Math.min(100, cap_rows - rows.length);
    const parsed = await callOnce(pageLim, off);
    if (parsed.error) return {error: parsed.error, rows, count: rows.length, total, partial: true};
    if (!parsed?.rows?.length) break;
    rows.push(...parsed.rows);
    total = parsed.total || total;
    if (!parsed.has_more) break;
    off = parsed.next_offset;
  }
  return {rows, count: rows.length, total, truncated: rows.length >= cap_rows && total > rows.length};
}
```

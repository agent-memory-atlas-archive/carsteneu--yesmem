---
name: cap_save_analysis
description: "Append-only persist of an analysis result to cap_<cap>__analyses table (auto-created on first call). Records source-meta (table, where, args, row_count), the instruction asked, the summary text, the model that produced it, optional comma-separated tags. cap_store auto-adds id (PK), created_at (DATETIME), updated_at. Used after cap_collect→in-chat-summary OR cap_collect mode='haiku' to keep a queryable history of analyses. Later retrieval via cap_search({cap, table:'analyses', where:...}) lets you find/aggregate past analyses by topic, time, model, or source-filter — and recursively cap_collect over the analyses themselves for meta-analysis."
version: 97
tags: [cap_store, analysis, persist, append-only, history]
requires: [store]
scope: user
tested: true
auto_active: true
---

## Purpose

Append-only persist of an analysis result to cap_<cap>__analyses table (auto-created on first call). Records source-meta (table, where, args, row_count), the instruction asked, the summary text, the model that produced it, optional comma-separated tags. cap_store auto-adds id (PK), created_at (DATETIME), updated_at. Used after cap_collect→in-chat-summary OR cap_collect mode='haiku' to keep a queryable history of analyses. Later retrieval via cap_search({cap, table:'analyses', where:...}) lets you find/aggregate past analyses by topic, time, model, or source-filter — and recursively cap_collect over the analyses themselves for meta-analysis.

## Scripts

### cap_save_analysis
kind: tool
schema: {"type":"object","properties":{"cap":{"type":"string","description":"Source capability name (e.g. 'reddit')"},"source_table":{"type":"string","description":"Table that was analyzed (e.g. 'posts')"},"filter_where":{"type":"string","description":"WHERE clause used to scope the source rows"},"filter_args":{"description":"Args bound to filter_where (array or scalar)"},"instruction":{"type":"string","description":"What was asked of the model"},"summary":{"type":"string","description":"The analysis result text"},"row_count":{"type":"integer","description":"Number of source rows analyzed"},"model":{"type":"string","description":"Model that produced the summary (default claude-opus-4-7)"},"tags":{"type":"string","description":"Comma-separated tags for later filtering"}},"required":["cap","source_table","instruction","summary"]}
sandbox: standard

```javascript
async ({cap, source_table, filter_where, filter_args, instruction, summary, row_count, model, tags}) => {
  if (!cap || !source_table || !instruction || !summary) return {error: 'cap, source_table, instruction, summary required'};
  const createRes = await mcp__yesmem__cap_store({capability: cap, action: 'create_table', table: 'analyses', columns: JSON.stringify([
    {name:'source_table',type:'TEXT'},
    {name:'filter_where',type:'TEXT'},
    {name:'filter_args',type:'TEXT'},
    {name:'instruction',type:'TEXT'},
    {name:'summary',type:'TEXT'},
    {name:'row_count',type:'INTEGER'},
    {name:'model',type:'TEXT'},
    {name:'tags',type:'TEXT'}
  ])});
  if (typeof createRes === 'string' && createRes.startsWith('Error:') && !createRes.includes('already exists')) {
    return {error: 'create_table failed', detail: createRes};
  }
  const fa = filter_args === undefined || filter_args === null ? '' : (Array.isArray(filter_args) ? JSON.stringify(filter_args) : String(filter_args));
  const row = {
    source_table,
    filter_where: filter_where || '',
    filter_args: fa,
    instruction,
    summary,
    row_count: typeof row_count === 'number' ? row_count : 0,
    model: model || 'claude-opus-4-7',
    tags: tags || ''
  };
  const r = await mcp__yesmem__cap_store({capability: cap, action: 'upsert', table: 'analyses', data: JSON.stringify(row)});
  if (typeof r === 'string' && r.startsWith('Error:')) {
    return {error: 'upsert failed', detail: r};
  }
  const parsed = typeof r === 'string' ? JSON.parse(r) : r;
  return {id: parsed?.id, cap, table: 'analyses', model: row.model, row_count: row.row_count};
}
```

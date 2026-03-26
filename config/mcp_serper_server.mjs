#!/usr/bin/env node
/**
 * Serper API MCP Server (Node.js)
 * Provides web search via Serper API (Google Search)
 * Protocol: MCP over stdio (JSON-RPC 2.0)
 */

import { createInterface } from 'readline';

const SERPER_API_KEY = process.env.SERPER_API_KEY || '';

const TOOL_DEF = {
  name: 'web_search',
  description: '使用 Serper API 進行網路搜尋，返回 Google 搜尋結果。支援多種搜尋類型（網頁、圖片、新聞等）',
  inputSchema: {
    type: 'object',
    properties: {
      q: { type: 'string', description: '搜尋查詢字串' },
      num: { type: 'integer', description: '返回結果數量（預設 10）', default: 10 },
      type: { type: 'string', description: '搜尋類型：search（網頁）、images（圖片）、news（新聞）', enum: ['search', 'images', 'news'], default: 'search' },
      lang: { type: 'string', description: '搜尋語言（如：zh-tw, en, ja）', default: 'zh-tw' }
    },
    required: ['q']
  }
};

async function doSearch(args) {
  if (!SERPER_API_KEY) {
    return '錯誤：未設置 SERPER_API_KEY 環境變數。';
  }
  const query = args.q || '';
  const num = args.num || 10;
  const searchType = args.type || 'search';
  const lang = args.lang || 'zh-tw';
  if (!query) return '錯誤：查詢字串不能為空';

  const urls = { search: 'https://google.serper.dev/search', images: 'https://google.serper.dev/images', news: 'https://google.serper.dev/news' };
  const url = urls[searchType] || urls.search;

  const res = await fetch(url, {
    method: 'POST',
    headers: { 'X-API-KEY': SERPER_API_KEY, 'Content-Type': 'application/json' },
    body: JSON.stringify({ q: query, num, gl: lang.split('-')[0], hl: lang })
  });
  if (!res.ok) throw new Error(`Serper API ${res.status}: ${await res.text()}`);
  const data = await res.json();

  let text = `搜尋查詢：${query}\n找到約 ${data.searchInformation?.totalResults || 'N/A'} 筆結果\n\n`;
  if (searchType === 'search') {
    (data.organic || []).slice(0, num).forEach((r, i) => {
      text += `${i + 1}. ${r.title || 'N/A'}\n   連結：${r.link || 'N/A'}\n   摘要：${r.snippet || 'N/A'}\n\n`;
    });
  } else if (searchType === 'images') {
    (data.images || []).slice(0, num).forEach((r, i) => {
      text += `${i + 1}. ${r.title || 'N/A'}\n   圖片：${r.imageUrl || 'N/A'}\n   來源：${r.link || 'N/A'}\n\n`;
    });
  } else if (searchType === 'news') {
    (data.news || []).slice(0, num).forEach((r, i) => {
      text += `${i + 1}. ${r.title || 'N/A'}\n   來源：${r.source || 'N/A'}\n   連結：${r.link || 'N/A'}\n   時間：${r.date || 'N/A'}\n\n`;
    });
  }
  return text;
}

// --- MCP JSON-RPC over stdio ---
const rl = createInterface({ input: process.stdin, terminal: false });

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

rl.on('line', async (line) => {
  let req;
  try { req = JSON.parse(line); } catch { return; }

  const { id, method, params } = req;

  if (method === 'initialize') {
    send({ jsonrpc: '2.0', id, result: { protocolVersion: '2024-11-05', capabilities: { tools: {} }, serverInfo: { name: 'serper-search', version: '1.0.0' } } });
  } else if (method === 'notifications/initialized') {
    // no response needed
  } else if (method === 'tools/list') {
    send({ jsonrpc: '2.0', id, result: { tools: [TOOL_DEF] } });
  } else if (method === 'tools/call') {
    try {
      const text = await doSearch(params?.arguments || {});
      send({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } });
    } catch (e) {
      send({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: `搜尋失敗: ${e.message}` }], isError: true } });
    }
  } else if (id !== undefined) {
    send({ jsonrpc: '2.0', id, error: { code: -32601, message: `Method not found: ${method}` } });
  }
});

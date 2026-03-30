#!/usr/bin/env node
/**
 * Plugin Forge MCP Server
 * Provides plugin_forge tool for Main Agent to delegate plugin creation to Sub-Agent
 * Protocol: MCP over stdio (JSON-RPC 2.0)
 */

import { createInterface } from 'readline';

const MIRROR_SERVICE_URL = process.env.MIRROR_INTERNAL_URL || 'http://localhost:8080';

const TOOL_DEF = {
  name: 'plugin_forge',
  description: '啟動插件鍛造 Sub-Agent 建立新插件。當用戶要求新增自定義功能（如番茄鐘、看板、自定義面板等）時使用此工具。Sub-Agent 會讀取內建插件原始碼作為參考，自動建立、編譯並註冊新插件。',
  inputSchema: {
    type: 'object',
    properties: {
      title: { type: 'string', description: '插件名稱（如「番茄鐘插件」）' },
      prompt: { type: 'string', description: '用戶的完整需求描述' },
    },
    required: ['title', 'prompt']
  }
};

async function forgePlugin(args) {
  const { title, prompt } = args;
  if (!title || !prompt) return '錯誤：title 和 prompt 為必填';

  // 從環境變數取得當前 session 資訊
  const memberID = process.env.VAULT_USER_ID || '';
  const wsSessionID = process.env.WS_SESSION_ID || '';

  const res = await fetch(`${MIRROR_SERVICE_URL}/api/internal/forge`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title, prompt, memberID, wsSessionID }),
  });

  if (!res.ok) {
    const errText = await res.text();
    return `插件鍛造失敗 (HTTP ${res.status}): ${errText}`;
  }

  const result = await res.json();
  if (result.status === 'success') {
    return `插件「${result.title}」已建立並編譯成功。\n插件目錄：plugins/${result.pluginDir}\nbundleHash：${result.bundleHash}\n用戶刷新後即可使用。`;
  } else {
    return `插件鍛造失敗：${result.error || '未知錯誤'}`;
  }
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
    send({ jsonrpc: '2.0', id, result: { protocolVersion: '2024-11-05', capabilities: { tools: {} }, serverInfo: { name: 'plugin-forge', version: '1.0.0' } } });
  } else if (method === 'notifications/initialized') {
    // no response needed
  } else if (method === 'tools/list') {
    send({ jsonrpc: '2.0', id, result: { tools: [TOOL_DEF] } });
  } else if (method === 'tools/call') {
    try {
      const text = await forgePlugin(params?.arguments || {});
      send({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } });
    } catch (e) {
      send({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: `插件鍛造失敗: ${e.message}` }], isError: true } });
    }
  } else if (id !== undefined) {
    send({ jsonrpc: '2.0', id, error: { code: -32601, message: `Method not found: ${method}` } });
  }
});

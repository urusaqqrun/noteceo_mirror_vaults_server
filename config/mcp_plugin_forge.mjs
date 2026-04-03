#!/usr/bin/env node
/**
 * Plugin Forge MCP Server
 * Provides plugin_forge tool for Main Agent to delegate plugin creation to Sub-Agent
 * Protocol: MCP over stdio (JSON-RPC 2.0)
 */

import { createInterface } from 'readline';
import { readFileSync } from 'fs';
import { join } from 'path';

if (!process.env.MIRROR_INTERNAL_URL) {
  process.stderr.write('[plugin-forge] MIRROR_INTERNAL_URL 未設定\n');
  process.exit(1);
}
const MIRROR_SERVICE_URL = process.env.MIRROR_INTERNAL_URL;

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

function loadScopedSessionBinding() {
  const bindingPath = join(process.cwd(), `.ws_session_id.${process.ppid}.json`);
  try {
    const parsed = JSON.parse(readFileSync(bindingPath, 'utf8'));
    return typeof parsed?.sessionID === 'string' ? parsed.sessionID.trim() : '';
  } catch {
    return '';
  }
}

async function forgePlugin(args) {
  const { title, prompt } = args;
  if (!title || !prompt) return '錯誤：title 和 prompt 為必填';

  const memberID = process.env.VAULT_USER_ID || '';
  let wsSessionID = process.env.WS_SESSION_ID || '';
  if (!wsSessionID) {
    wsSessionID = loadScopedSessionBinding();
  }
  if (!memberID || !wsSessionID) return '錯誤：缺少有效的 forge session 綁定';

  // forge 禁止 timeout — 用 http.request 手動發 POST，socket timeout 設為 0（永不超時）
  const http = await import('http');
  const url = new URL(`${MIRROR_SERVICE_URL}/api/internal/forge`);
  const postData = JSON.stringify({ title, prompt, memberID, wsSessionID });

  const result = await new Promise((resolve, reject) => {
    const req = http.request({
      hostname: url.hostname,
      port: url.port,
      path: url.pathname,
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(postData), 'X-Internal-Secret': process.env.INTERNAL_SECRET || '' },
      timeout: 0,  // 禁止 timeout
    }, (res) => {
      let body = '';
      res.on('data', (chunk) => body += chunk);
      res.on('end', () => {
        if (res.statusCode !== 200) {
          resolve(`插件鍛造失敗 (HTTP ${res.statusCode}): ${body}`);
          return;
        }
        try {
          const json = JSON.parse(body);
          if (json.status === 'success') {
            resolve(`插件「${json.title}」已建立並編譯成功。\n插件目錄：plugins/${json.pluginDir}\nbundleHash：${json.bundleHash}\n用戶刷新後即可使用。`);
          } else if (json.status === 'cancelled') {
            resolve('插件鍛造已中斷。');
          } else {
            resolve(`插件鍛造失敗：${json.error || '未知錯誤'}`);
          }
        } catch (e) {
          resolve(`插件鍛造回應解析失敗: ${body.substring(0, 200)}`);
        }
      });
    });
    req.on('error', (e) => resolve(`插件鍛造連線失敗: ${e.message}`));
    req.setTimeout(0);  // 確保 socket 也不 timeout
    req.write(postData);
    req.end();
  });

  return result;
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

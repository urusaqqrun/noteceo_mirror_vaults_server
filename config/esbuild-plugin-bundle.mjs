#!/usr/bin/env node
// esbuild wrapper: 把所有非相對路徑的 import 都標記 external
// 這樣 AI 不管 import 什麼（react, lodash, d3, ...），都不會讓 esbuild 報錯
// 前端的 require shim 會負責從 window.__NC__ 解析

import { build } from 'esbuild';

const [entry, outfile] = process.argv.slice(2);
if (!entry || !outfile) {
    console.error('Usage: node esbuild-plugin-bundle.mjs <entry> <outfile>');
    process.exit(1);
}

const externalAllBareImports = {
    name: 'external-bare-imports',
    setup(b) {
        // 非 . 或 / 開頭的 import 都視為 external（bare specifier）
        b.onResolve({ filter: /^[^./]/ }, (args) => ({
            path: args.path,
            external: true,
        }));
    },
};

try {
    await build({
        entryPoints: [entry],
        bundle: true,
        format: 'iife',
        globalName: '__plugin__',
        jsx: 'automatic',
        loader: { '.tsx': 'tsx', '.ts': 'ts', '.css': 'css' },
        outfile,
        plugins: [externalAllBareImports],
    });
} catch (err) {
    console.error(err.message);
    process.exit(1);
}

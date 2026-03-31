#!/usr/bin/env node
// esbuild wrapper: react/react-dom/@cubelv/sdk 無條件 external（singleton），
// 其餘 import 嘗試 resolve → 有就 bundle → 沒有就 fallback external（runtime Proxy 兜底）

import { build } from 'esbuild';

const [entry, outfile] = process.argv.slice(2);
if (!entry || !outfile) {
    console.error('Usage: node esbuild-plugin-bundle.mjs <entry> <outfile>');
    process.exit(1);
}

// 只有 React 家族和 CubeLV SDK 必須與宿主共享（singleton 需求）
const ALWAYS_EXTERNAL = /^(react(\/.*)?|react-dom(\/.*)?|@cubelv\/sdk)$/;

// AI 子代理可能照抄內建插件的相對路徑 import，自動 rewrite 成 @cubelv/sdk
const SDK_PATH_PATTERNS = [
    /^\.\.\/.*\/workspace\//,
    /^\.\.\/.*\/Store\//,
    /^\.\.\/.*\/types\//,
    /^\.\.\/.*\/i18n\//,
    /^\.\.\/.*\/components\/common\//,
    /^\.\.\/.*\/components\/setting\//,
    /^\.\.\/.*\/components\/plugins\//,
];

const rewriteToSdk = {
    name: 'rewrite-relative-to-sdk',
    setup(b) {
        b.onResolve({ filter: /^\.\./ }, (args) => {
            if (SDK_PATH_PATTERNS.some(re => re.test(args.path))) {
                return { path: '@cubelv/sdk', external: true };
            }
            return undefined;
        });
    },
};

// 自動判定 external：react/@cubelv/sdk 無條件 external，
// 其餘嘗試正常 resolve，找不到才 fallback external
const smartExternal = {
    name: 'smart-external',
    setup(b) {
        b.onResolve({ filter: /^[^./]/ }, async (args) => {
            if (ALWAYS_EXTERNAL.test(args.path)) {
                return { path: args.path, external: true };
            }
            const result = await b.resolve(args.path, {
                resolveDir: args.resolveDir,
                kind: args.kind,
            });
            if (result.errors.length > 0) {
                return { path: args.path, external: true };
            }
            return result;
        });
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
        plugins: [rewriteToSdk, smartExternal],
    });
} catch (err) {
    console.error(err.message);
    process.exit(1);
}

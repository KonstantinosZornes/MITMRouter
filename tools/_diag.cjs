const { parse } = require('../web/node_modules/.pnpm/@vue+compiler-sfc@3.5.41/node_modules/@vue/compiler-sfc/dist/compiler-sfc.cjs.js');
const fs = require('fs');

let tpl = fs.readFileSync('src/App.vue', 'utf8');
tpl = tpl.slice(tpl.indexOf('<template>') + '<template>'.length, tpl.lastIndexOf('</template>'));

function errs(t) {
  const r = parse(`<template>${t}</template>`, { filename: 'x.vue' });
  return r.errors;
}
console.log('原始错误数:', errs(tpl).length);

// 候选删除块（正则匹配完整块）
const blocks = [
  ['lang-dropdown', /<el-dropdown @command="switchLang">[\s\S]*?<\/el-dropdown>/],
  ['user-dropdown', /<el-dropdown>\s*<span class="user-chip">[\s\S]*?<\/el-dropdown>/],
  ['globe-svg-line', /<el-icon><svg[\s\S]*?<\/svg><\/el-icon>/],
];
for (const [name, re] of blocks) {
  const m = tpl.match(re);
  if (!m) { console.log(name, ': 块未匹配'); continue; }
  const stripped = tpl.replace(m[0], '');
  console.log(`去掉 ${name} 后错误数:`, errs(stripped).length);
}

// 再试：把 globe svg 换成简单圆
const simplified = tpl.replace(/<svg[^>]*>[\s\S]*?<\/svg>/,
  '<svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor"><circle cx="12" cy="12" r="10"/></svg>');
console.log('简化svg后错误数:', errs(simplified).length);

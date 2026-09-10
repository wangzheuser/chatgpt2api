const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const ts = require("../web/node_modules/typescript");
const root = path.resolve(__dirname, "..");

/** 从真实源码提取纯函数，避免加载页面和浏览器依赖。 */
function functions(file, names) {
  const source = fs.readFileSync(path.join(root, file), "utf8");
  const tree = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const selected = tree.statements.filter(node => ts.isFunctionDeclaration(node) && names.includes(node.name?.text));
  assert.equal(selected.length, names.length);
  const code = ts.transpileModule(selected.map(node => node.getText(tree)).join("\n"), {}).outputText;
  return vm.runInNewContext(code + ";({" + names.join(",") + "})");
}

const { normalizeStoredImageModel } = functions("web/src/app/image/page.tsx", ["normalizeStoredImageModel"]);
const available = ["gpt-image-2", "gpt-image-2-5", "codex-gpt-image-2"];
assert.equal(normalizeStoredImageModel(null, available), "gpt-image-2-5");
assert.equal(normalizeStoredImageModel(null, []), "gpt-image-2-5");
for (const model of available) assert.equal(normalizeStoredImageModel(model, available), model);

const { normalizeConversation } = functions("web/src/store/image-conversations.ts", [
  "normalizeStoredImage", "normalizeReferenceImage", "dataUrlMimeType", "getLegacyReferenceImages",
  "normalizeTurn", "normalizeConversation",
]);
for (const model of [undefined, ...available]) {
  const expected = model || "gpt-image-2-5";
  assert.equal(normalizeConversation({ model }).turns[0].model, expected);
  assert.equal(normalizeConversation({ turns: [{ model }] }).turns[0].model, expected);
}
console.log("FRONTEND_MODEL_DEFAULTS_PASS");

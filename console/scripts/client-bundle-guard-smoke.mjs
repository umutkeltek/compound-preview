import { readdir, readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { builtinModules } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

/**
 * Client-bundle guard for the console (regression 9c75d29). A `'use client'`
 * component that VALUE-imports a module whose static import graph transitively
 * reaches a `node:` builtin drags server-only code into the client bundle and
 * breaks the build. The original break: `DecideQueue` value-imported
 * `genericReviewResolutionActs` from `@/lib/decide-model`, whose graph reaches
 * `@/lib/compound` -> `node:child_process`; the fix moved the value into
 * `@/lib/decide-resolution-acts` and left only an `import type` of `decide-model`.
 *
 * No ESLint is configured in this console yet, so this deterministic
 * TypeScript-AST scan is the enforcement. It joins the `compound proof-check`
 * console gate list (cmd/compound/proof_check.go).
 *
 * THE LAW: starting from every `'use client'` entry under the scan root, no
 * runtime module edge may reach a Node builtin or server capability material.
 * Static imports/re-exports, dynamic import(), and local require() are runtime
 * edges; whole-statement and specifier-only type imports/exports are not.
 *
 * Usage: node --experimental-strip-types client-bundle-guard-smoke.mjs [scanRoot]
 * scanRoot defaults to console/src; the `@/` alias resolves to scanRoot.
 */

const consoleRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const scanRoot = path.resolve(process.argv[2] ?? path.join(consoleRoot, "src"));

const EXTENSIONS = [".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"];
const INDEX_FILES = EXTENSIONS.map((extension) => `index${extension}`);
const CAPABILITY_MATERIAL_RE = /COMPOUND_CONSOLE_CAPABILITY|x-compound-console-capability/;
const NODE_BUILTINS = new Set(builtinModules.map((name) => name.replace(/^node:/, "")));

async function sourceFiles(dir) {
  const entries = await readdir(dir, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) files.push(...(await sourceFiles(full)));
    else if (entry.isFile() && EXTENSIONS.includes(path.extname(entry.name))) files.push(full);
  }
  return files.sort();
}

function scriptKind(file) {
  switch (path.extname(file)) {
    case ".ts":
      return ts.ScriptKind.TS;
    case ".tsx":
      return ts.ScriptKind.TSX;
    case ".jsx":
      return ts.ScriptKind.JSX;
    default:
      return ts.ScriptKind.JS;
  }
}

function runtimeImportClause(clause) {
  if (!clause) return true;
  if (clause.isTypeOnly) return false;
  if (clause.name) return true;
  const bindings = clause.namedBindings;
  if (!bindings || ts.isNamespaceImport(bindings)) return true;
  return bindings.elements.length === 0 || bindings.elements.some((element) => !element.isTypeOnly);
}

function runtimeExportClause(node) {
  if (node.isTypeOnly) return false;
  if (!node.exportClause || !ts.isNamedExports(node.exportClause)) return true;
  return node.exportClause.elements.length === 0 || node.exportClause.elements.some((element) => !element.isTypeOnly);
}

function parseModule(file, source) {
  const parsed = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, scriptKind(file));
  const found = [];
  const addLiteral = (node) => {
    if (node && ts.isStringLiteralLike(node)) found.push(node.text);
  };
  const visit = (node) => {
    if (ts.isImportDeclaration(node)) {
      if (runtimeImportClause(node.importClause)) addLiteral(node.moduleSpecifier);
    } else if (ts.isExportDeclaration(node)) {
      if (runtimeExportClause(node)) addLiteral(node.moduleSpecifier);
    } else if (ts.isImportEqualsDeclaration(node) && !node.isTypeOnly) {
      if (ts.isExternalModuleReference(node.moduleReference)) addLiteral(node.moduleReference.expression);
    } else if (ts.isCallExpression(node) && node.arguments.length === 1) {
      if (node.expression.kind === ts.SyntaxKind.ImportKeyword) addLiteral(node.arguments[0]);
      if (ts.isIdentifier(node.expression) && node.expression.text === "require") addLiteral(node.arguments[0]);
    }
    ts.forEachChild(node, visit);
  };
  visit(parsed);
  return { parsed, specifiers: [...new Set(found)] };
}

function hasUseClientDirective(parsed) {
  for (const statement of parsed.statements) {
    if (!ts.isExpressionStatement(statement) || !ts.isStringLiteralLike(statement.expression)) return false;
    if (statement.expression.text === "use client") return true;
  }
  return false;
}

function carriesCapabilityMaterial(parsed) {
  let found = false;
  const visit = (node) => {
    if (found) return;
    if ((ts.isIdentifier(node) || ts.isStringLiteralLike(node)) && CAPABILITY_MATERIAL_RE.test(node.text)) {
      found = true;
      return;
    }
    ts.forEachChild(node, visit);
  };
  visit(parsed);
  return found;
}

function isNodeBuiltin(specifier) {
  if (specifier.startsWith("node:")) return true;
  return NODE_BUILTINS.has(specifier) || NODE_BUILTINS.has(specifier.split("/")[0]);
}

/** Resolve a local specifier to a source file, or null for external/unresolved. */
function resolveLocal(fromFile, specifier) {
  let base;
  if (specifier.startsWith("@/")) base = path.join(scanRoot, specifier.slice(2));
  else if (specifier.startsWith(".")) base = path.resolve(path.dirname(fromFile), specifier);
  else return null; // bare package specifier (react, next/…, @radix-ui/…)
  if (EXTENSIONS.includes(path.extname(base)) && existsSync(base)) return base;
  for (const extension of EXTENSIONS) {
    if (existsSync(base + extension)) return base + extension;
  }
  for (const index of INDEX_FILES) {
    const candidate = path.join(base, index);
    if (existsSync(candidate)) return candidate;
  }
  return null;
}

const moduleCache = new Map();
async function moduleOf(file) {
  if (!moduleCache.has(file)) moduleCache.set(file, parseModule(file, await readFile(file, "utf8")));
  return moduleCache.get(file);
}

/**
 * DFS from one client entry; returns the first value chain reaching a
 * server-only boundary. Capability identifiers are checked in every reached
 * module, not only the file carrying the `use client` directive.
 */
async function firstServerBoundaryReach(entry) {
  const visited = new Set();
  async function walk(file, trail) {
    if (visited.has(file)) return null;
    visited.add(file);
    const module = await moduleOf(file);
    if (carriesCapabilityMaterial(module.parsed)) {
      return [...trail, "server capability material"];
    }
    for (const specifier of module.specifiers) {
      if (isNodeBuiltin(specifier)) {
        return [...trail, specifier];
      }
      const local = resolveLocal(file, specifier);
      if (!local) continue;
      const hit = await walk(local, [...trail, path.relative(scanRoot, local).split(path.sep).join("/")]);
      if (hit) return hit;
    }
    return null;
  }
  return walk(entry, [path.relative(scanRoot, entry).split(path.sep).join("/")]);
}

const violations = [];
for (const file of await sourceFiles(scanRoot)) {
  const module = await moduleOf(file);
  if (!hasUseClientDirective(module.parsed)) continue;
  const chain = await firstServerBoundaryReach(file);
  if (chain) violations.push(chain.join(" -> "));
}

if (violations.length > 0) {
  console.error(`client-bundle guard FAILED: ${violations.length} client entr${violations.length === 1 ? "y" : "ies"} reach a server-only boundary:`);
  for (const chain of violations) console.error(`  ${chain}`);
  console.error("fix: move server values and capability material behind a server-only boundary; keep client imports type-only or client-safe.");
  process.exit(1);
}

// The default proof-check run also proves the guard itself against a synthetic
// transitive leak. An explicit scanRoot is the child/focused path and must not
// recurse into this falsifier again.
if (process.argv[2] === undefined) {
  const fixtureCases = [
    {
      directory: "client-capability-regression",
      expected: /entry-client\.tsx -> capability-bridge\.ts -> server capability material/,
    },
    {
      directory: "client-dynamic-regression",
      expected: /entry-client\.jsx -> capability-bridge\.mjs -> server capability material/,
    },
    {
      directory: "client-require-regression",
      expected: /entry-client\.cjs -> builtin-bridge\.js -> (?:node:)?fs/,
    },
    {
      directory: "client-reexport-regression",
      expected: /entry-client\.tsx -> barrel\.js -> capability-bridge\.mjs -> server capability material/,
    },
  ];
  for (const fixtureCase of fixtureCases) {
    const fixtureRoot = path.join(consoleRoot, "scripts", "fixtures", fixtureCase.directory);
    const fixture = spawnSync(
      process.execPath,
      ["--experimental-strip-types", fileURLToPath(import.meta.url), fixtureRoot],
      { encoding: "utf8" },
    );
    if (fixture.status === 0) {
      console.error(`client-bundle guard self-test FAILED: ${fixtureCase.directory} was accepted`);
      process.exit(1);
    }
    if (!fixtureCase.expected.test(fixture.stderr)) {
      console.error(`client-bundle guard self-test FAILED: ${fixtureCase.directory} failed for the wrong reason`);
      process.exit(1);
    }
  }

  const safeFixtureRoot = path.join(consoleRoot, "scripts", "fixtures", "client-type-only-safe");
  const safeFixture = spawnSync(
    process.execPath,
    ["--experimental-strip-types", fileURLToPath(import.meta.url), safeFixtureRoot],
    { encoding: "utf8" },
  );
  if (safeFixture.status !== 0) {
    console.error("client-bundle guard self-test FAILED: type-only imports or comments were treated as value edges");
    process.exit(1);
  }
}

console.log(`client-bundle guard ok: no 'use client' entry under ${path.relative(consoleRoot, scanRoot) || "."} reaches a node: builtin or server capability material`);

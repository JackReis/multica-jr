#!/usr/bin/env node
/**
 * FAIL-closed Dune import graph for multica-jr.
 *
 * Doctrine skill: dune-electron-doctrine.
 * Vault: Architecture/fleet/DUNE-ELECTRON-VAULT-DOCTRINE-20260927.md §3.
 * Card: AEGI-178 (01a0e545-edc9-793d-be5f-2b1258ad9449).
 * Baton: 01a0e5b2-e7d3-7dda-9c88-0c348a62797a.
 *
 * Four edges, adapted to this repo's planes (web UI / shared views vs Electron
 * main+preload vs the Go server and daemon). Multica's issue API client in
 * packages/core is the one assignment plane. Electron and UI must not grow a
 * second board client.
 *
 *   unprivileged-to-privileged  UI, shared protocol, or core imports main,
 *                               preload, or server/
 *   shortcut-ipc                renderer/shared imports electron or a Node
 *                               builtin (skipping the preload bridge), or an
 *                               Electron zone imports a different zone
 *   secret-reader-in-ui         unprivileged code imports a daemon token
 *                               reader or a keychain/safeStorage module
 *   parallel-board-client       Electron or UI calls fetch (or a sibling HTTP
 *                               client) with an /api/issues URL
 *
 * Missing policy, missing exception ledger, a policy that drops an edge, an
 * unreadable or unparsable source file, an unresolved relative import, a
 * vanished secret-reader or sanctioned board client, or an unused stamp all
 * fail the process. A banned edge fails unless the ledger stamps that exact
 * file, edge, and specifier.
 *
 * Edges come from TypeScript syntax nodes, not a regex over source text, so a
 * commented-out import is not a live edge.
 *
 * Run: node scripts/dune-import-bans.mjs
 */

import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { builtinModules } from "node:module";
import { dirname, join, relative, resolve, sep } from "node:path";
import { pathToFileURL } from "node:url";
import ts from "typescript";

const EDGES = [
  "unprivileged-to-privileged",
  "shortcut-ipc",
  "secret-reader-in-ui",
  "parallel-board-client",
];

const SOURCE_EXTENSIONS = [".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"];
const SKIP_DIRS = new Set([
  "node_modules",
  ".git",
  ".next",
  ".turbo",
  "out",
  "dist",
  "build",
  "coverage",
]);
const ASSET_EXTENSIONS = new Set([
  ".css",
  ".scss",
  ".svg",
  ".png",
  ".jpg",
  ".jpeg",
  ".gif",
  ".webp",
  ".avif",
  ".ico",
  ".json",
  ".md",
  ".mdx",
  ".wasm",
  ".txt",
  ".node",
]);
const HTTP_LIBS = new Set(["axios", "ky", "got"]);
const HTTP_METHODS = new Set(["get", "post", "put", "patch", "delete", "request", "fetch"]);

const repoRoot = resolve(import.meta.dirname, "..");

function toPosix(path) {
  return path.split(sep).join("/");
}

function repoRelative(absPath) {
  return toPosix(relative(repoRoot, absPath));
}

function hasPrefix(posixPath, prefix) {
  return posixPath === prefix.replace(/\/$/, "") || posixPath.startsWith(prefix);
}

function prefixMatch(posixPath, prefixes) {
  return prefixes.find((prefix) => hasPrefix(posixPath, prefix)) ?? null;
}

function readJson(path, label) {
  if (!existsSync(path)) {
    return { error: `${label} missing: ${path}` };
  }
  let text;
  try {
    text = readFileSync(path, "utf8");
  } catch (error) {
    return { error: `${label} unreadable: ${error instanceof Error ? error.message : String(error)}` };
  }
  try {
    return { value: JSON.parse(text) };
  } catch (error) {
    return { error: `${label} is not JSON: ${error instanceof Error ? error.message : String(error)}` };
  }
}

function asStringArray(value) {
  return Array.isArray(value) && value.every((item) => typeof item === "string" && item.length > 0);
}

export function loadPolicy(path) {
  const loaded = readJson(path, "policy");
  if (loaded.error) return loaded;
  const policy = loaded.value;
  const errors = [];
  if (!policy || typeof policy !== "object" || Array.isArray(policy)) {
    return { error: "policy is not an object" };
  }
  if (policy.schema !== "multica.dune-import-policy.v1") {
    errors.push(`policy schema must be multica.dune-import-policy.v1 (got ${JSON.stringify(policy.schema)})`);
  }
  if (!asStringArray(policy.edges) || policy.edges.length !== EDGES.length || EDGES.some((edge) => !policy.edges.includes(edge))) {
    errors.push(`policy.edges must be exactly ${EDGES.join(", ")}`);
  }
  for (const key of [
    "unprivileged_prefixes",
    "privileged_prefixes",
    "secret_reader_files",
    "secret_reader_specifiers",
    "shortcut_ipc_specifiers",
    "shortcut_node_from_prefixes",
    "board_scan_prefixes",
    "required_directories",
    "scan_directories",
  ]) {
    if (!asStringArray(policy[key]) || policy[key].length === 0) {
      errors.push(`policy.${key} must be a non-empty string array`);
    }
  }
  if (!Array.isArray(policy.alias_roots) || policy.alias_roots.length === 0) {
    errors.push("policy.alias_roots must list each @/ root");
  } else if (
    policy.alias_roots.some(
      (entry) =>
        !entry ||
        typeof entry.importer_prefix !== "string" ||
        typeof entry.target !== "string" ||
        !entry.importer_prefix.endsWith("/") ||
        entry.target.length === 0,
    )
  ) {
    errors.push("policy.alias_roots entries need importer_prefix and target");
  }
  if (!policy.electron_zones || typeof policy.electron_zones !== "object" || Array.isArray(policy.electron_zones)) {
    errors.push("policy.electron_zones must be an object of zone name to path prefix");
  } else {
    const zones = Object.entries(policy.electron_zones);
    if (zones.length < 3 || zones.some(([name, prefix]) => typeof name !== "string" || typeof prefix !== "string" || !prefix.endsWith("/"))) {
      errors.push("policy.electron_zones needs main, preload, and renderer prefixes ending in /");
    }
  }
  if (typeof policy.sanctioned_board_client !== "string" || policy.sanctioned_board_client.length === 0) {
    errors.push("policy.sanctioned_board_client must be a repo-relative path");
  }
  if (typeof policy.board_path_marker !== "string" || !policy.board_path_marker.startsWith("/api/")) {
    errors.push("policy.board_path_marker must be an /api/ path");
  }
  const named = policy.secret_reader_named_imports;
  if (!named || typeof named !== "object" || Array.isArray(named)) {
    errors.push("policy.secret_reader_named_imports must be an object");
  }
  if (errors.length > 0) return { errors };
  return { policy };
}

export function loadExceptions(path) {
  const loaded = readJson(path, "exception ledger");
  if (loaded.error) return loaded;
  const ledger = loaded.value;
  const errors = [];
  if (!ledger || typeof ledger !== "object" || Array.isArray(ledger)) {
    return { error: "exception ledger is not an object" };
  }
  if (ledger.schema !== "multica.dune-import-exception.v1") {
    errors.push("exception ledger schema must be multica.dune-import-exception.v1");
  }
  if (!Array.isArray(ledger.exceptions)) {
    errors.push("exception ledger exceptions must be an array");
    return { errors };
  }
  const seen = new Set();
  const exceptions = [];
  for (const [index, entry] of ledger.exceptions.entries()) {
    const where = `exceptions[${index}]`;
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) {
      errors.push(`${where} must be an object`);
      continue;
    }
    const { id, edge, from, specifier, reason, stamp } = entry;
    if (typeof id !== "string" || !/^[a-z0-9][a-z0-9-]{2,64}$/.test(id)) {
      errors.push(`${where}.id must be a lowercase slug`);
    } else if (seen.has(id)) {
      errors.push(`${where}.id duplicates ${id}`);
    } else {
      seen.add(id);
    }
    if (!EDGES.includes(edge)) errors.push(`${where}.edge must be one of ${EDGES.join(", ")}`);
    if (typeof from !== "string" || !from.includes("/")) errors.push(`${where}.from must be a repo-relative path`);
    if (typeof specifier !== "string" || specifier.length === 0) errors.push(`${where}.specifier must be a non-empty string`);
    if (typeof reason !== "string" || reason.trim().length < 12) {
      errors.push(`${where}.reason must explain the exception`);
    }
    if (typeof stamp !== "string" || stamp.trim().length < 8 || /^todo$/i.test(stamp.trim())) {
      errors.push(`${where}.stamp must be a real stamp, at least 8 characters`);
    }
    if (errors.length === 0) exceptions.push({ id, edge, from, specifier, reason, stamp });
  }
  if (errors.length > 0) return { errors };
  return { exceptions };
}

function walk(dir, out) {
  let entries;
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch (error) {
    return { error: `cannot read ${dir}: ${error instanceof Error ? error.message : String(error)}` };
  }
  for (const dirent of entries) {
    if (dirent.name.startsWith(".")) continue;
    const full = join(dir, dirent.name);
    if (dirent.isDirectory()) {
      if (SKIP_DIRS.has(dirent.name)) continue;
      const nested = walk(full, out);
      if (nested.error) return nested;
    } else if (SOURCE_EXTENSIONS.some((ext) => dirent.name.endsWith(ext))) {
      out.push(full);
    }
  }
  return {};
}

function resolutionCandidates(base) {
  const candidates = [base];
  for (const ext of SOURCE_EXTENSIONS) {
    candidates.push(`${base}${ext}`, join(base, `index${ext}`));
  }
  if (base.endsWith(".js")) {
    const stem = base.slice(0, -3);
    candidates.push(`${stem}.ts`, `${stem}.tsx`, `${stem}.mts`, `${stem}.cts`);
  }
  if (base.endsWith(".d.ts")) candidates.push(base);
  return candidates;
}

function stripQuery(specifier) {
  const query = specifier.indexOf("?");
  const hash = specifier.indexOf("#");
  let end = specifier.length;
  if (query >= 0) end = Math.min(end, query);
  if (hash >= 0) end = Math.min(end, hash);
  return specifier.slice(0, end);
}

function assetExtension(specifier) {
  const clean = stripQuery(specifier);
  const dot = clean.lastIndexOf(".");
  if (dot < 0) return null;
  const ext = clean.slice(dot).toLowerCase();
  return ASSET_EXTENSIONS.has(ext) ? ext : null;
}

function isGeneratedTarget(specifier, posixPath) {
  return (
    specifier === "@/.source" ||
    posixPath.includes("/.next/") ||
    posixPath.endsWith("/.next") ||
    posixPath.includes("/.source/") ||
    posixPath.endsWith("/.source")
  );
}

export function resolveSpecifier(specifier, importerAbs, known, policy) {
  const clean = stripQuery(specifier);
  if (!clean.startsWith(".") && !clean.startsWith("@/")) {
    return { external: true, specifier: clean };
  }
  let base;
  if (clean.startsWith("@/")) {
    const importer = repoRelative(importerAbs);
    const alias = policy.alias_roots.find((entry) => importer.startsWith(entry.importer_prefix));
    if (!alias) {
      return { error: `@/ alias has no root for ${importer} importing ${specifier}` };
    }
    base = join(repoRoot, alias.target, clean.slice(2));
  } else {
    base = resolve(dirname(importerAbs), clean);
  }
  if (assetExtension(clean)) {
    return { asset: true, specifier: clean };
  }
  for (const candidate of resolutionCandidates(base)) {
    if (known.has(candidate)) return { file: candidate, specifier: clean };
  }
  const posix = repoRelative(base);
  if (prefixMatch(posix, policy.privileged_prefixes) || posix.startsWith("server/")) {
    return { privilegedUnresolved: posix, specifier: clean };
  }
  if (isGeneratedTarget(clean, posix)) {
    return { generated: true, specifier: clean };
  }
  return { unresolved: true, specifier: clean, target: posix };
}

function zoneOf(posixPath, zones) {
  for (const [name, prefix] of Object.entries(zones)) {
    if (hasPrefix(posixPath, prefix)) return name;
  }
  return null;
}

function addFinding(findings, edge, from, specifier, detail) {
  findings.push({ edge, from, specifier, detail });
}

function literalTexts(node, out) {
  if (!node) return;
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) {
    out.push(node.text);
    return;
  }
  if (ts.isTemplateExpression(node)) {
    out.push(node.head.text);
    for (const span of node.templateSpans) {
      literalTexts(span.expression, out);
      out.push(span.literal.text);
    }
    return;
  }
  ts.forEachChild(node, (child) => literalTexts(child, out));
}

function boardPathPattern(marker) {
  const escaped = marker.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`(?:^|[^A-Za-z0-9_])${escaped}(?:\\/|\\?|$|[^A-Za-z0-9_])|^${escaped}$`);
}

function isFetchCallee(expr) {
  if (ts.isIdentifier(expr)) return expr.text === "fetch" || HTTP_LIBS.has(expr.text);
  if (!ts.isPropertyAccessExpression(expr)) return false;
  if (expr.name.text === "fetch") return true;
  return (
    HTTP_METHODS.has(expr.name.text) &&
    ts.isIdentifier(expr.expression) &&
    HTTP_LIBS.has(expr.expression.text)
  );
}

function stringBindings(sourceFile) {
  const bindings = new Map();
  const visit = (node) => {
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer) {
      const parts = [];
      literalTexts(node.initializer, parts);
      if (parts.length > 0) bindings.set(node.name.text, parts.join(""));
    }
    ts.forEachChild(node, visit);
  };
  visit(sourceFile);
  return bindings;
}

function importRecords(sourceFile) {
  const records = [];
  const visit = (node) => {
    if (
      (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) &&
      node.moduleSpecifier &&
      ts.isStringLiteral(node.moduleSpecifier)
    ) {
      const names = [];
      if (ts.isImportDeclaration(node) && node.importClause?.namedBindings && ts.isNamedImports(node.importClause.namedBindings)) {
        for (const element of node.importClause.namedBindings.elements) {
          if (!element.isTypeOnly) names.push((element.propertyName ?? element.name).text);
        }
      }
      records.push({
        specifier: node.moduleSpecifier.text,
        typeOnly: ts.isImportDeclaration(node) ? node.importClause?.isTypeOnly === true : false,
        names,
      });
    } else if (
      ts.isCallExpression(node) &&
      node.expression.kind === ts.SyntaxKind.ImportKeyword &&
      node.arguments.length > 0 &&
      ts.isStringLiteral(node.arguments[0])
    ) {
      records.push({ specifier: node.arguments[0].text, typeOnly: false, names: [] });
    } else if (
      ts.isCallExpression(node) &&
      ts.isIdentifier(node.expression) &&
      node.expression.text === "require" &&
      node.arguments.length > 0 &&
      ts.isStringLiteral(node.arguments[0])
    ) {
      records.push({ specifier: node.arguments[0].text, typeOnly: false, names: [] });
    } else if (
      ts.isImportTypeNode(node) &&
      ts.isLiteralTypeNode(node.argument) &&
      ts.isStringLiteral(node.argument.literal)
    ) {
      records.push({ specifier: node.argument.literal.text, typeOnly: true, names: [] });
    }
    ts.forEachChild(node, visit);
  };
  visit(sourceFile);
  return records;
}

function boardCalls(sourceFile, marker) {
  const pattern = boardPathPattern(marker);
  const bindings = stringBindings(sourceFile);
  const hits = [];
  const visit = (node) => {
    if (ts.isCallExpression(node) && isFetchCallee(node.expression)) {
      const parts = [];
      for (const arg of node.arguments) {
        if (ts.isIdentifier(arg)) {
          const bound = bindings.get(arg.text);
          if (bound) parts.push(bound);
        }
        literalTexts(arg, parts);
      }
      const text = parts.join(" ");
      if (pattern.test(text) || parts.some((part) => part === marker || pattern.test(part))) {
        const shown = parts.find((part) => pattern.test(part) || part.includes(marker)) ?? marker;
        hits.push(shown.slice(0, 180));
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sourceFile);
  return hits;
}

function classifyImport(record, from, resolved, policy, nodeBuiltins) {
  const findings = [];
  const specifier = record.specifier;
  const clean = stripQuery(specifier);
  const unprivileged = prefixMatch(from, policy.unprivileged_prefixes);
  const targetPath = resolved.file ? repoRelative(resolved.file) : resolved.privilegedUnresolved ?? null;

  const namedSecret = policy.secret_reader_named_imports?.[clean];
  const secretNamed = Array.isArray(namedSecret) && record.names.some((name) => namedSecret.includes(name));
  const secretSpecifier = policy.secret_reader_specifiers.includes(clean) || secretNamed;
  const secretFile = targetPath && policy.secret_reader_files.includes(targetPath);
  if (unprivileged && (secretSpecifier || secretFile)) {
    addFinding(
      findings,
      "secret-reader-in-ui",
      from,
      clean,
      secretFile ? `imports secret reader ${targetPath}` : `imports secret-reading module ${clean}`,
    );
  }

  const fromZone = zoneOf(from, policy.electron_zones);
  const toZone = targetPath ? zoneOf(targetPath, policy.electron_zones) : null;
  const nodeBuiltin = nodeBuiltins.has(clean) || nodeBuiltins.has(clean.startsWith("node:") ? clean.slice(5) : `node:${clean}`);
  const shortcutSpecifier =
    policy.shortcut_ipc_specifiers.includes(clean) && Boolean(unprivileged);
  const shortcutNode = nodeBuiltin && prefixMatch(from, policy.shortcut_node_from_prefixes);
  const shortcutZone = fromZone && toZone && fromZone !== toZone;
  if (shortcutSpecifier || shortcutNode || shortcutZone) {
    const detail = shortcutZone
      ? `${fromZone} imported ${toZone} (${targetPath})`
      : shortcutNode
        ? `renderer/shared imported Node builtin ${clean}`
        : `imported ${clean} outside the preload bridge`;
    addFinding(findings, "shortcut-ipc", from, shortcutZone ? targetPath : clean, detail);
  }

  const privilegedTarget =
    (targetPath && prefixMatch(targetPath, policy.privileged_prefixes)) ||
    (resolved.privilegedUnresolved && prefixMatch(resolved.privilegedUnresolved, policy.privileged_prefixes));
  if (unprivileged && privilegedTarget) {
    addFinding(
      findings,
      "unprivileged-to-privileged",
      from,
      clean,
      `imports privileged ${targetPath ?? resolved.privilegedUnresolved}`,
    );
  }
  return findings;
}

export function scanRepo(root, policy) {
  const errors = [];
  if (builtinModules.length < 10) {
    errors.push("node builtin module list is empty; cannot prove the IPC ban");
  }
  const nodeBuiltins = new Set(builtinModules.flatMap((name) => [name, `node:${name}`]));

  for (const dir of policy.required_directories) {
    const abs = join(root, dir);
    if (!existsSync(abs) || !statSync(abs).isDirectory()) {
      errors.push(`required directory missing: ${dir}`);
    }
  }
  const sanctioned = join(root, policy.sanctioned_board_client);
  if (!existsSync(sanctioned)) {
    errors.push(`sanctioned board client missing: ${policy.sanctioned_board_client}`);
  }
  for (const file of policy.secret_reader_files) {
    if (!existsSync(join(root, file))) errors.push(`secret reader missing: ${file}`);
  }
  if (errors.length > 0) return { errors, findings: [] };

  const files = [];
  for (const dir of policy.scan_directories) {
    const abs = join(root, dir);
    if (!existsSync(abs)) {
      errors.push(`scan directory missing: ${dir}`);
      continue;
    }
    const walked = walk(abs, files);
    if (walked.error) errors.push(walked.error);
  }
  if (errors.length > 0) return { errors, findings: [] };
  if (files.length === 0) return { errors: ["scan produced zero source files"], findings: [] };

  const known = new Set(files);
  const findings = [];
  for (const file of files) {
    let text;
    try {
      text = readFileSync(file, "utf8");
    } catch (error) {
      errors.push(`unreadable ${repoRelative(file)}: ${error instanceof Error ? error.message : String(error)}`);
      continue;
    }
    const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, scriptKind(file));
    const diagnostics = source.parseDiagnostics ?? [];
    if (diagnostics.length > 0) {
      const message = ts.flattenDiagnosticMessageText(diagnostics[0].messageText, "\n");
      errors.push(`parse error ${repoRelative(file)}:${diagnostics[0].start ?? 0}: ${message}`);
      continue;
    }
    const from = repoRelative(file);
    for (const record of importRecords(source)) {
      const resolved = resolveSpecifier(record.specifier, file, known, policy);
      if (resolved.error) {
        errors.push(resolved.error);
        continue;
      }
      if (resolved.unresolved) {
        errors.push(`unresolved import ${from} -> ${record.specifier}`);
        continue;
      }
      findings.push(...classifyImport(record, from, resolved, policy, nodeBuiltins));
    }
    if (prefixMatch(from, policy.board_scan_prefixes)) {
      for (const hit of boardCalls(source, policy.board_path_marker)) {
        addFinding(
          findings,
          "parallel-board-client",
          from,
          hit,
          `HTTP call carries ${policy.board_path_marker}; the sanctioned client is ${policy.sanctioned_board_client}`,
        );
      }
    }
  }
  return { errors, findings };
}

function scriptKind(file) {
  if (file.endsWith(".tsx")) return ts.ScriptKind.TSX;
  if (file.endsWith(".ts") || file.endsWith(".mts") || file.endsWith(".cts")) return ts.ScriptKind.TS;
  if (file.endsWith(".jsx")) return ts.ScriptKind.JSX;
  return ts.ScriptKind.JS;
}

export function applyExceptions(findings, exceptions) {
  const unused = [];
  const remaining = [...findings];
  for (const exception of exceptions) {
    const index = remaining.findIndex(
      (finding) =>
        finding.edge === exception.edge && finding.from === exception.from && finding.specifier === exception.specifier,
    );
    if (index < 0) unused.push(exception);
    else remaining.splice(index, 1);
  }
  return { remaining, unused };
}

export function evaluate(root, policyPath, exceptionsPath) {
  const policyLoaded = loadPolicy(policyPath);
  if (policyLoaded.error) return { errors: [policyLoaded.error], findings: [], unused: [] };
  if (policyLoaded.errors) return { errors: policyLoaded.errors, findings: [], unused: [] };
  const exceptionsLoaded = loadExceptions(exceptionsPath);
  if (exceptionsLoaded.error) return { errors: [exceptionsLoaded.error], findings: [], unused: [] };
  if (exceptionsLoaded.errors) return { errors: exceptionsLoaded.errors, findings: [], unused: [] };
  const scanned = scanRepo(root, policyLoaded.policy);
  if (scanned.errors.length > 0) return { errors: scanned.errors, findings: scanned.findings, unused: [] };
  const applied = applyExceptions(scanned.findings, exceptionsLoaded.exceptions);
  const errors = applied.unused.map(
    (exception) => `unused stamped exception ${exception.id} (${exception.edge} ${exception.from} ${exception.specifier})`,
  );
  return { errors, findings: applied.remaining, unused: applied.unused };
}

function main() {
  const policyPath = process.env.DUNE_IMPORT_POLICY ?? join(repoRoot, "scripts/dune-import-bans.policy.json");
  const exceptionsPath =
    process.env.DUNE_IMPORT_EXCEPTIONS ?? join(repoRoot, "scripts/dune-import-bans.exceptions.json");
  const result = evaluate(repoRoot, policyPath, exceptionsPath);
  if (result.errors.length > 0 || result.findings.length > 0) {
    console.error("dune-import-bans: FAIL");
    for (const error of result.errors) console.error(`  ${error}`);
    for (const finding of result.findings) {
      console.error(`  ${finding.edge} ${finding.from} -> ${finding.specifier}`);
      console.error(`    ${finding.detail}`);
    }
    console.error(
      "  Stamp a real exception in scripts/dune-import-bans.exceptions.json or remove the edge.",
    );
    process.exit(1);
  }
  console.log("dune-import-bans: ok (0 banned edges)");
}

const invokedDirectly =
  process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;

if (invokedDirectly) main();

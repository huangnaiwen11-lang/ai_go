import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  classifyCall,
  extractCalls,
  formatMatrixValidationReport,
  validateMigrationMatrixDocument,
} from './lib/phase0-audit-lib.mjs';

const serviceRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const sourceRoot = path.resolve(serviceRoot, '../ai-host-v2-platform-main');
const adminApp = path.join(sourceRoot, 'admin/src/App.tsx');
const adminRoot = path.join(sourceRoot, 'admin');
const routeFile = path.join(sourceRoot, 'backend/src/bootstrap/registerApiRoutes.js');
const defaultOutDir = path.join(serviceRoot, 'docs/audit');
const defaultMatrixPath = path.join(defaultOutDir, 'api-migration-matrix.json');
const outputDirectoryOverride = process.env.PHASE0_AUDIT_OUTPUT_DIR;
// 此覆盖项只用于隔离本地验证；正常运行仍使用已纳入版本控制的 docs/audit 目录和既定产物路径。
const outDir = outputDirectoryOverride
  ? path.resolve(outputDirectoryOverride)
  : defaultOutDir;
const argumentsList = process.argv.slice(2);

function parseCommandLine(values) {
  if (values.length === 0) return { mode: 'generate' };

  if (values.length === 1 && values[0] === '--validate-matrix') {
    return { mode: 'validate', matrixPath: defaultMatrixPath };
  }

  if (
    values.length === 3
    && values[0] === '--validate-matrix'
    && values[1] === '--matrix-path'
    && values[2] !== ''
    && !values[2].startsWith('--')
  ) {
    return { mode: 'validate', matrixPath: path.resolve(process.cwd(), values[2]) };
  }

  return { mode: 'invalid' };
}

function validateMatrix(inputMatrixPath) {
  // 此路径刻意保持只读，让受限环境无需访问已纳入版本控制的审计产物，也能校验受控矩阵。
  let matrix;
  try {
    matrix = JSON.parse(fs.readFileSync(inputMatrixPath, 'utf8'));
  } catch {
    process.stderr.write('matrix validation failed\n');
    process.exitCode = 1;
    return;
  }

  const matrixEntryCount = Array.isArray(matrix?.entries) ? matrix.entries.length : 0;
  const validation = validateMigrationMatrixDocument(matrix);

  console.log(formatMatrixValidationReport(validation));
  console.log(`matrix validation: ${matrixEntryCount} entries; ${validation.valid ? 'valid' : 'invalid'}`);

  if (!validation.valid) process.exitCode = 1;
}

const sourceFiles = (dir) => fs.readdirSync(dir, { withFileTypes: true })
  .sort((left, right) => left.name.localeCompare(right.name))
  .flatMap((entry) => {
    const fullPath = path.join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(fullPath);
    return /\.(ts|tsx)$/.test(entry.name) && !/\.test\.(ts|tsx)$/.test(entry.name) ? [fullPath] : [];
  });
const read = (file) => fs.readFileSync(file, 'utf8');
const scanCalls = (dir) => sourceFiles(dir).flatMap((file) => (
  extractCalls(path.relative(sourceRoot, file), read(file))
));
const sortedUnique = (values) => [...new Set(values)].sort((left, right) => left.localeCompare(right));
const toPublicApiPath = (endpointPath) => (endpointPath === '/' ? '/api' : `/api${endpointPath}`);
const compareEndpoints = (left, right) => (
  left.path.localeCompare(right.path) || left.method.localeCompare(right.method)
);

/**
 * 将管理端的相对导入解析为真实源码文件。只处理本地静态导入；无法静态解析的路径
 * 不会被编造成页面依赖，后续仍需要运行时证据补齐。
 */
function resolveLocalModule(importerPath, specifier) {
  if (!specifier.startsWith('.')) return null;

  const candidate = path.resolve(path.dirname(importerPath), specifier);
  const candidates = [
    candidate,
    `${candidate}.ts`,
    `${candidate}.tsx`,
    path.join(candidate, 'index.ts'),
    path.join(candidate, 'index.tsx'),
  ];
  return candidates.find((item) => {
    try {
      return fs.statSync(item).isFile();
    } catch {
      return false;
    }
  }) ?? null;
}

function relativeToSourceRoot(sourcePath) {
  return path.relative(sourceRoot, sourcePath).split(path.sep).join('/');
}

/**
 * App.tsx 同时使用普通 import 与 React.lazy。两种写法都显式收集，才能让页面路由
 * 稳定关联到真实组件文件，而不是错误地归到 LazyPage 包装器。
 */
function extractAdminPageComponentSources(appSource) {
  const components = new Map();
  const addComponent = (component, specifier) => {
    const sourcePath = resolveLocalModule(adminApp, specifier);
    if (sourcePath != null) components.set(component, sourcePath);
  };

  for (const match of appSource.matchAll(/import\s+([A-Z][A-Za-z0-9_]*)\s+from\s+['"]([^'"]+)['"]/g)) {
    addComponent(match[1], match[2]);
  }
  for (const match of appSource.matchAll(/const\s+([A-Z][A-Za-z0-9_]*)\s*=\s*lazy\(\(\)\s*=>\s*import\(['"]([^'"]+)['"]\)\)/g)) {
    addComponent(match[1], match[2]);
  }

  return components;
}

/**
 * 审计只读取每个路由行上最终渲染的 *Page 组件。Navigate 别名和外层 /* 路由不表示
 * 独立页面，故刻意排除；这样不会把重定向误写成可迁移页面。
 */
function extractAdminPageRoutes(appSource, componentSources) {
  return appSource
    .split('\n')
    .flatMap((line) => {
      const route = line.match(/<Route\s+path="([^"]+)"\s+element=\{(.+)\}\s*\/>/);
      if (route == null || route[2].includes('<Navigate')) return [];

      const pageComponents = [...route[2].matchAll(/<([A-Z][A-Za-z0-9_]*Page)\b/g)]
        .map((match) => match[1])
        .filter((component) => component !== 'LazyPage');
      const component = pageComponents.at(-1);
      if (component == null) return [];

      return [{ path: route[1], component, sourcePath: componentSources.get(component) ?? null }];
    })
    .sort((left, right) => left.path.localeCompare(right.path));
}

/**
 * 仅枚举页面文件中直接导入的 admin/src/api 模块。子组件、动态 import、全局状态和
 * 运行时拼接请求都不在这份静态证据内，避免误把不完整扫描说成完整 API 契约。
 */
function extractDirectAdminApiModules(componentSourcePath) {
  if (componentSourcePath == null) return [];

  const pageSource = read(componentSourcePath);
  const apiModules = new Set();
  for (const match of pageSource.matchAll(/\bfrom\s+['"]([^'"]+)['"]/g)) {
    const importedPath = resolveLocalModule(componentSourcePath, match[1]);
    if (importedPath == null || !importedPath.startsWith(`${adminRoot}${path.sep}src${path.sep}api${path.sep}`)) {
      continue;
    }
    apiModules.add(relativeToSourceRoot(importedPath));
  }

  return [...apiModules].sort((left, right) => left.localeCompare(right));
}

function buildAdminPageApiPermissionInventory(appSource) {
  const componentSources = extractAdminPageComponentSources(appSource);
  const pages = extractAdminPageRoutes(appSource, componentSources).map((route) => ({
    path: route.path,
    component: route.component,
    componentSource: route.sourcePath == null ? null : relativeToSourceRoot(route.sourcePath),
    directApiModules: extractDirectAdminApiModules(route.sourcePath),
    accessGuard: route.path === '/login'
      ? '登录入口：不经过 ProtectedRoute 与 AdminAccessGuard'
      : '受 ProtectedRoute 与 AdminAccessGuard 统一保护',
    apiEvidenceLimit: '仅扫描当前页面文件的直接 API import；不含子组件或运行时调用。',
    classification: '待核实',
  }));

  return {
    schemaVersion: 1,
    generatedAt: new Date().toISOString(),
    entryPath: '/admin/',
    sourceOfTruth: adminRoot,
    accessGuard: {
      sources: [
        'admin/src/App.tsx#ProtectedRoute',
        'admin/src/App.tsx#AdminAccessGuard',
        'admin/src/auth/adminAccess.ts#canAccessAdminPath',
      ],
      staticRoleCases: [
        { roleCase: 'super_admin', staticRule: '全部路径允许' },
        { roleCase: 'legacy_admin_without_permission_scope', staticRule: '全部路径允许' },
        { roleCase: 'admin_with_permission_scope', staticRule: '仅已授权菜单路径允许' },
      ],
      evidenceStatus: '待核实',
    },
    pages,
    note: '页面、权限和 API 关联仅来自静态源码。缺少子组件、实际访问、接口契约、审计与回滚证据，不能作为 Go 或 Vue 迁移、路由放行或流量分流依据。',
  };
}

let temporaryFileSequence = 0;

function writeFileAtomically(destinationPath, content) {
  const destinationDirectory = path.dirname(destinationPath);
  const temporaryPath = path.join(
    destinationDirectory,
    `.${path.basename(destinationPath)}.${process.pid}.${temporaryFileSequence++}.tmp`,
  );

  fs.mkdirSync(destinationDirectory, { recursive: true });
  try {
    fs.writeFileSync(temporaryPath, content, { encoding: 'utf8', flag: 'wx' });
    fs.renameSync(temporaryPath, destinationPath);
  } finally {
    // 同目录 rename 是原子操作；这里负责清理发生在替换前的失败所遗留的临时文件。
    fs.rmSync(temporaryPath, { force: true });
  }
}

function generateAuditArtifacts() {
  const routes = read(routeFile);
  const adminAppSource = read(adminApp);
  const routePrefixes = [...routes.matchAll(/apiV1Router\.use\(['"]([^'"]+)['"]/g)]
    .map((match) => match[1])
    .sort((left, right) => left.localeCompare(right));
  const adminPageApiPermissionInventory = buildAdminPageApiPermissionInventory(adminAppSource);
  const adminRoutes = adminPageApiPermissionInventory.pages.map((page) => ({
    path: page.path,
    component: `${page.component}.tsx`,
    source: 'admin/src/App.tsx',
    classification: '待核实',
  }));
  const frontendCalls = scanCalls(path.join(sourceRoot, 'frontend/src'));
  const adminCalls = scanCalls(path.join(sourceRoot, 'admin/src'));
  const candidates = new Map();

  for (const [source, calls] of [['frontend', frontendCalls], ['admin', adminCalls]]) {
    for (const call of calls) {
      const key = `${call.method}\u0000${call.path}`;
      const candidate = candidates.get(key) ?? {
        method: call.method,
        path: call.path,
        frontendCallsites: [],
        adminCallsites: [],
      };

      candidate[`${source}Callsites`].push(call.callsite);
      candidates.set(key, candidate);
    }
  }

  const entries = [...candidates.values()]
    .map((candidate) => classifyCall({
      ...candidate,
      frontendCallsites: sortedUnique(candidate.frontendCallsites),
      adminCallsites: sortedUnique(candidate.adminCallsites),
    }, routePrefixes))
    .map((entry) => ({ ...entry, path: toPublicApiPath(entry.path) }))
    .sort(compareEndpoints);

  writeFileAtomically(path.join(outDir, 'api-inventory.generated.json'), `${JSON.stringify({
    schemaVersion: 1,
    generatedAt: new Date().toISOString(),
    sourceOfTruth: sourceRoot,
    routePrefixes,
    entries,
    note: '静态文本候选均为待人工核验；缺少完整接口契约和业务语义基线，不能作为 Go 分流依据。',
  }, null, 2)}\n`);
  writeFileAtomically(path.join(outDir, 'admin-page-inventory.generated.json'), `${JSON.stringify({
    schemaVersion: 1,
    generatedAt: new Date().toISOString(),
    entryPath: '/admin/',
    sourceOfTruth: path.join(sourceRoot, 'admin'),
    pages: adminRoutes,
    note: '静态文本候选均为待人工核验；缺少访问、权限和接口契约证据，不能作为 Go 分流依据。',
  }, null, 2)}\n`);
  writeFileAtomically(
    path.join(outDir, 'admin-page-api-permission-inventory.generated.json'),
    `${JSON.stringify(adminPageApiPermissionInventory, null, 2)}\n`,
  );

  console.log(`generated ${entries.length} endpoint entries; ${routePrefixes.length} registered route prefixes`);
}

const command = parseCommandLine(argumentsList);

if (command.mode === 'invalid') {
  process.stderr.write('invalid arguments\n');
  process.exitCode = 1;
} else if (command.mode === 'validate') {
  validateMatrix(command.matrixPath);
} else {
  generateAuditArtifacts();
}

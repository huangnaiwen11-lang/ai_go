import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import * as phase0Audit from './lib/phase0-audit-lib.mjs';

const {
  classifyCall,
  extractCalls,
  formatMatrixValidationReport,
  validateMigrationMatrix,
} = phase0Audit;

const serviceRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const auditDocumentRoot = path.join(serviceRoot, 'docs/audit');
const phase23EntryGatesPath = path.join(auditDocumentRoot, 'phase-2-3-entry-gates.md');
const goldenCasesPath = path.join(serviceRoot, 'docs/semantics/golden-cases.json');
const phaseCompletionStatusPath = path.join(auditDocumentRoot, 'phase-completion-status.md');
// 这是报告与矩阵共享的唯一历史 Animate 路由来源。
const RETIRED_ANIMATE_ROUTES = [
  { method: 'GET', path: '/api/animate/cost' },
  { method: 'POST', path: '/api/animate/start' },
  { method: 'GET', path: '/api/animate/status/:taskId' },
  { method: 'GET', path: '/api/animate/history' },
  { method: 'POST', path: '/api/animate/cancel/:taskId' },
  { method: 'DELETE', path: '/api/animate/:taskId' },
  { method: 'POST', path: '/api/animate/refund/:taskId' },
];

function isRetiredAnimatePath(entryPath) {
  return /^\/api\/animate(?:\/|$)/.test(entryPath);
}

function readLocalJsonDocument(documentPath) {
  return JSON.parse(fs.readFileSync(documentPath, 'utf8'));
}

function runAuditCommand(argumentsList, environment = {}) {
  return spawnSync(process.execPath, ['scripts/phase0-audit.mjs', ...argumentsList], {
    cwd: serviceRoot,
    encoding: 'utf8',
    env: {
      ...process.env,
      ...environment,
    },
  });
}

function withTemporaryAuditOutput(assertResult) {
  const temporaryDirectory = fs.mkdtempSync(path.join(os.tmpdir(), 'phase0-audit-output-'));
  const auditOutputDirectory = path.join(temporaryDirectory, 'audit-output');

  try {
    assertResult({ temporaryDirectory, auditOutputDirectory });
  } finally {
    fs.rmSync(temporaryDirectory, { recursive: true, force: true });
  }
}

function withTemporaryMatrix(matrix, assertResult) {
  const temporaryDirectory = fs.mkdtempSync(path.join(os.tmpdir(), 'phase0-matrix-validation-'));
  const temporaryMatrixPath = path.join(temporaryDirectory, 'matrix.json');

  try {
    fs.writeFileSync(temporaryMatrixPath, `${JSON.stringify(matrix)}\n`);
    const result = runAuditCommand([
      '--validate-matrix',
      '--matrix-path',
      temporaryMatrixPath,
    ]);

    assertResult({ result, report: result.stdout, temporaryDirectory });
  } finally {
    fs.rmSync(temporaryDirectory, { recursive: true, force: true });
  }
}

test('遗留 API 清单是已废弃产物而非接口迁移决策清单', () => {
  const inventory = readLocalJsonDocument(path.join(auditDocumentRoot, 'api-inventory.json'));

  assert.equal(inventory.artifactStatus, '已废弃');
  assert.equal(Object.hasOwn(inventory, 'status'), false);
  assert.equal(Object.hasOwn(inventory, 'entries'), false);
  assert.match(inventory.reason, /不承载接口迁移决策/);
});

test('阶段 0 报告中的候选数与生成审计清单保持一致', () => {
  const report = fs.readFileSync(path.join(auditDocumentRoot, 'phase-0-report.md'), 'utf8');
  const apiInventory = readLocalJsonDocument(path.join(auditDocumentRoot, 'api-inventory.generated.json'));
  const adminInventory = readLocalJsonDocument(path.join(auditDocumentRoot, 'admin-page-inventory.generated.json'));
  const counts = report.match(/静态扫描得到 (\d+) 个 API 候选、(\d+) 个后台页面/);

  assert.ok(counts, '报告必须声明与生成产物对应的候选数量');
  assert.equal(Number(counts[1]), apiInventory.entries.length);
  assert.equal(Number(counts[2]), adminInventory.pages.length);
});

test('阶段完成状态台账区分本地门禁和正式迁移，并冻结 Animate 的 Node-only 边界', () => {
  const statusLedger = fs.readFileSync(phaseCompletionStatusPath, 'utf8');
  const requiredStatements = [
    '本地工程门禁已完成',
    '正式迁移验收未完成',
    '真实运行验证、灰度观察和回滚演练',
    'T2I、I2I、I2V',
    'Animate 继续 Node-only',
    '不得连接、配置或切换生产环境',
  ];

  for (const statement of requiredStatements) {
    assert.ok(statusLedger.includes(statement), `阶段状态台账必须说明：${statement}`);
  }
});

test('阶段 0 报告准确冻结 Animate 历史路由和 retry 分支范围', () => {
  const report = fs.readFileSync(path.join(auditDocumentRoot, 'phase-0-report.md'), 'utf8');
  const retiredAnimateRoutes = RETIRED_ANIMATE_ROUTES.map(({ method, path: routePath }) => (
    `${method} ${routePath}`
  ));

  assert.match(report, /7 条\s*(?:`)?\/api\/animate\/(?:`)?\s*历史路由/);
  for (const route of retiredAnimateRoutes) {
    assert.ok(report.includes(route), `报告必须逐条列明 ${route}`);
  }
  assert.match(report, /候选淘汰/);
  assert.match(report, /存量 Animate.*继续 Node-only/);
  assert.match(report, /不构成 Go 放行或迁移完成/);
  assert.match(report, /POST \/api\/creations\/retry.*仅.*type=animate.*分支/);
});

test('Animate 内部路径匹配严格区分路径边界', () => {
  assert.equal(isRetiredAnimatePath('/api/animate'), true);
  assert.equal(isRetiredAnimatePath('/api/animate/cost'), true);
  assert.equal(isRetiredAnimatePath('/api/animatees/cost'), false);
});

test('阶段 3 离线证据门禁仅覆盖三种基础能力，且不授予迁移或切流许可', () => {
  const gates = fs.readFileSync(phase23EntryGatesPath, 'utf8');
  const generationEvidenceGate = gates.match(
    /### 已完成：三项基础创作能力的离线证据门禁\n([\s\S]*?)(?=\n#### 已废弃的 Animate 历史审计记录)/,
  );

  assert.ok(generationEvidenceGate, '阶段 3 必须单列三项基础能力的离线证据门禁');
  const gateText = generationEvidenceGate[1];
  const requiredStatements = [
    '`internal/generationcontract`',
    '`text_to_image`',
    '`image_to_image`',
    '`image_to_video`',
    '不创建 HTTP 路由、不访问数据库，也不提交生成任务',
    '明确拒绝 Animate',
    '不构成 Go 迁移或 Gateway 切流许可',
  ];

  for (const statement of requiredStatements) {
    assert.ok(gateText.includes(statement), `阶段 3 离线证据门禁必须说明：${statement}`);
  }
});

test('黄金用例 v2 分离证据状态、迁移资格和精确矩阵引用', () => {
  const goldenCases = readLocalJsonDocument(goldenCasesPath);

  assert.equal(goldenCases.schemaVersion, 2);
  for (const goldenCase of goldenCases.cases) {
    assert.equal(Object.hasOwn(goldenCase, 'status'), false, `${goldenCase.id} 不得复用 status 字段`);
    assert.equal(typeof goldenCase.evidenceStatus, 'string', `${goldenCase.id} 必须记录证据状态`);
    assert.equal(typeof goldenCase.migrationEligibility, 'string', `${goldenCase.id} 必须记录迁移资格`);
    assert.ok(Array.isArray(goldenCase.matrixRouteRefs), `${goldenCase.id} 必须引用精确矩阵路由`);
  }
});

test('黄金用例保持路由门禁：Growth Ping 已确认，其余待核实，Animate 仅由 Node 处理', () => {
  const goldenCases = readLocalJsonDocument(goldenCasesPath);
  const casesById = new Map(goldenCases.cases.map((goldenCase) => [goldenCase.id, goldenCase]));
  const growthPingCases = goldenCases.cases.filter((goldenCase) => goldenCase.id.startsWith('gateway-growth-ping'));
  const pendingCases = [
    'read-authorized',
    'generation-terminal-success',
    'generation-duplicate-callback',
    'sse-snapshot-reconnect',
  ].map((caseId) => casesById.get(caseId));
  const animateCases = goldenCases.cases.filter((goldenCase) => (
    goldenCase.id.includes('animate') || JSON.stringify(goldenCase.sourceEvidence).includes('/animate')
  ));

  assert.ok(growthPingCases.length > 0, '必须继续显式登记 Growth Ping 用例');
  for (const goldenCase of growthPingCases) {
    assert.equal(goldenCase.migrationEligibility, '确认迁移');
    assert.deepEqual(goldenCase.matrixRouteRefs, ['GET /api/growth/ping']);
  }
  for (const goldenCase of pendingCases) {
    assert.ok(goldenCase, '必须继续登记 Catalog、回调和 SSE 用例');
    assert.equal(goldenCase.migrationEligibility, '待核实（非路由放行）');
    assert.ok(goldenCase.matrixRouteRefs.length > 0);
  }
  assert.ok(animateCases.length > 0, '必须将 Animate 用例保留为历史行为');
  for (const goldenCase of animateCases) {
    assert.equal(goldenCase.migrationEligibility, '历史 Node-only');
  }
});

test('迁移矩阵保持 Animate 候选淘汰分类，并只淘汰 creations retry 的 Animate 分支', () => {
  const matrix = readLocalJsonDocument(path.join(auditDocumentRoot, 'api-migration-matrix.json'));
  const actualAnimateRoutes = matrix.entries
    .filter((entry) => isRetiredAnimatePath(entry.path))
    .map(({ method, path: entryPath }) => ({ method, path: entryPath }));
  const retryRetirements = matrix.entries
    .filter((entry) => (
      entry.method === 'POST'
      && entry.path === '/api/creations/retry'
      && entry.classification === '候选淘汰'
    ))
    .map(({ method, path: entryPath, scope }) => ({ method, path: entryPath, scope }));

  assert.deepEqual(
    actualAnimateRoutes,
    RETIRED_ANIMATE_ROUTES,
    '迁移矩阵的 /api/animate/ 路由必须精确包含这 7 条历史候选，且不得增减',
  );

  for (const route of RETIRED_ANIMATE_ROUTES) {
    const entry = matrix.entries.find((candidate) => (
      candidate.method === route.method && candidate.path === route.path
    ));

    assert.ok(entry, `必须保留 Animate 历史路由 ${route.method} ${route.path}`);
    assert.equal(entry.classification, '候选淘汰', `${route.method} ${route.path} 必须保持候选淘汰`);
  }
  assert.deepEqual(
    retryRetirements,
    [{
      method: 'POST',
      path: '/api/creations/retry',
      scope: {
        typeField: 'type',
        equals: 'animate',
      },
    }],
    'POST /api/creations/retry 只能淘汰 type=animate 分支',
  );
});

test('Phase 5～7 门禁锁定 Admin 三个权限分支及真实运行 capture 要求', () => {
  const gates = fs.readFileSync(path.join(auditDocumentRoot, 'phase-5-7-entry-gates.md'), 'utf8');
  const requiredStatements = [
    '`super_admin`：全部权限。',
    '无任何 permission scope 的 legacy `admin`：允许访问全部路径。',
    '有 permission scope 的 `admin`：才按菜单 scope 限制访问。',
    '以上三个权限分支均须取得真实运行 capture 才能迁移；静态权限代码不构成 Admin 迁移许可。',
  ];

  for (const statement of requiredStatements) {
    assert.ok(gates.includes(statement), `Phase 7 门禁必须锁定：${statement}`);
  }
});

test('Phase 5～6 静态合同清单冻结身份与钱包的高风险语义，但不授予迁移许可', () => {
  const inventory = readLocalJsonDocument(
    path.join(auditDocumentRoot, 'identity-wallet-static-contracts.json'),
  );
  const identityLogin = inventory.identity.routes.find((route) => (
    route.method === 'POST' && route.path === '/api/auth/login'
  ));
  const walletBalance = inventory.wallet.routes.find((route) => (
    route.method === 'GET' && route.path === '/api/wallet/balance'
  ));

  assert.equal(inventory.schemaVersion, 1);
  assert.equal(inventory.classification, '待核实');
  assert.equal(inventory.migrationPermission, '未授予');
  assert.ok(identityLogin, '必须记录登录入口');
  assert.deepEqual(identityLogin.knownProviders, ['apple', 'google', 'facebook', 'twitter']);
  assert.ok(identityLogin.aliases.includes('/api/v1/auth/login'));
  assert.ok(identityLogin.staticEvidence.includes('frontend/src/api/auth.ts'));
  assert.ok(walletBalance, '必须记录余额入口');
  assert.equal(walletBalance.readSideEffect, '可能创建 Wallet');
  assert.match(walletBalance.cacheContract, /private, no-store/);
  assert.ok(inventory.wallet.paymentBoundary.staticEvidence.includes('payment-service'));
  assert.match(inventory.note, /不含真实运行 capture/);
});

test('迁移矩阵只确认放行 Growth Ping，并将关键候选保持为待核实', () => {
  const matrix = readLocalJsonDocument(path.join(auditDocumentRoot, 'api-migration-matrix.json'));
  const confirmedRoutes = matrix.entries
    .filter((entry) => entry.classification === '确认迁移')
    .map(({ method, path: entryPath }) => ({ method, path: entryPath }));
  const pendingRoutes = [
    { method: 'GET', path: '/api/homepage/video-templates' },
    { method: 'GET', path: '/api/v1/homepage/video-templates' },
    { method: 'GET', path: '/api/users/me/generations/stream' },
    { method: 'POST', path: '/api/v1/internal/generation-callback' },
  ];

  assert.deepEqual(
    confirmedRoutes,
    [{ method: 'GET', path: '/api/growth/ping' }],
    '确认迁移集合必须严格且仅包含 GET /api/growth/ping',
  );
  for (const route of pendingRoutes) {
    const entry = matrix.entries.find((candidate) => (
      candidate.method === route.method && candidate.path === route.path
    ));

    assert.ok(entry, `迁移矩阵必须登记 ${route.method} ${route.path}`);
    assert.equal(entry.classification, '待核实', `${route.method} ${route.path} 不得被路由放行`);
  }
});

test('Phase 4～7 的 Media、Identity 与 Wallet 候选只登记为待核实', () => {
  const matrix = readLocalJsonDocument(path.join(auditDocumentRoot, 'api-migration-matrix.json'));
  const identityStaticContract = readLocalJsonDocument(
    path.join(auditDocumentRoot, 'identity-static-contract.json'),
  );
  const expectedCandidates = [
    { method: 'POST', path: '/api/upload/ugc/presign' },
    ...identityStaticContract.routes.map(({ method, path: routePath }) => ({
      method,
      path: routePath,
    })),
    { method: 'GET', path: '/api/wallet/balance' },
    { method: 'GET', path: '/api/wallet/ledger' },
    { method: 'POST', path: '/api/wallet/verify-purchase' },
    { method: 'GET', path: '/api/wallet/external-payment-methods' },
    { method: 'POST', path: '/api/wallet/create-external-checkout' },
    { method: 'POST', path: '/api/wallet/external-checkout-billing-prefill' },
    { method: 'POST', path: '/api/wallet/crypto/create-payment' },
    { method: 'POST', path: '/api/wallet/crypto/refresh-payment' },
    { method: 'GET', path: '/api/wallet/crypto/status/:orderId' },
  ];
  const confirmedOnlyFields = [
    'authentication',
    'requestExample',
    'responseExample',
    'dataDependencies',
    'errorCodes',
    'callVolume',
    'targetService',
    'semanticBaseline',
    'rollbackSwitch',
    'owner',
  ];
  const phase47Routes = matrix.entries
    .filter((entry) => (
      entry.path.startsWith('/api/upload/')
      || entry.path.startsWith('/api/auth/')
      || entry.path.startsWith('/api/wallet/')
    ))
    .map(({ method, path: entryPath }) => ({ method, path: entryPath }));
  const adminEntries = matrix.entries.filter((entry) => /\/admin(?:\/|$)/.test(entry.path));

  assert.deepEqual(
    phase47Routes,
    expectedCandidates,
    'Media、Identity 与 Wallet 范围内只能登记此处列明的精确待核实候选，且每条仅能登记一次',
  );
  assert.equal(adminEntries.length, 0, '静态证据不足时不得新增 Admin 迁移矩阵条目');

  for (const expected of expectedCandidates) {
    const entry = matrix.entries.find((candidate) => (
      candidate.method === expected.method && candidate.path === expected.path
    ));

    assert.ok(entry, `迁移矩阵必须登记 ${expected.method} ${expected.path}`);
    assert.equal(entry.classification, '待核实', `${expected.method} ${expected.path} 不得被路由放行`);
    assert.ok(Array.isArray(entry.evidence) && entry.evidence.length > 0, '待核实条目必须保留静态证据');
    assert.equal(typeof entry.blockingReason, 'string', '待核实条目必须说明阻塞原因');
    assert.match(entry.blockingReason, /Go 不得分流/);
    assert.match(entry.blockingReason, /新增写入方/);
    for (const field of confirmedOnlyFields) {
      assert.equal(Object.hasOwn(entry, field), false, `${expected.method} ${expected.path} 不得拥有确认迁移放行字段 ${field}`);
    }
  }
});

test('T2I、I2I 与 I2V 新建入口必须登记为待核实，且不得被 Go 路由放行', () => {
  const matrix = readLocalJsonDocument(path.join(auditDocumentRoot, 'api-migration-matrix.json'));
  const expectedCandidates = [
    {
      method: 'POST',
      path: '/api/chat/image/async',
      capabilities: ['T2I', 'I2I'],
    },
    {
      method: 'POST',
      path: '/api/chat/video',
      capabilities: ['I2V'],
    },
  ];

  for (const expected of expectedCandidates) {
    const entry = matrix.entries.find((candidate) => (
      candidate.method === expected.method && candidate.path === expected.path
    ));

    assert.ok(entry, `迁移矩阵必须登记 ${expected.method} ${expected.path}`);
    assert.equal(entry.classification, '待核实', `${expected.method} ${expected.path} 不得被路由放行`);
    assert.deepEqual(entry.capabilities, expected.capabilities);
    assert.match(entry.blockingReason, /Go 不得新增路由|不得切换写入方/);
  }

  const goldenCases = readLocalJsonDocument(goldenCasesPath);
  const goldenCasesById = new Map(goldenCases.cases.map((goldenCase) => [goldenCase.id, goldenCase]));
  for (const [caseId, routeRef] of [
    ['image-create-t2i-i2i', 'POST /api/chat/image/async'],
    ['video-create-i2v', 'POST /api/chat/video [imageUrl present]'],
  ]) {
    const goldenCase = goldenCasesById.get(caseId);

    assert.ok(goldenCase, `黄金用例必须登记 ${caseId}`);
    assert.equal(goldenCase.migrationEligibility, '待核实（非路由放行）');
    assert.deepEqual(goldenCase.matrixRouteRefs, [routeRef]);
  }
});

test('Animate 历史取消超时用例明确禁止新增 Go 迁移职责', () => {
  const goldenCases = readLocalJsonDocument(goldenCasesPath);
  const animateCancelTimeout = goldenCases.cases.find((goldenCase) => (
    goldenCase.id === 'generation-cancel-timeout'
  ));

  assert.ok(animateCancelTimeout, '必须保留 Animate 历史取消超时用例');
  assert.equal(
    animateCancelTimeout.expected,
    '沿用 pending/processing/completed/failed/cancelled 状态和原取消、超时、退款规则。这是 Animate 历史 Node-only 收敛，绝不构成新增 Go 路由、服务、写入方或结算协调许可。',
    '用例必须明确禁止将 Animate 历史收敛作为新增 Go 职责的许可',
  );
});

test('matrix CLI validates a temporary matrix without creating audit artifacts', () => {
  withTemporaryMatrix({ entries: null }, ({ result, report, temporaryDirectory }) => {
    assert.equal(result.status, 1, 'the temporary invalid matrix must fail validation');
    assert.match(report, /状态：未通过/);
    assert.deepEqual(
      fs.readdirSync(temporaryDirectory).sort(),
      ['matrix.json'],
      'validation must not create inventory or report artifacts beside a controlled input',
    );
  });
});

test('CLI rejects malformed arguments without generating audit artifacts', () => {
  const malformedArguments = [
    ['--valdiate-matrix'],
    ['--validate-matrix=1'],
    ['--matrix-path', 'matrix.json'],
    ['--validate-matrix', '--matrix-path'],
    ['--validate-matrix', '--matrix-path', 'one.json', '--matrix-path', 'two.json'],
    ['--validate-matrix', 'extra'],
  ];

  withTemporaryAuditOutput(({ auditOutputDirectory }) => {
    for (const argumentsList of malformedArguments) {
      const result = runAuditCommand(argumentsList, {
        PHASE0_AUDIT_OUTPUT_DIR: auditOutputDirectory,
      });

      assert.equal(result.status, 1, `${argumentsList.join(' ')} must fail`);
      assert.equal(result.stdout, '', 'invalid arguments must not produce a report');
      assert.equal(result.stderr, 'invalid arguments\n');
      assert.equal(
        fs.existsSync(auditOutputDirectory),
        false,
        'invalid arguments must not generate audit artifacts',
      );
    }
  });
});

test('generation writes audit artifacts atomically to an injected output directory', () => {
  withTemporaryAuditOutput(({ auditOutputDirectory }) => {
    const result = runAuditCommand([], {
      PHASE0_AUDIT_OUTPUT_DIR: auditOutputDirectory,
    });

    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /^generated \d+ endpoint entries; \d+ registered route prefixes\n$/);
    assert.deepEqual(fs.readdirSync(auditOutputDirectory).sort(), [
      'admin-page-api-permission-inventory.generated.json',
      'admin-page-inventory.generated.json',
      'api-inventory.generated.json',
    ]);
    assert.equal(
      fs.readdirSync(auditOutputDirectory).some((fileName) => fileName.endsWith('.tmp')),
      false,
      'successful writes must leave no temporary audit files',
    );

    for (const fileName of fs.readdirSync(auditOutputDirectory)) {
      const document = JSON.parse(fs.readFileSync(path.join(auditOutputDirectory, fileName), 'utf8'));
      assert.equal(document.schemaVersion, 1);
    }
  });
});

test('Admin 静态清单将页面、直接 API 导入与统一权限守卫关联为待核实证据', () => {
  withTemporaryAuditOutput(({ auditOutputDirectory }) => {
    const result = runAuditCommand([], {
      PHASE0_AUDIT_OUTPUT_DIR: auditOutputDirectory,
    });
    assert.equal(result.status, 0, result.stderr);

    const document = readLocalJsonDocument(
      path.join(auditOutputDirectory, 'admin-page-api-permission-inventory.generated.json'),
    );
    const walletPage = document.pages.find((page) => page.path === '/wallet');

    assert.equal(document.schemaVersion, 1);
    assert.deepEqual(document.accessGuard.sources, [
      'admin/src/App.tsx#ProtectedRoute',
      'admin/src/App.tsx#AdminAccessGuard',
      'admin/src/auth/adminAccess.ts#canAccessAdminPath',
    ]);
    assert.deepEqual(
      document.accessGuard.staticRoleCases.map((item) => item.roleCase),
      ['super_admin', 'legacy_admin_without_permission_scope', 'admin_with_permission_scope'],
    );
    assert.ok(walletPage, '必须登记 /wallet 页面');
    assert.equal(walletPage.component, 'WalletPage');
    assert.equal(walletPage.componentSource, 'admin/src/pages/wallet/WalletPage.tsx');
    assert.deepEqual(walletPage.directApiModules, [
      'admin/src/api/adminData.ts',
      'admin/src/api/index.ts',
      'admin/src/api/wallet.ts',
    ]);
    assert.equal(walletPage.classification, '待核实');
    assert.match(walletPage.apiEvidenceLimit, /不含子组件或运行时调用/);
  });
});

function confirmedMigration(overrides = {}) {
  return {
    method: 'GET',
    path: '/api/growth/ping',
    classification: '确认迁移',
    evidence: ['route contract'],
    authentication: 'no requireAuth',
    requestExample: { 'X-Request-Id': 'path-validation-test' },
    responseExample: { success: true, data: { ok: true } },
    dataDependencies: { dependencies: [], rationale: 'fixed read-only response' },
    errorCodes: { codes: [], rationale: 'fixed success response' },
    callVolume: 'local verification only',
    targetService: 'Go API Gateway',
    semanticBaseline: 'gateway-growth-ping',
    rollbackSwitch: 'remove exact handler',
    owner: 'API Platform',
    ...overrides,
  };
}

test('normalizes a dynamic query call to its registered API path', () => {
  const calls = extractCalls('frontend/src/api/notifications.ts', `
    export const remove = (ids) => api.delete(\`/notifications?ids=\${ids.join(',')}\`);
  `);

  assert.deepEqual(calls, [{
    method: 'DELETE',
    path: '/notifications',
    callsite: 'frontend/src/api/notifications.ts',
  }]);
});

test('提取可静态确认的同源 fetch 和 EventSource 候选', () => {
  const source = [
    "fetch('/api/config', { credentials: 'omit' });",
    "window.fetch('/api/auth/email/send-magic-link', { method: 'POST' });",
    "globalThis.fetch('/api/v1/profile', { headers: { method: 'DELETE' }, 'method': 'PATCH' });",
    "new EventSource('/api/users/me/generations/stream?token=test-token');",
    "new EventSource(`/api/realtime/health`);",
    "fetch('/api');",
    "fetch('/api/v1/health', { headers: { method: 'POST' } });",
    "client . fetch('/api/object-method');",
    "fetch('/api/dynamic-options', requestOptions);",
    "fetch('/api/dynamic-method', { method: requestMethod });",
    "fetch(externalUrl, { method: 'POST' });",
    "new EventSource(`${baseUrl}/users/me/generations/stream?token=${token}`);",
    "fetch(`/api/users/${userId}`);",
    "const example = \"fetch('/api/in-a-string', { method: 'DELETE' })\";",
    "// fetch('/api/in-a-line-comment', { method: 'DELETE' })",
    "/* new EventSource('/api/in-a-block-comment') */",
  ].join('\n');

  const calls = extractCalls('frontend/src/network.ts', source);

  assert.deepEqual(calls, [
    {
      method: 'GET',
      path: '/config',
      callsite: 'frontend/src/network.ts',
    },
    {
      method: 'POST',
      path: '/auth/email/send-magic-link',
      callsite: 'frontend/src/network.ts',
    },
    {
      method: 'PATCH',
      path: '/v1/profile',
      callsite: 'frontend/src/network.ts',
    },
    {
      method: 'GET',
      path: '/users/me/generations/stream',
      callsite: 'frontend/src/network.ts',
    },
    {
      method: 'GET',
      path: '/realtime/health',
      callsite: 'frontend/src/network.ts',
    },
    {
      method: 'GET',
      path: '/',
      callsite: 'frontend/src/network.ts',
    },
    {
      method: 'GET',
      path: '/v1/health',
      callsite: 'frontend/src/network.ts',
    },
  ]);
});

test('忽略成员调用与动态 options 形成的原生请求歧义', () => {
  const source = [
    "client. /* 仍是对象成员调用 */ fetch('/api/object-comment-right');",
    "client.window.fetch('/api/not-global-window');",
    "client.globalThis.fetch('/api/not-global-global-this');",
    "fetch('/api/options-suffix', { method: 'POST' } && requestOptions);",
    "fetch('/api/options-member', { method: 'POST' }.method);",
    "fetch('/api/dynamic-headers', { headers: buildHeaders() });",
    "new EventSource('/api/stream' + token);",
    "new EventSource('/api/stream', options);",
    "new EventSource('/api/stream' as string);",
  ].join('\n');

  assert.deepEqual(extractCalls('frontend/src/network.ts', source), []);
});

test('ignores an unclosed escape-heavy string literal without a candidate', () => {
  const calls = extractCalls('frontend/src/api/malformed.ts', `
    api.get('${'\\'.repeat(30)}
  `);

  assert.deepEqual(calls, []);
});

test('does not let an unclosed string literal consume the next valid API call', () => {
  const source = [
    "api.get('broken\\",
    "api.get('/safe')",
  ].join('\n');

  const calls = extractCalls('probe.ts', source);

  assert.deepEqual(calls, [{
    method: 'GET',
    path: '/safe',
    callsite: 'probe.ts',
  }]);
});

test('rejects confirmed migration entries without contract and ownership evidence', () => {
  const result = validateMigrationMatrix([{
    method: 'GET',
    path: '/api/homepage/content',
    classification: '确认迁移',
    evidence: ['contract snapshot'],
    callVolume: 'low',
    targetService: 'api-gateway',
    semanticBaseline: 'node-backend',
  }]);

  assert.equal(result.valid, false);
  assert.deepEqual(result.errors[0].missing, [
    'authentication',
    'requestExample',
    'responseExample',
    'dataDependencies',
    'errorCodes',
    'rollbackSwitch',
    'owner',
  ]);
});

test('rejects confirmed migration entries without phase 0 evidence fields', () => {
  const result = validateMigrationMatrix([{
    method: 'GET',
    path: '/api/homepage/content',
    classification: '确认迁移',
    authentication: 'session',
    requestExample: { locale: 'zh-CN' },
    responseExample: { success: true },
    dataDependencies: ['homepage-content'],
    errorCodes: ['UNAUTHORIZED'],
    rollbackSwitch: 'gateway.homepageContent.enabled',
    owner: 'growth-platform',
  }]);

  assert.equal(result.valid, false);
  assert.deepEqual(result.errors[0].missing, [
    'evidence',
    'dataDependencies',
    'errorCodes',
    'callVolume',
    'targetService',
    'semanticBaseline',
  ]);
});

test('formats migration matrix validation errors as a body-free Markdown report', () => {
  assert.equal(
    typeof formatMatrixValidationReport,
    'function',
    'audit library must export formatMatrixValidationReport',
  );

  const report = formatMatrixValidationReport({
    valid: false,
    errors: [{
      method: 'GET',
      path: '/api/homepage/content',
      missing: ['evidence', 'callVolume'],
      requestExample: { secret: 'request-body-secret-marker' },
      responseExample: { secret: 'response-body-secret-marker' },
    }],
  });

  assert.equal(typeof report, 'string');
  assert.match(report, /GET \/api\/homepage\/content/);
  assert.match(report, /evidence/);
  assert.match(report, /callVolume/);
  assert.doesNotMatch(report, /request-body-secret-marker/);
  assert.doesNotMatch(report, /response-body-secret-marker/);
});

test('rejects non-canonical API namespace paths without echoing their values', () => {
  const c1ControlPath = '/api/growth\u0085ping';
  const invalidPaths = [
    '/apiary/growth/ping',
    '/api /growth',
    '/api/growth/ping?x=1',
    '/api/growth/ping#fragment',
    '/api/growth\nping',
    '/api/growth\u0000ping',
    '/api/growth\u007Fping',
    c1ControlPath,
    ' /api/growth/ping',
    '/api/growth/ping ',
  ];

  for (const path of invalidPaths) {
    const result = validateMigrationMatrix([confirmedMigration({ path })]);

    assert.equal(result.valid, false, `path ${JSON.stringify(path)} must be invalid`);
    assert.deepEqual(result.errors[0].missing, ['path']);
    assert.equal(result.errors[0].path, undefined, 'non-canonical paths must not be echoed');

    if (path === c1ControlPath) {
      const report = formatMatrixValidationReport(result);
      assert.doesNotMatch(report, /\u0085/, 'reports must not contain an invalid C1 path');
    }
  }
});

test('accepts canonical API namespace paths', () => {
  for (const path of ['/api', '/api/', '/api/growth/ping']) {
    assert.equal(validateMigrationMatrix([confirmedMigration({ path })]).valid, true);
  }
});

test('matrix CLI rejects a document without entries without reporting body markers', () => {
  withTemporaryMatrix({
    requestExample: { marker: 'missing-entries-request-marker' },
    responseExample: { marker: 'missing-entries-response-marker' },
  }, ({ result, report }) => {
    assert.equal(result.status, 1, 'missing entries must fail the matrix CLI');
    assert.match(report, /状态：未通过/);
    assert.match(report, /entries/);
    assert.doesNotMatch(report, /missing-entries-request-marker/);
    assert.doesNotMatch(report, /missing-entries-response-marker/);
  });
});

test('matrix CLI rejects a non-array entries value without reporting body markers', () => {
  withTemporaryMatrix({
    entries: null,
    requestExample: { marker: 'non-array-request-marker' },
    responseExample: { marker: 'non-array-response-marker' },
  }, ({ result, report }) => {
    assert.equal(result.status, 1, 'non-array entries must fail the matrix CLI');
    assert.match(report, /状态：未通过/);
    assert.match(report, /entries/);
    assert.doesNotMatch(report, /non-array-request-marker/);
    assert.doesNotMatch(report, /non-array-response-marker/);
  });
});

test('matrix CLI rejects a non-object entry without reporting body markers', () => {
  withTemporaryMatrix({
    entries: [null],
    requestExample: { marker: 'null-entry-request-marker' },
    responseExample: { marker: 'null-entry-response-marker' },
  }, ({ result, report }) => {
    assert.equal(result.status, 1, 'a null matrix entry must fail the matrix CLI');
    assert.match(report, /状态：未通过/);
    assert.doesNotMatch(report, /null-entry-request-marker/);
    assert.doesNotMatch(report, /null-entry-response-marker/);
  });
});

test('matrix CLI rejects forged confirmed migration fields without reporting body markers', () => {
  withTemporaryMatrix({
    entries: [{
      method: { marker: 'forged-method-marker' },
      path: '/api/growth/ping',
      classification: '确认迁移',
      evidence: ['  '],
      authentication: true,
      requestExample: true,
      responseExample: { marker: 'forged-response-body-marker' },
      dataDependencies: { dependencies: [], rationale: '' },
      errorCodes: { codes: [], rationale: '' },
      callVolume: {},
      targetService: {},
      semanticBaseline: 0,
      rollbackSwitch: 'remove exact handler',
      owner: 'API Platform',
    }],
  }, ({ result, report }) => {
    assert.equal(result.status, 1, 'forged confirmed fields must fail the matrix CLI');
    assert.match(report, /状态：未通过/);
    assert.doesNotMatch(report, /forged-method-marker/);
    assert.doesNotMatch(report, /forged-response-body-marker/);
  });
});

test('matrix CLI removes its temporary fixture after an assertion failure', () => {
  let temporaryDirectory;

  assert.throws(
    () => withTemporaryMatrix({ entries: null }, ({ temporaryDirectory: directory }) => {
      temporaryDirectory = directory;
      throw new Error('intentional assertion failure');
    }),
    /intentional assertion failure/,
  );

  assert.equal(fs.existsSync(temporaryDirectory), false);
});

test('rejects confirmed migration entries without an endpoint method or path', () => {
  const migrationEvidence = {
    classification: '确认迁移',
    evidence: ['contract snapshot'],
    authentication: 'session',
    requestExample: { locale: 'zh-CN' },
    responseExample: { success: true },
    dataDependencies: { dependencies: [], rationale: 'no persistence' },
    errorCodes: { codes: [], rationale: 'Node fallback handles errors' },
    callVolume: 'low',
    targetService: 'api-gateway',
    semanticBaseline: 'node-backend',
    rollbackSwitch: 'gateway.growthPing.enabled',
    owner: 'growth-platform',
  };

  const result = validateMigrationMatrix([
    { ...migrationEvidence, path: '/api/homepage/content' },
    { ...migrationEvidence, method: 'GET' },
  ]);

  assert.equal(result.valid, false);
  assert.deepEqual(result.errors.map((error) => error.missing), [
    ['method'],
    ['path'],
  ]);
});

test('marks an unregistered frontend API call as pending verification', () => {
  const result = classifyCall({
    method: 'GET',
    path: '/not-registered',
    frontendCallsites: ['frontend/src/api/demo.ts'],
    adminCallsites: [],
  }, ['/homepage']);

  assert.equal(result.classification, '待核实');
  assert.equal(result.routeRegistration, '');
});

test('keeps a registered v1 candidate pending until its contract evidence is complete', () => {
  const result = classifyCall({
    method: 'GET',
    path: ' /v1/homepage/content?locale=zh-CN ',
    frontendCallsites: ['frontend/src/api/homepage.ts'],
    adminCallsites: [],
  }, ['/homepage']);

  assert.deepEqual(result, {
    method: 'GET',
    path: '/v1/homepage/content',
    frontendCallsites: ['frontend/src/api/homepage.ts'],
    adminCallsites: [],
    routeRegistration: 'backend/src/bootstrap/registerApiRoutes.js',
    evidence: ['静态文本候选，待人工核验', 'route registration scan'],
    classification: '待核实',
  });
});

test('labels callsite matches as static text candidates pending manual verification', () => {
  const result = classifyCall({
    method: 'GET',
    path: '/homepage/content',
    frontendCallsites: ['frontend/src/api/homepage.ts'],
    adminCallsites: [],
  }, ['/homepage']);

  assert.deepEqual(result.evidence, [
    '静态文本候选，待人工核验',
    'route registration scan',
  ]);
  assert.equal(result.classification, '待核实');
});

test('does not treat a lookalike path as a registered route prefix', () => {
  const result = classifyCall({
    method: 'GET',
    path: '/homepages',
    frontendCallsites: ['frontend/src/api/demo.ts'],
    adminCallsites: [],
  }, ['/homepage']);

  assert.equal(result.routeRegistration, '');
  assert.equal(result.classification, '待核实');
});

test('normalizes a non-root route prefix with a trailing slash', () => {
  const result = classifyCall({
    method: 'GET',
    path: '/homepage/content',
    frontendCallsites: ['frontend/src/api/homepage.ts'],
    adminCallsites: [],
  }, ['/homepage/']);

  assert.equal(result.routeRegistration, 'backend/src/bootstrap/registerApiRoutes.js');
});

test('keeps a registered route scan without callsites pending verification', () => {
  const result = classifyCall({
    method: 'GET',
    path: '/homepage',
    frontendCallsites: [],
    adminCallsites: [],
  }, ['/homepage']);

  assert.equal(result.routeRegistration, 'backend/src/bootstrap/registerApiRoutes.js');
  assert.deepEqual(result.evidence, ['route registration scan']);
  assert.equal(result.classification, '待核实');
});

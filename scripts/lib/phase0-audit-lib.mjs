const HTTP_METHODS = '(get|post|put|patch|delete)';
const HTTP_CLIENT_CALL = new RegExp(
  String.raw`\b(?:api|client)\s*\.\s*${HTTP_METHODS}\s*(?:<[^>]*>)?\s*\(\s*(?:'((?:\\[^\r\n]|[^'\\\r\n])*)'|"((?:\\[^\r\n]|[^"\\\r\n])*)"|\`((?:\\[^\r\n]|[^\`\\\r\n])*)\`)`,
  'gs',
);
const VALID_MIGRATION_CLASSIFICATIONS = new Set([
  '确认迁移',
  '待核实',
  '候选淘汰',
  '已确认废弃',
]);
const STANDARD_HTTP_METHODS = new Set([
  'GET',
  'POST',
  'PUT',
  'PATCH',
  'DELETE',
  'HEAD',
  'OPTIONS',
]);
const CONFIRMED_MIGRATION_FIELDS = [
  'evidence',
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

/**
 * 路由比较前移除查询参数，因为接口注册只以 path 为键，不依赖某次请求携带的 query。
 */
export function normalizeApiPath(rawPath) {
  const withoutQuery = String(rawPath ?? '').trim().split('?', 1)[0].trim();
  return withoutQuery.startsWith('/') ? withoutQuery : `/${withoutQuery}`;
}

function normalizeTemplateExpressions(route) {
  return route
    .replace(/\$\{[^}]*\}/g, ':param')
    .replace(/\$\{.*$/, ':param');
}

function normalizeSameOriginApiPath(route) {
  const normalized = normalizeApiPath(route);
  if (normalized === '/api') return '/';
  if (!normalized.startsWith('/api/')) return null;

  return normalizeApiPath(normalized.slice('/api'.length));
}

function isIdentifierStart(character) {
  return /[A-Za-z_$]/.test(character ?? '');
}

function isIdentifierPart(character) {
  return /[A-Za-z0-9_$]/.test(character ?? '');
}

function readIdentifier(source, index) {
  if (!isIdentifierStart(source[index])) return null;

  let cursor = index + 1;
  while (isIdentifierPart(source[cursor])) cursor += 1;
  return { value: source.slice(index, cursor), end: cursor };
}

function skipWhitespaceAndComments(source, index) {
  let cursor = index;
  while (cursor < source.length) {
    if (/\s/.test(source[cursor])) {
      cursor += 1;
      continue;
    }
    if (source.startsWith('//', cursor)) {
      const newline = source.indexOf('\n', cursor + 2);
      cursor = newline === -1 ? source.length : newline + 1;
      continue;
    }
    if (source.startsWith('/*', cursor)) {
      const commentEnd = source.indexOf('*/', cursor + 2);
      cursor = commentEnd === -1 ? source.length : commentEnd + 2;
      continue;
    }
    break;
  }
  return cursor;
}

function readStaticStringLiteral(source, index) {
  const quote = source[index];
  if (quote !== "'" && quote !== '"' && quote !== String.fromCharCode(96)) return null;

  let value = '';
  let hasTemplateExpression = false;
  for (let cursor = index + 1; cursor < source.length; cursor += 1) {
    const character = source[cursor];
    if (character === '\\') {
      const escaped = source[cursor + 1];
      if (escaped == null) return { value: null, end: source.length };
      if (!hasTemplateExpression) value += escaped;
      cursor += 1;
      continue;
    }
    if (character === quote) {
      return { value: hasTemplateExpression ? null : value, end: cursor + 1 };
    }
    if (quote !== String.fromCharCode(96) && (character === '\n' || character === '\r')) {
      return { value: null, end: cursor };
    }
    if (quote === String.fromCharCode(96) && character === '$' && source[cursor + 1] === '{') {
      hasTemplateExpression = true;
    }
    if (!hasTemplateExpression) value += character;
  }
  return { value: null, end: source.length };
}

function readLexicalObjectLiteral(source, index) {
  if (source[index] !== '{') return null;

  let depth = 0;
  let cursor = index;
  while (cursor < source.length) {
    const next = skipWhitespaceAndComments(source, cursor);
    if (next !== cursor) {
      cursor = next;
      continue;
    }
    const literal = readStaticStringLiteral(source, cursor);
    if (literal != null) {
      cursor = literal.end;
      continue;
    }
    if (source[cursor] === '{') depth += 1;
    if (source[cursor] === '}') {
      depth -= 1;
      if (depth === 0) return { start: index, end: cursor + 1 };
    }
    cursor += 1;
  }
  return null;
}

function readLexicalObjectKey(source, index) {
  const literal = readStaticStringLiteral(source, index);
  if (literal != null) return literal.value == null ? null : literal;
  return readIdentifier(source, index);
}

function readStaticLexicalArrayLiteral(source, index) {
  if (source[index] !== '[') return null;

  let cursor = index + 1;
  while (cursor < source.length) {
    cursor = skipWhitespaceAndComments(source, cursor);
    if (source[cursor] === ']') return { end: cursor + 1 };

    const value = readStaticLexicalValue(source, cursor);
    if (value == null) return null;
    cursor = skipWhitespaceAndComments(source, value.end);
    if (source[cursor] === ',') {
      cursor += 1;
      continue;
    }
    if (source[cursor] === ']') return { end: cursor + 1 };
    return null;
  }
  return null;
}

function readStaticLexicalObjectProperties(source, object, onProperty) {
  let cursor = object.start + 1;

  while (cursor < object.end) {
    cursor = skipWhitespaceAndComments(source, cursor);
    if (source[cursor] === '}') return { end: cursor + 1 };
    // 展开与计算属性可能在运行时覆盖 method，静态审计不能把它们误判为确定配置。
    if (source.startsWith('...', cursor) || source[cursor] === '[') return null;

    const key = readLexicalObjectKey(source, cursor);
    if (key == null) return null;
    cursor = skipWhitespaceAndComments(source, key.end);
    if (source[cursor] !== ':') return null;
    cursor = skipWhitespaceAndComments(source, cursor + 1);

    const value = readStaticLexicalValue(source, cursor);
    if (value == null || onProperty?.(key.value, value) === false) return null;
    cursor = value.end;

    cursor = skipWhitespaceAndComments(source, cursor);
    if (source[cursor] === ',') {
      cursor += 1;
      continue;
    }
    if (source[cursor] === '}') return { end: cursor + 1 };
    return null;
  }
  return null;
}

function readStaticLexicalObjectLiteral(source, index) {
  const object = readLexicalObjectLiteral(source, index);
  if (object == null) return null;
  return readStaticLexicalObjectProperties(source, object);
}

/**
 * 仅接受不依赖运行时求值的表达式，避免把函数调用、变量、展开或成员访问误当成静态 RequestInit。
 * 返回结果的 value 只在顶层 method 解析时使用；其他静态值只需确认其边界。
 */
function readStaticLexicalValue(source, index) {
  const literal = readStaticStringLiteral(source, index);
  if (literal != null) {
    return literal.value == null ? null : { end: literal.end, value: literal.value };
  }
  if (source[index] === '{') return readStaticLexicalObjectLiteral(source, index);
  if (source[index] === '[') return readStaticLexicalArrayLiteral(source, index);

  const number = /-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;
  number.lastIndex = index;
  const numberMatch = number.exec(source);
  if (numberMatch != null) return { end: number.lastIndex };

  const identifier = readIdentifier(source, index);
  if (identifier != null && ['true', 'false', 'null'].includes(identifier.value)) {
    return { end: identifier.end };
  }
  return null;
}

function readLexicalFetchMethod(source, object) {
  let method = 'GET';
  const parsed = readStaticLexicalObjectProperties(source, object, (key, value) => {
    if (key !== 'method') return true;
    if (typeof value.value !== 'string') return false;

    const normalizedMethod = value.value.toUpperCase();
    if (!STANDARD_HTTP_METHODS.has(normalizedMethod)) return false;
    method = normalizedMethod;
    return true;
  });

  return parsed == null ? null : { ...parsed, method };
}

function lexicalFetchMethodAfterUrlLiteral(source, index) {
  let cursor = skipWhitespaceAndComments(source, index);
  if (source[cursor] === ')') return 'GET';
  if (source[cursor] !== ',') return null;

  cursor = skipWhitespaceAndComments(source, cursor + 1);
  const options = readLexicalObjectLiteral(source, cursor);
  if (options == null) return null;
  const parsed = readLexicalFetchMethod(source, options);
  if (parsed == null) return null;

  // 静态对象必须完整作为 fetch 的第二个参数；对象后继续计算会改变实际 RequestInit。
  return source[skipWhitespaceAndComments(source, parsed.end)] === ')' ? parsed.method : null;
}

function lexicalEventSourceMethodAfterUrlLiteral(source, index) {
  // 审计只接受单一静态 URL 参数；拼接表达式或 options 会使连接语义依赖运行时配置。
  return source[skipWhitespaceAndComments(source, index)] === ')' ? 'GET' : null;
}

function readLexicalRawBrowserCall(source, calleeEnd, methodResolver) {
  let cursor = skipWhitespaceAndComments(source, calleeEnd);
  if (source[cursor] !== '(') return { end: calleeEnd, call: null };

  cursor = skipWhitespaceAndComments(source, cursor + 1);
  const url = readStaticStringLiteral(source, cursor);
  if (url == null) return { end: cursor, call: null };

  const path = url.value == null ? null : normalizeSameOriginApiPath(url.value);
  const method = methodResolver(source, url.end);
  return {
    end: url.end,
    call: path == null || method == null ? null : { method, path },
  };
}

function skipWhitespaceAndCommentsBackward(source, index) {
  let cursor = index;
  while (cursor >= 0) {
    if (/\s/.test(source[cursor])) {
      cursor -= 1;
      continue;
    }
    if (source[cursor] === '/' && source[cursor - 1] === '*') {
      const commentStart = source.lastIndexOf('/*', cursor - 1);
      if (commentStart === -1) break;
      cursor = commentStart - 1;
      continue;
    }
    const lineStart = Math.max(source.lastIndexOf('\n', cursor), source.lastIndexOf('\r', cursor)) + 1;
    const lineCommentStart = source.lastIndexOf('//', cursor);
    if (lineCommentStart >= lineStart) {
      cursor = lineCommentStart - 1;
      continue;
    }
    break;
  }
  return cursor;
}

function hasPropertyAccessBefore(source, index) {
  const cursor = skipWhitespaceAndCommentsBackward(source, index - 1);
  return source[cursor] === '.';
}

function extractLexicalRawBrowserCalls(callsite, source) {
  const calls = [];
  let cursor = 0;

  while (cursor < source.length) {
    const next = skipWhitespaceAndComments(source, cursor);
    if (next !== cursor) {
      cursor = next;
      continue;
    }
    const literal = readStaticStringLiteral(source, cursor);
    if (literal != null) {
      cursor = literal.end;
      continue;
    }
    const identifier = readIdentifier(source, cursor);
    if (identifier == null) {
      cursor += 1;
      continue;
    }

    let parsed = null;
    if (identifier.value === 'fetch' && !hasPropertyAccessBefore(source, cursor)) {
      parsed = readLexicalRawBrowserCall(source, identifier.end, lexicalFetchMethodAfterUrlLiteral);
    } else if (
      (identifier.value === 'window' || identifier.value === 'globalThis')
      && !hasPropertyAccessBefore(source, cursor)
    ) {
      const dot = skipWhitespaceAndComments(source, identifier.end);
      if (source[dot] === '.') {
        const member = readIdentifier(source, skipWhitespaceAndComments(source, dot + 1));
        if (member?.value === 'fetch') {
          parsed = readLexicalRawBrowserCall(source, member.end, lexicalFetchMethodAfterUrlLiteral);
        }
      }
    } else if (identifier.value === 'new') {
      const constructor = readIdentifier(source, skipWhitespaceAndComments(source, identifier.end));
      if (constructor?.value === 'EventSource') {
        parsed = readLexicalRawBrowserCall(
          source,
          constructor.end,
          lexicalEventSourceMethodAfterUrlLiteral,
        );
      }
    }

    if (parsed != null) {
      if (parsed.call != null) calls.push({ ...parsed.call, callsite });
      cursor = Math.max(parsed.end, identifier.end);
      continue;
    }
    cursor = identifier.end;
  }

  return calls;
}

/**
 * 对文字源码进行静态候选提取：只接受可完整确定的字面量与对象结构，不执行或评估应用代码。
 * 动态 URL 或动态 method 一律不猜测；提取结果仍需路由、测试或观测证据确认，不能单独作为迁移结论。
 */
export function extractCalls(callsite, source) {
  const sourceText = String(source ?? '');
  const calls = [];

  for (const match of sourceText.matchAll(HTTP_CLIENT_CALL)) {
    const route = match[2] ?? match[3] ?? match[4] ?? '';
    calls.push({
      method: match[1].toUpperCase(),
      path: normalizeApiPath(normalizeTemplateExpressions(route)),
      callsite,
    });
  }

  // 原生浏览器调用使用轻量词法扫描而非宽泛正则：只接受代码区内、同源 /api 下的静态
  // URL 与确定 method。动态变量、外部地址和 Request 对象继续留给人工核验。
  calls.push(...extractLexicalRawBrowserCalls(callsite, sourceText));

  return calls;
}

function normalizeRoutePrefix(prefix) {
  const normalized = normalizeApiPath(prefix);
  return normalized === '/' ? normalized : normalized.replace(/\/+$/, '');
}

function isRegisteredRoute(candidatePath, routePrefixes) {
  // 浏览器兼容调用可能保留 `/v1`，而 Node 在 `/` 下注册相同接口；只能在注册命名空间内比较。
  const registrationPath = normalizeApiPath(candidatePath).replace(/^\/v1(?=\/)/, '');

  return (routePrefixes ?? [])
    .map((prefix) => normalizeRoutePrefix(prefix))
    .some((prefix) => registrationPath === prefix || registrationPath.startsWith(`${prefix}/`));
}

/**
 * 将静态调用候选与路由注册扫描合并为审计条目。
 * 静态证据无法证明鉴权、数据依赖、错误语义或业务所有者，因此任何结果都只能待核实，
 * 不能直接成为 Go 分流或迁移的依据。
 */
export function classifyCall(call, routePrefixes) {
  const path = normalizeApiPath(call?.path);
  const frontendCallsites = [...(call?.frontendCallsites ?? [])];
  const adminCallsites = [...(call?.adminCallsites ?? [])];
  const registered = isRegisteredRoute(path, routePrefixes);
  const hasCallsiteEvidence = frontendCallsites.length > 0 || adminCallsites.length > 0;

  return {
    method: call?.method,
    path,
    frontendCallsites,
    adminCallsites,
    routeRegistration: registered ? 'backend/src/bootstrap/registerApiRoutes.js' : '',
    evidence: [
      ...(hasCallsiteEvidence ? ['静态文本候选，待人工核验'] : []),
      ...(registered ? ['route registration scan'] : []),
    ],
    classification: '待核实',
  };
}

function isPlainObject(value) {
  if (value == null || typeof value !== 'object' || Array.isArray(value)) return false;

  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function isNonEmptyString(value) {
  return typeof value === 'string' && value.trim() !== '';
}

function isNonEmptyStringArray(value) {
  return Array.isArray(value) && value.length > 0 && value.every(isNonEmptyString);
}

function isValidHttpMethod(value) {
  return typeof value === 'string' && STANDARD_HTTP_METHODS.has(value);
}

function isApiPath(value) {
  // 矩阵键必须是规范化路由路径，而非类似 URL 的输入。严格限制该边界，可防止相似命名空间
  // 或请求修饰信息被误记录为迁移端点。
  return typeof value === 'string'
    && value === value.trim()
    && !/[\s\u0000-\u001F\u007F-\u009F]/.test(value)
    && !value.includes('?')
    && !value.includes('#')
    && (value === '/api' || value.startsWith('/api/'));
}

function isNonEmptyPlainObject(value) {
  return isPlainObject(value) && Object.keys(value).length > 0;
}

function hasDependencyDescriptor(value, arrayField) {
  return isPlainObject(value)
    && Array.isArray(value[arrayField])
    && isNonEmptyString(value.rationale);
}

function isValidConfirmedMigrationField(entry, field) {
  switch (field) {
    case 'evidence':
      return isNonEmptyStringArray(entry.evidence);
    case 'authentication':
    case 'callVolume':
    case 'targetService':
    case 'semanticBaseline':
    case 'rollbackSwitch':
    case 'owner':
      return isNonEmptyString(entry[field]);
    case 'requestExample':
    case 'responseExample':
      return isNonEmptyPlainObject(entry[field]);
    case 'dataDependencies':
      return hasDependencyDescriptor(entry.dataDependencies, 'dependencies');
    case 'errorCodes':
      return hasDependencyDescriptor(entry.errorCodes, 'codes');
    default:
      return false;
  }
}

function safeValidationEndpoint(entry) {
  return {
    method: isValidHttpMethod(entry?.method) ? entry.method : undefined,
    path: isApiPath(entry?.path) ? entry.path : undefined,
  };
}

export function validateMigrationMatrix(entries) {
  if (!Array.isArray(entries)) {
    return {
      valid: false,
      errors: [{
        method: 'MATRIX',
        path: 'api-migration-matrix.json',
        missing: ['entries（必须为数组）'],
      }],
    };
  }

  const errors = [];

  for (const [index, entry] of entries.entries()) {
    if (!isPlainObject(entry)) {
      errors.push({
        method: undefined,
        path: undefined,
        missing: [`entries[${index}]（必须为对象）`],
      });
      continue;
    }

    const missing = [];
    if (!VALID_MIGRATION_CLASSIFICATIONS.has(entry.classification)) missing.push('classification');
    if (!isValidHttpMethod(entry.method)) missing.push('method');
    if (!isApiPath(entry.path)) missing.push('path');

    if (entry.classification === '确认迁移') {
      missing.push(...CONFIRMED_MIGRATION_FIELDS.filter((field) => (
        !isValidConfirmedMigrationField(entry, field)
      )));
    }

    if (missing.length > 0) {
      errors.push({
        ...safeValidationEndpoint(entry),
        missing,
      });
    }
  }

  return { valid: errors.length === 0, errors };
}

/**
 * 校验矩阵文档的结构后再校验“确认迁移”条目。结构错误复用 method/path/missing
 * 错误形状，以便报告生成器无需读取或序列化矩阵中的请求、响应等任意正文。
 */
export function validateMigrationMatrixDocument(matrix) {
  if (matrix == null || typeof matrix !== 'object' || Array.isArray(matrix)) {
    return {
      valid: false,
      errors: [{
        method: 'MATRIX',
        path: 'api-migration-matrix.json',
        missing: ['顶层对象（必须为对象）'],
      }],
    };
  }

  if (!Array.isArray(matrix.entries)) {
    return {
      valid: false,
      errors: [{
        method: 'MATRIX',
        path: 'api-migration-matrix.json',
        missing: ['entries（必须为数组）'],
      }],
    };
  }

  return validateMigrationMatrix(matrix.entries);
}

function formatReportValue(value, fallback) {
  const normalized = String(value ?? '').trim().replace(/[\r\n]+/g, ' ');
  return (normalized || fallback).replace(/`/g, '\\`').replace(/\|/g, '\\|');
}

/**
 * 生成可提交的矩阵校验摘要。报告刻意只读取校验结果中的端点和缺失字段，
 * 避免把请求、响应或校验器未来附带的任意正文写进审计文档。
 */
export function formatMatrixValidationReport(validation) {
  if (validation?.valid === true) {
    return [
      '# API 迁移矩阵校验报告',
      '',
      '状态：通过',
      '',
      '所有“确认迁移”条目均已提供阶段 0 所需的放行字段。',
      '',
    ].join('\n');
  }

  const errors = (Array.isArray(validation?.errors) ? validation.errors : [])
    .map(({ method, path, missing }) => ({
      method: formatReportValue(method, '缺少 method'),
      path: formatReportValue(path, '缺少 path'),
      missing: (Array.isArray(missing) ? missing : [])
        .map((field) => formatReportValue(field, '未命名字段'))
        .sort((left, right) => left.localeCompare(right)),
    }))
    .sort((left, right) => (
      `${left.method} ${left.path}`.localeCompare(`${right.method} ${right.path}`)
    ));

  const rows = errors.map(({ method, path, missing }) => (
    `| \`${method} ${path}\` | ${missing.map((field) => `\`${field}\``).join('、') || '未提供'} |`
  ));

  return [
    '# API 迁移矩阵校验报告',
    '',
    '状态：未通过',
    '',
    '以下“确认迁移”条目或矩阵结构缺少阶段 0 放行字段：',
    '',
    '| Endpoint | 缺失字段或结构错误 |',
    '| --- | --- |',
    ...rows,
    '',
  ].join('\n');
}

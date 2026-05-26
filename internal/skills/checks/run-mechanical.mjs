#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

function parseArgs() {
  const args = process.argv.slice(2);
  let project = process.cwd();
  let root = '';
  let format = 'human';
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === '--project') project = path.resolve(args[++i]);
    else if (a === '--root') root = path.resolve(args[++i]);
    else if (a === '--format') format = args[++i];
    else if (a === '-h' || a === '--help') {
      process.stdout.write(
        'Usage: run-mechanical.mjs [--project <path>] [--root <path>] [--format human|json]\n' +
          '\n' +
          'Runs all [M] mechanical critique checks against a Missions project.\n' +
          '--root sets the project root for CLAUDE.md (defaults to --project).\n' +
          'Exit code: 0 = all pass, 1 = at least one fail.\n'
      );
      process.exit(0);
    }
  }
  if (!root) root = project;
  return { project, root, format };
}

const results = [];

function record(id, category, status, message, opts = {}) {
  results.push({ id, category, status, message, ...opts });
}

function readFileSafe(p) {
  try {
    return fs.readFileSync(p, 'utf8');
  } catch {
    return null;
  }
}

function parseValidationContract(content) {
  const assertions = [];
  const lines = content.split('\n');
  let currentCategory = null;
  const assertionRe = /^\s*[-*]\s+\*\*(.+?)\*\*/;
  const headingRe = /^##\s+(.+)$/;
  lines.forEach((line, idx) => {
    const h = line.match(headingRe);
    if (h) {
      currentCategory = h[1].trim();
      return;
    }
    const a = line.match(assertionRe);
    if (a) {
      let id = a[1].trim();
      const colonIdx = id.indexOf(':');
      if (colonIdx > 0) id = id.substring(0, colonIdx).trim();
      assertions.push({ id, line: idx + 1, category: currentCategory });
    }
  });
  return assertions;
}

function checkSpec(project) {
  const contractPath = path.join(project, 'mission/validation-contract.md');
  const content = readFileSafe(contractPath);
  if (content === null) {
    record('M-S0', 'spec', 'fail', `mission/validation-contract.md not found at ${contractPath}`);
    return { assertions: [] };
  }

  const assertions = parseValidationContract(content);
  const idRe = /^[a-z][a-z0-9_]*\.\d+$/;

  const badIds = assertions.filter(a => !idRe.test(a.id));
  if (badIds.length === 0) {
    record('M-S1', 'spec', 'pass', `${assertions.length} assertion ID(s) all match ^[a-z][a-z0-9_]*\\.\\d+$`);
  } else {
    record(
      'M-S1',
      'spec',
      'fail',
      `${badIds.length} assertion ID(s) violate pattern: ${badIds.map(a => `${a.id} (line ${a.line})`).join(', ')}`
    );
  }

  const orphaned = assertions.filter(a => a.category === null);
  if (orphaned.length === 0) {
    record('M-S2', 'spec', 'pass', `all ${assertions.length} assertion(s) appear under a ## category heading`);
  } else {
    record(
      'M-S2',
      'spec',
      'fail',
      `${orphaned.length} assertion(s) appear before any ## heading: ${orphaned.map(a => `${a.id} (line ${a.line})`).join(', ')}`
    );
  }

  const markers = ['[TODO]', '[CONFIRM]', '[FILL]'];
  const found = [];
  content.split('\n').forEach((line, idx) => {
    for (const m of markers) {
      if (line.includes(m)) found.push({ marker: m, line: idx + 1 });
    }
  });
  if (found.length === 0) {
    record('M-S3', 'spec', 'pass', 'no [TODO]/[CONFIRM]/[FILL] draft markers remain');
  } else {
    const head = found.slice(0, 5).map(f => `${f.marker} at line ${f.line}`).join(', ');
    record('M-S3', 'spec', 'fail', `${found.length} draft marker(s) remain: ${head}${found.length > 5 ? ', …' : ''}`);
  }

  const seen = new Map();
  const dups = [];
  for (const a of assertions) {
    if (seen.has(a.id)) dups.push({ id: a.id, lines: [seen.get(a.id), a.line] });
    else seen.set(a.id, a.line);
  }
  if (dups.length === 0) {
    record('M-S4', 'spec', 'pass', 'no duplicate assertion IDs');
  } else {
    record(
      'M-S4',
      'spec',
      'fail',
      `${dups.length} duplicate ID(s): ${dups.map(d => `${d.id} (lines ${d.lines.join(', ')})`).join('; ')}`
    );
  }

  const categories = [...new Set(assertions.map(a => (a.category || '').toLowerCase()))].filter(Boolean);
  const hasFailure = categories.some(c => /fail|error|edge/.test(c));
  const hasSetup = categories.some(c => /setup|infra/.test(c));
  if (assertions.length === 0) {
    record('M-S6', 'spec', 'pass', '(no assertions in contract; vacuously pass — will re-check once features start adding assertions)');
  } else if (categories.length >= 3 && hasFailure) {
    record(
      'M-S6',
      'spec',
      'pass',
      `${categories.length} distinct categories, including failure/error/edge coverage${hasSetup ? ' and setup/infra' : ''}`
    );
  } else {
    const missing = [];
    if (categories.length < 3) missing.push(`only ${categories.length} distinct categor(y/ies), need ≥3`);
    if (!hasFailure) missing.push('no category name matching /fail|error|edge/');
    if (!hasSetup) missing.push('(advisory: no setup/infra category)');
    record('M-S6', 'spec', 'fail', `categories incomplete: ${missing.join('; ')}`);
  }

  return { assertions };
}

function parseSections(content) {
  const lines = content.split('\n');
  const sections = [];
  lines.forEach((line, idx) => {
    const m = line.match(/^(#{1,6})\s+(.+?)\s*$/);
    if (m) sections.push({ level: m[1].length, title: m[2], line: idx + 1 });
  });
  sections.forEach((s, i) => {
    s.bodyStart = s.line;
    s.bodyEnd = i + 1 < sections.length ? sections[i + 1].line - 1 : lines.length;
  });
  return { sections, lines };
}

function checkArchitecture(project) {
  const claudePath = path.join(project, 'CLAUDE.md');
  const content = readFileSafe(claudePath);
  if (content === null) {
    record('M-A0', 'architecture', 'fail', `CLAUDE.md not found at ${claudePath}`);
    return;
  }

  const { sections, lines } = parseSections(content);
  const findSection = re => sections.find(s => re.test(s.title));
  const bodyOf = section => (section ? lines.slice(section.bodyStart, section.bodyEnd).join('\n') : '');

  const archSection = findSection(/^architecture\b/i);
  if (archSection) {
    record('M-A1', 'architecture', 'pass', `found "${archSection.title}" at line ${archSection.line}`);
  } else {
    record(
      'M-A1',
      'architecture',
      'fail',
      'no Architecture section found (expected `## Architecture` or `## Architecture / Stack`)'
    );
  }

  if (archSection) {
    const body = bodyOf(archSection);
    const hasBullets = /^\s*[-*]\s+/m.test(body);
    const hasSubsections = sections.some(
      s => s.line > archSection.line && s.line <= archSection.bodyEnd && s.level > archSection.level
    );
    if (hasBullets || hasSubsections) {
      record('M-A2', 'architecture', 'pass', 'Architecture section has component listings (bullets or subsections)');
    } else {
      record(
        'M-A2',
        'architecture',
        'fail',
        'Architecture section has no bullets or subsections enumerating components'
      );
    }
  } else {
    record('M-A2', 'architecture', 'fail', '(skipped: no Architecture section)');
  }

  const depSection = findSection(/depend|external\s+services?|integrations?|stack/i);
  if (depSection) {
    const body = bodyOf(depSection);
    if (body.trim() && /^\s*[-*]\s+/m.test(body)) {
      record('M-A3', 'architecture', 'pass', `found "${depSection.title}" with bullet entries`);
    } else {
      record('M-A3', 'architecture', 'fail', `"${depSection.title}" exists but has no bullet entries`);
    }
  } else {
    record(
      'M-A3',
      'architecture',
      'fail',
      'no External Dependencies / Integrations section found (databases, queues, APIs, LLM providers)'
    );
  }

  const rulesSection = findSection(/inviolable|critical\s+rule|hard\s+rule|^rules?\b/i);
  if (rulesSection) {
    const body = bodyOf(rulesSection);
    const substantive = body
      .split('\n')
      .some(l => l.trim() && !l.trim().startsWith('[TODO') && !l.trim().startsWith('['));
    if (substantive) {
      record('M-A4', 'architecture', 'pass', `found "${rulesSection.title}" with substantive content`);
    } else {
      record('M-A4', 'architecture', 'fail', `"${rulesSection.title}" is empty or only placeholders`);
    }
  } else {
    record('M-A4', 'architecture', 'fail', 'no Inviolable Rules / Critical Rules section found');
  }
}

function findCycles(graph) {
  const cycles = [];
  const seen = new Set();
  const visiting = new Set();
  const visited = new Set();

  function dfs(node, stack) {
    if (visiting.has(node)) {
      const idx = stack.indexOf(node);
      if (idx >= 0) {
        const cycle = stack.slice(idx).concat(node);
        const key = [...cycle].sort().join('|');
        if (!seen.has(key)) {
          seen.add(key);
          cycles.push(cycle);
        }
      }
      return;
    }
    if (visited.has(node)) return;
    visiting.add(node);
    for (const next of graph.get(node) || []) dfs(next, stack.concat(node));
    visiting.delete(node);
    visited.add(node);
  }

  for (const node of graph.keys()) dfs(node, []);
  return cycles;
}

function checkDecomposition(project, assertions) {
  const featuresPath = path.join(project, 'mission/features.json');
  const content = readFileSafe(featuresPath);
  if (content === null) {
    record('M-D0', 'decomposition', 'fail', `mission/features.json not found at ${featuresPath}`);
    return;
  }

  let data;
  try {
    data = JSON.parse(content);
  } catch (e) {
    record('M-D0', 'decomposition', 'fail', `mission/features.json is not valid JSON: ${e.message}`);
    return;
  }

  const features = Array.isArray(data.features) ? data.features : [];
  const fixFeatures = Array.isArray(data.fix_features) ? data.fix_features : [];
  const allFeatures = features.concat(fixFeatures);
  const lifecycle = Array.isArray(data.status_lifecycle) ? data.status_lifecycle : [];
  const required = ['id', 'title', 'phase', 'status', 'depends_on', 'scope', 'validation_refs'];

  const missingFields = [];
  allFeatures.forEach(f => {
    const missing = required.filter(k => !(k in f));
    if (missing.length) missingFields.push({ id: f.id || '(no id)', missing });
  });
  if (missingFields.length === 0) {
    record(
      'M-D1',
      'decomposition',
      'pass',
      `all ${allFeatures.length} feature(s) across features+fix_features have required fields`
    );
  } else {
    record(
      'M-D1',
      'decomposition',
      'fail',
      `${missingFields.length} feature(s) missing fields: ${missingFields
        .slice(0, 5)
        .map(m => `${m.id} → [${m.missing.join(', ')}]`)
        .join('; ')}`
    );
  }

  const rootIdRe = /^F\d+[a-z]?$/;
  const fixIdRe = /^F\d+[a-z]?(?:-fix-\d+)+$/;
  const badRootIDs = features.filter(f => !rootIdRe.test(f.id || ''));
  const badFixIDs = fixFeatures.filter(f => !fixIdRe.test(f.id || ''));
  if (badRootIDs.length === 0 && badFixIDs.length === 0) {
    record(
      'M-D2',
      'decomposition',
      'pass',
      'all IDs match expected patterns (features: ^F\\d+[a-z]?$; fix_features: ^F\\d+[a-z]?(?:-fix-\\d+)+$)'
    );
  } else {
    const issues = [];
    if (badRootIDs.length > 0) {
      issues.push(`root IDs: ${badRootIDs.map(f => f.id || '(empty)').join(', ')}`);
    }
    if (badFixIDs.length > 0) {
      issues.push(`fix IDs: ${badFixIDs.map(f => f.id || '(empty)').join(', ')}`);
    }
    record(
      'M-D2',
      'decomposition',
      'fail',
      `ID pattern violation(s): ${issues.join('; ')}`
    );
  }

  const duplicateIds = [];
  const seenIds = new Map();
  allFeatures.forEach(f => {
    const id = f.id || '(empty)';
    if (!seenIds.has(id)) {
      seenIds.set(id, 1);
      return;
    }
    seenIds.set(id, seenIds.get(id) + 1);
  });
  for (const [id, count] of seenIds.entries()) {
    if (count > 1) duplicateIds.push(`${id} (x${count})`);
  }
  if (duplicateIds.length === 0) {
    record('M-D7', 'decomposition', 'pass', 'feature IDs are globally unique across features+fix_features');
  } else {
    record('M-D7', 'decomposition', 'fail', `duplicate IDs across features+fix_features: ${duplicateIds.join(', ')}`);
  }

  const ids = new Set(allFeatures.map(f => f.id));
  const dangling = [];
  allFeatures.forEach(f => {
    (f.depends_on || []).forEach(d => {
      if (!ids.has(d)) dangling.push({ feature: f.id, missing: d });
    });
  });
  if (dangling.length === 0) {
    record('M-D3', 'decomposition', 'pass', 'all depends_on entries reference existing features');
  } else {
    record(
      'M-D3',
      'decomposition',
      'fail',
      `${dangling.length} dangling dependenc(y/ies): ${dangling.map(d => `${d.feature} → ${d.missing}`).join('; ')}`
    );
  }

  const graph = new Map(allFeatures.map(f => [f.id, f.depends_on || []]));
  const cycles = findCycles(graph);
  if (cycles.length === 0) {
    record('M-D4', 'decomposition', 'pass', 'dependency graph is acyclic');
  } else {
    record(
      'M-D4',
      'decomposition',
      'fail',
      `${cycles.length} cycle(s) detected: ${cycles.slice(0, 3).map(c => c.join(' → ')).join('; ')}`
    );
  }

  const assertionIds = new Set(assertions.map(a => a.id));
  const danglingRefs = [];
  allFeatures.forEach(f => {
    (f.validation_refs || []).forEach(r => {
      if (!assertionIds.has(r)) danglingRefs.push({ feature: f.id, ref: r });
    });
  });
  if (danglingRefs.length === 0) {
    const msg =
      assertions.length > 0
        ? 'all validation_refs resolve to assertions in validation-contract.md'
        : '(no assertions in contract; vacuously pass)';
    record('M-D5', 'decomposition', 'pass', msg);
  } else {
    record(
      'M-D5',
      'decomposition',
      'fail',
      `${danglingRefs.length} dangling validation_ref(s): ${danglingRefs
        .map(d => `${d.feature} → ${d.ref}`)
        .join('; ')}`
    );
  }

  if (lifecycle.length === 0) {
    record('M-D6', 'decomposition', 'fail', 'status_lifecycle is empty or missing');
  } else {
    const bad = allFeatures.filter(f => !lifecycle.includes(f.status));
    if (bad.length === 0) {
      record(
        'M-D6',
        'decomposition',
        'pass',
        `all statuses across features+fix_features are within status_lifecycle [${lifecycle.join(', ')}]`
      );
    } else {
      record(
        'M-D6',
        'decomposition',
        'fail',
        `${bad.length} feature(s) with status outside lifecycle: ${bad.map(f => `${f.id}=${f.status}`).join(', ')}`
      );
    }
  }
}

function render(results, format) {
  const passed = results.filter(r => r.status === 'pass').length;
  const failed = results.filter(r => r.status === 'fail').length;

  if (format === 'json') {
    process.stdout.write(
      JSON.stringify({ summary: { total: results.length, passed, failed }, results }, null, 2) + '\n'
    );
    return;
  }

  const byCategory = {};
  for (const r of results) (byCategory[r.category] ||= []).push(r);
  const order = ['spec', 'architecture', 'decomposition'];
  const sortedCats = [...new Set([...order.filter(c => byCategory[c]), ...Object.keys(byCategory)])];

  for (const cat of sortedCats) {
    process.stdout.write(`\n[${cat.toUpperCase()}]\n`);
    for (const r of byCategory[cat]) {
      process.stdout.write(`  ${r.status === 'pass' ? 'PASS' : 'FAIL'}  ${r.id}: ${r.message}\n`);
    }
  }
  process.stdout.write(`\nTotal: ${passed} pass, ${failed} fail (${results.length} checks)\n`);
}

function main() {
  const { project, root, format } = parseArgs();
  const { assertions } = checkSpec(project);
  checkArchitecture(root);
  checkDecomposition(project, assertions);
  render(results, format);
  process.exit(results.some(r => r.status === 'fail') ? 1 : 0);
}

main();

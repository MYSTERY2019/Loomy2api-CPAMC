const fs = require('fs');
const raw = fs.readFileSync(process.argv[2], 'utf8');
const d = JSON.parse(raw);
const rows = (d.data || []).map((m) => ({
  id: m.id,
  name: m.name || '',
  ctx: m.context_length || 0,
  tools: !!(m.capabilities && m.capabilities.function_calling),
  vision: !!(m.capabilities && m.capabilities.vision)
}));
console.log('total=' + rows.length);
for (const r of rows) {
  const mult = (r.name.match(/\(x([\d.]+)\)/) || [])[1] || '';
  const free = /限时免费/.test(r.name) ? 'FREE' : '';
  console.log([r.id, (r.ctx / 1024) + 'K', r.tools ? 'tools' : '-', r.vision ? 'vision' : '-', 'x' + (mult || '?'), free].join(' | '));
}

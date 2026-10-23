import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// Lightweight DOM contract tests run the actual bundled scripts offline.
// No browser, provider, private tokens, npm runtime dependency or innerHTML.
class Element {
  constructor(tag = 'div') {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.events = new Map();
    this.classList = { toggle() {} };
  }
  set innerHTML(_) { throw new Error('Unsafe innerHTML is not allowed'); }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  setAttribute(name, value) { this[name] = value; }
  addEventListener(name, callback) { this.events.set(name, callback); }
  querySelector(selector) {
    for (const child of this.children) {
      if (child.tagName === selector) return child;
      const nested = child.querySelector?.(selector);
      if (nested) return nested;
    }
    return null;
  }
  showModal() { this.open = true; }
  close() { this.open = false; }
}
function browser() {
  const elements = new Map();
  const document = {
    querySelector(selector) {
      if (!elements.has(selector)) elements.set(selector, new Element());
      return elements.get(selector);
    },
    querySelectorAll() { return []; },
    createElement(tag) { return new Element(tag); },
    createTextNode(text) { return { textContent: text }; },
  };
  const stored = new Map();
  const context = vm.createContext({
    document, URL, console, Date,
    sessionStorage: {
      getItem: key => stored.get(key),
      setItem: (key, value) => stored.set(key, value),
      removeItem: key => stored.delete(key),
    },
    fetch() { throw new Error('Unexpected network request'); },
    location: { reload() {} },
    confirm: () => false,
  });
  for (const script of ['app.js', 'providers.js']) {
    vm.runInContext(readFileSync(new URL('../web/' + script, import.meta.url), 'utf8'), context, { filename: script });
  }
  return { context, elements, stored, run: source => vm.runInContext(source, context) };
}
function allText(element) {
  return [element.textContent ?? '', ...(element.children ?? []).map(allText)].join(' ');
}

test('safe links and model citations remain unverified text', () => {
  const b = browser();
  assert.equal(b.run(`safeLink('javascript:alert(1)')`), '#');
  assert.equal(b.run(`safeLink('data:text/html,test')`), '#');
  assert.equal(b.run(`safeLink('https://example.com/')`), 'https://example.com/');
  b.run(`renderResult({results:[{title:'<img src=x onerror=alert(1)>',url:'javascript:alert(1)',snippet:'<script>attack()</script>',basis:'model_citation',providers:['test']}],warnings:[]})`);
  const card = b.elements.get('#search-results').children[0];
  assert.equal(card.children[0].href, '#');
  assert.equal(card.children[0].rel, 'noopener noreferrer');
  assert.match(allText(card), /模型提供，未核验/);
  assert.match(allText(card), /<script>attack\(\)<\/script>/);
});

test('admin API handles auth and structured failures without token output', async () => {
  const b = browser();
  b.run(`token='fake-admin-token';`);
  let request;
  b.context.fetch = async (path, options) => {
    request = { path, options };
    return { status: 200, ok: true, json: async () => ({ saved: true }) };
  };
  const result = await b.run(`api('/api/admin/keys/7','PUT',{enabled:true,weight:2,max_concurrent:3})`);
  assert.equal(result.saved, true);
  assert.equal(request.path, '/api/admin/keys/7');
  assert.equal(request.options.headers.Authorization, 'Bearer fake-admin-token');
  assert.deepEqual(JSON.parse(request.options.body), { enabled: true, weight: 2, max_concurrent: 3 });
  b.context.fetch = async () => ({ status: 503, ok: false, json: async () => ({ error: 'database unavailable' }) });
  await assert.rejects(b.run(`api('/api/admin/overview')`), /database unavailable/);
  b.stored.set('sas-admin', 'fake-admin-token');
  b.context.fetch = async () => ({ status: 401, ok: false, json: async () => ({ error: 'unauthorized' }) });
  await assert.rejects(b.run(`api('/api/admin/keys')`), /令牌无效/);
  assert.equal(b.stored.has('sas-admin'), false);
  assert.equal(b.run('token'), '');
  assert.equal(b.elements.get('#login-dialog').open, true);
});

test('provider and key controls render through safe DOM operations', () => {
  const b = browser();
  b.run(`providers=[{id:'tavily',kind:'tavily',name:'Tavily',base_url:'https://example.com',enabled:true,timeout_seconds:15}];keys=[{id:7,provider_id:'tavily',label:'<script>label</script>',masked:'••••fake',enabled:true,cooldown_until:0,weight:1,max_concurrent:2,in_flight:0,uses:2,successes:1}];renderProviders();`);
  const card = b.elements.get('#provider-cards').children[0];
  assert.equal(card.dataset.providerId, 'tavily');
  assert.match(allText(card), /<script>label<\/script>/);
  assert.match(allText(card), /重置并启用/);
  assert.match(allText(card), /测试此 Key/);
});

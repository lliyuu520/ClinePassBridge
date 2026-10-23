const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const html = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/index.html'), 'utf8');
function section(start, end) {
  const from = html.indexOf(start);
  const to = html.indexOf(end, from);
  assert.ok(from >= 0 && to > from, `missing UI section: ${start}`);
  return html.slice(from, to);
}
const keyDeclaration = html.match(/^    const modelTestKey = .+;$/m);
assert.ok(keyDeclaration, 'missing model test cache key');
const code = [
  keyDeclaration[0],
  section('    function renderModelRows()', '    const parseProviders ='),
  section('    async function testMappedModel(', '    function candidateChanges('),
].join('\n');

function page(reply = async () => ({ ok: true, provider: 'openai-compatible-private' })) {
  const state = {
    models: [{ id: 'flash', upstream_id: 'cline-pass/flash', providers: ['deepseek'] }],
    config: { provider_policy: 'config' }, modelBusy: false,
  };
  const nodes = new Map();
  const pinButton = { dataset: { index: '0' }, setAttribute() {} };
  function node(id) {
    if (!nodes.has(id)) nodes.set(id, { value: '', innerHTML: '' });
    return nodes.get(id);
  }
  node('modelTestCredential').value = node('pinProbeCredential').value = 'fixture-credential';
  node('pinModelList').querySelectorAll = () => [pinButton];
  const context = vm.createContext({
    state, $: node, modelTestResults: new Map(), reasoningMappingRevision: 0,
    reasoningMappingBusy: false, pinAutosave: { flight: null }, performance,
    shown: value => value, esc: value => String(value), buttonIcon: () => '',
    modelToolsLabel: () => '', setButtonLabel() {},
    modelTestHTML: result => result ? result.pending ? 'pending' : result.provider : '',
    api: reply,
  });
  vm.runInContext(code, context);
  return {
    state, node, pinButton,
    run: () => context.testMappedModel({ ...state.models[0] }),
    setHas(has) {
      state.models = [{ ...state.models[0], has }];
      context.renderModelRows();
    },
    assertProvider(provider) {
      assert.equal(node('pinModelTest-0').innerHTML, provider);
      if (provider) assert.ok(node('modelList').innerHTML.includes(provider));
      else assert.ok(!node('modelList').innerHTML.includes('openai-compatible-private'));
    },
  };
}

test('adding, replacing or clearing has hides results from the previous routing configuration', async () => {
  for (const [before, after] of [[undefined, ['vision']], [['vision'], ['implicit-caching']], [['vision'], []]]) {
    const view = page();
    view.setHas(before);
    await view.run();
    view.assertProvider('openai-compatible-private');
    view.setHas(after);
    view.assertProvider('');
  }
});

test('missing, null and empty has retain the same unfiltered test result', async () => {
  const view = page();
  await view.run();
  for (const has of [null, []]) {
    view.setHas(has);
    view.assertProvider('openai-compatible-private');
  }
});

test('a late response from the old capability filter stays hidden until a new test runs', async () => {
  let finishOld;
  let calls = 0;
  const view = page(() => ++calls === 1
    ? new Promise(resolve => { finishOld = resolve; })
    : Promise.resolve({ ok: true, provider: 'deepseek' }));
  const oldTest = view.run();
  view.assertProvider('pending');
  assert.equal(view.pinButton.disabled, true);
  view.setHas(['vision']);
  view.assertProvider('');
  assert.equal(view.pinButton.disabled, false);
  finishOld({ ok: true, provider: 'openai-compatible-private' });
  await oldTest;
  view.assertProvider('');
  await view.run();
  assert.equal(calls, 2);
  view.assertProvider('deepseek');
  assert.ok(!view.node('modelList').innerHTML.includes('openai-compatible-private'));
});

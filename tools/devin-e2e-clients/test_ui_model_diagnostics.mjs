import assert from 'node:assert/strict'
import test from 'node:test'
import { createModelDiagnostics, modelControlSnapshot } from './ui-model-diagnostics.mjs'

const origin = 'http://127.0.0.1:9001'
function request(path, method = 'GET') { return { url: () => new URL(path, origin).href, method: () => method } }
function response(path, payload, status = 200) {
  return { request: () => request(path), status: () => status, json: async () => payload }
}
const manifest = '/v1/models?client_version=0.158.0'

test('dashboard statistics cannot stand in for the public manifest', async () => {
  const observer = createModelDiagnostics(origin, 'swe-2-high')
  const unrelated = '/api/v1/usage/dashboard/models'
  observer.request(request(unrelated))
  await observer.response(response(unrelated, { data: { models: [] } }))
  assert.equal(observer.diagnostics.catalog_requests, 0)
  assert.deepEqual(observer.diagnostics.model_http_statuses, [])
  assert.equal(observer.diagnostics.model_count, 0)
  assert.equal(observer.diagnostics.model_response_shape, 'not_received')
})

test('a 239-model manifest records the exact selected UID', async () => {
  const observer = createModelDiagnostics(origin, 'swe-2-high')
  const models = Array.from({ length: 239 }, (_, i) => ({ slug: i === 238 ? 'swe-2-high' : `synthetic-${i}` }))
  observer.request(request(manifest))
  await observer.response(response(manifest, { models }))
  assert.equal(observer.diagnostics.catalog_requests, 1)
  assert.equal(observer.diagnostics.model_count, 239)
  assert.equal(observer.diagnostics.selected_model_available, true)
  assert.equal(observer.diagnostics.model_response_shape, 'models_array')
})

test('malformed and OpenAI-list responses retain an explicit shape failure', async () => {
  for (const payload of [{ models: {} }, { data: [{ id: 'swe-2-high' }] }, null]) {
    const observer = createModelDiagnostics(origin, 'swe-2-high')
    await observer.response(response(manifest, payload))
    assert.equal(observer.diagnostics.model_response_shape, 'invalid_manifest')
    assert.equal(observer.diagnostics.models_field_kind, payload === null || !Object.hasOwn(payload, 'models') ? 'missing' : 'object')
    assert.equal(observer.diagnostics.model_count, 0)
    assert.equal(observer.diagnostics.selected_model_available, false)
    assert.ok(Object.hasOwn(JSON.parse(JSON.stringify(observer.diagnostics)), 'model_count'))
  }
})

test('failed and cross-origin requests are counted without retaining destinations or bodies', async () => {
  const observer = createModelDiagnostics(origin, 'swe-2-high')
  const blocked = 'https://synthetic.invalid/v1/models?client_version=0.158.0&key=synthetic-secret'
  observer.request(request(blocked))
  observer.requestFailed(request(blocked))
  await observer.response(response(blocked, { models: [{ slug: 'swe-2-high' }], secret: 'synthetic-body' }))
  assert.equal(observer.diagnostics.cross_origin_catalog_requests, 1)
  assert.equal(observer.diagnostics.catalog_request_failures, 1)
  assert.deepEqual(observer.diagnostics.model_http_statuses, [])
  assert.doesNotMatch(JSON.stringify(observer.diagnostics), /synthetic\.invalid|synthetic-secret|synthetic-body/)
})

test('ordinary model lists and inference responses are excluded', async () => {
  const observer = createModelDiagnostics(origin, 'swe-2-high')
  for (const r of [request('/v1/models'), request(manifest, 'POST')]) observer.request(r)
  assert.equal(observer.diagnostics.catalog_requests, 0)
  await observer.response({ request: () => request(manifest), status: () => 502, json: async () => { throw Error('synthetic-body') } })
  assert.deepEqual(observer.diagnostics.model_http_statuses, [502])
  assert.equal(observer.diagnostics.model_response_shape, 'invalid_json')
})

test('browser aborts are distinguished without retaining arbitrary error messages', () => {
  const observer = createModelDiagnostics(origin, 'swe-2-high')
  observer.requestFailed({ ...request(manifest), failure: () => ({ errorText: 'net::ERR_ABORTED' }) })
  observer.requestFailed({ ...request(manifest), failure: () => ({ errorText: 'synthetic-private-error' }) })
  assert.equal(observer.diagnostics.catalog_request_failures, 2)
  assert.equal(observer.diagnostics.catalog_request_aborts, 1)
  assert.doesNotMatch(JSON.stringify(observer.diagnostics), /synthetic-private-error/)
})

test('DOM evidence distinguishes missing modal, wrong platform and missing target option', () => {
  try {
    globalThis.document = { querySelector: () => null }
    assert.deepEqual(modelControlSnapshot('swe-2-high'), {
      modal_present: false, modal_visible: false, devin_platform: false,
      select_present: false, select_disabled: null, option_count: 0, available_option_count: 0, target_option_present: false
    })
    const modal = { dataset: { platform: '' }, getClientRects: () => [{}] }
    const select = { disabled: false, options: [{ value: '' }, { value: 'other-synthetic-model' }] }
    globalThis.document = { querySelector: selector => selector.includes('use-key-modal') ? modal : select }
    const before = modelControlSnapshot('swe-2-high')
    assert.equal(before.modal_visible, true)
    assert.equal(before.devin_platform, false)
    assert.equal(before.target_option_present, false)
    assert.equal(before.available_option_count, 1)
    modal.dataset.platform = 'devin'
    select.options.push({ value: 'swe-2-high' })
    const after = modelControlSnapshot('swe-2-high')
    assert.equal(after.devin_platform, true)
    assert.equal(after.target_option_present, true)
    assert.equal(after.available_option_count, 2)
  } finally { delete globalThis.document }
})

test('ready DOM evidence does not depend on an asynchronous response-body read', async () => {
  const observer = createModelDiagnostics(origin, 'swe-2-high')
  let finishRead
  const reading = observer.response({ request: () => request(manifest), status: () => 200,
    json: () => new Promise(resolve => { finishRead = resolve }) })
  try {
    globalThis.document = { querySelector: selector => selector.includes('use-key-modal')
      ? { dataset: { platform: 'devin' }, getClientRects: () => [{}] }
      : { disabled: false, options: [{ value: '' }, { value: 'swe-2-high' }, { value: 'other' }] } }
    assert.equal(observer.diagnostics.model_count, 0)
    assert.equal(modelControlSnapshot('swe-2-high').available_option_count, 2)
  } finally {
    delete globalThis.document
    finishRead({ models: [{ slug: 'swe-2-high' }, { slug: 'other' }] })
    await reading
  }
  assert.equal(observer.diagnostics.model_count, 2)
})

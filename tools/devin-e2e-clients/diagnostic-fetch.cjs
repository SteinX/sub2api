// Mock-only diagnostics: preserve the installed fetch/proxy/CA implementation,
// allow only the configured loopback gateway, and never inspect headers/bodies.
const { appendFileSync } = require('node:fs')
const { createHash } = require('node:crypto')
if (process.env.DEVIN_E2E_MODE !== 'mock') throw new Error('Fetch diagnostics require a synthetic upstream')
const allowed = process.env.DEVIN_JOURNEY_FETCH_ORIGIN
if (!/^http:\/\/127\.0\.0\.1:[0-9]+$/.test(allowed || '')) throw new Error('Invalid diagnostic gateway')
const original = globalThis.fetch
globalThis.fetch = async function(input, init) {
  const url = new URL(typeof input === 'string' || input instanceof URL ? input : input.url)
  const known = ['/v1/chat/completions', '/v1/responses', '/v1/responses/compact', '/v1/models']
  const record = { destination: url.origin === allowed ? 'configured_loopback_gateway' : 'denied_other_origin',
    host: ['127.0.0.1', 'api.openai.com', 'api.anthropic.com', 'server.codeium.com'].includes(url.hostname) ? url.hostname : createHash('sha256').update(url.hostname).digest('hex'),
    path: known.includes(url.pathname) ? url.pathname : 'other', method: init?.method || input.method || 'GET' }
  if (url.origin !== allowed) {
    appendFileSync(process.env.DEVIN_JOURNEY_FETCH_TRACE, JSON.stringify(record) + '\n')
    throw new Error('journey_fetch_destination_denied')
  }
  try {
    const response = await original.call(this, input, init)
    record.status = response.status
    appendFileSync(process.env.DEVIN_JOURNEY_FETCH_TRACE, JSON.stringify(record) + '\n')
    return response
  } catch (error) {
    record.transport_error = true
    appendFileSync(process.env.DEVIN_JOURNEY_FETCH_TRACE, JSON.stringify(record) + '\n')
    throw error
  }
}

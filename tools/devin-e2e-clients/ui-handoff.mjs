// Only the browser reads/generates configuration. Never log page contents,
// clipboard data, console text, tokens or screenshots.
import { chromium } from 'playwright-core'
import { writeFile, mkdir, access } from 'node:fs/promises'
import { constants } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { createModelDiagnostics, modelControlSnapshot } from './ui-model-diagnostics.mjs'

let stage = 'input'
let diagnostics = {}
const journey = { auth_method: 'login_form', login_http_status: null, key_list_http_status: null, selected_key_matches: false, routes: [] }
const catalogRecovery = { loading_visible: false, timeout_visible: false, no_configuration_on_error: false, manual_refresh: false }
const uiScope = 'full product application: login, key list, selection and configuration copy'
async function main() {
  let raw = ''
  for await (const chunk of process.stdin) { raw += chunk; if (raw.length > 65536) throw new Error('Oversized UI request') }
  const input = JSON.parse(raw)
  const origin = new URL(input.ui_url).origin
  if (!/^http:\/\/127\.0\.0\.1:[0-9]+$/.test(origin)) throw new Error('Only the disposable UI is supported')
  const observer = createModelDiagnostics(origin, input.model)
  diagnostics = observer.diagnostics
  let executablePath = process.env.CHROMIUM_EXECUTABLE_PATH
  if (!executablePath) {
    for (const candidate of ['/usr/bin/chromium', '/usr/bin/google-chrome', '/usr/bin/chromium-browser']) {
      try { await access(candidate, constants.X_OK); executablePath = candidate; break } catch { /* try installed browser */ }
    }
  }
  if (!executablePath) throw new Error('Install a supported browser before running the journey')
  stage = 'browser-launch'
  const browser = await chromium.launch({ executablePath, headless: true })
  let page
  try {
    const context = await browser.newContext({ permissions: ['clipboard-read', 'clipboard-write'] })
    await context.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort())
    page = await context.newPage()
    page.on('request', observer.request)
    page.on('requestfailed', observer.requestFailed)
    page.on('response', async response => {
      const route = new URL(response.url()).pathname
      if (route === '/api/v1/auth/login') journey.login_http_status = response.status()
      if (route === '/api/v1/keys') {
        journey.key_list_http_status = response.status()
        try {
          const payload = await response.json()
          const items = payload.data?.items ?? []
          const selected = items.find(key => key.name === input.key_name)
          journey.selected_key_matches = Boolean(selected?.key && createHash('sha256').update(selected.key).digest('hex') === input.expected_key_sha256)
        } catch { /* never log key responses */ }
      }
      await observer.response(response)
    })
    page.on('pageerror', () => { diagnostics.page_errors++ })
    stage = 'navigate'
    await page.goto(`${origin}/login`, { waitUntil: 'networkidle', timeout: 30000 })
    journey.routes.push(new URL(page.url()).pathname)
    stage = 'product-login'
    await page.locator('#email').fill(input.email)
    await page.locator('#password').fill(input.password)
    await page.locator('button[type="submit"]').click()
    await page.waitForURL(url => url.pathname === '/dashboard', { timeout: 30000 })
    journey.routes.push(new URL(page.url()).pathname)
    stage = 'product-key-list'
    await page.locator('a[href="/keys"]').first().click()
    await page.waitForURL(url => url.pathname === '/keys', { timeout: 30000 })
    journey.routes.push(new URL(page.url()).pathname)
    await page.getByTestId('key-use').first().waitFor()
    let selectedButton
    for (const button of await page.getByTestId('key-use').all()) {
      if (await button.getAttribute('data-key-name') === input.key_name) { selectedButton = button; break }
    }
    if (!selectedButton || !journey.selected_key_matches || journey.login_http_status !== 200 || journey.key_list_http_status !== 200) throw new Error('Authenticated product key selection failed')
    await selectedButton.click()
    stage = 'product-key-modal'
    await page.getByTestId('use-key-modal').waitFor()
    stage = 'model-select-control'
    await page.getByTestId('devin-client-model').waitFor()
    Object.assign(diagnostics, await page.evaluate(modelControlSnapshot, input.model))
    if (input.catalog_recovery_probe) {
      catalogRecovery.loading_visible = await page.getByTestId('devin-models-loading').isVisible()
      if (catalogRecovery.loading_visible) {
        stage = 'catalog-timeout'
        await page.getByTestId('devin-models-error').waitFor()
        catalogRecovery.timeout_visible = await page.getByTestId('devin-models-error').getAttribute('data-timeout') === 'true'
        catalogRecovery.no_configuration_on_error = await page.getByTestId('client-config-file').count() === 0
        if (!catalogRecovery.timeout_visible || !catalogRecovery.no_configuration_on_error || !await page.getByTestId('devin-models-refresh').isEnabled()) throw new Error('Invalid catalog failure state')
        await page.getByTestId('devin-models-refresh').click()
        catalogRecovery.manual_refresh = true
      }
    }
    stage = 'model-target-option'
    await page.waitForFunction(model => [...document.querySelectorAll('#devin-client-model option')].some(option => option.value === model), input.model)
    Object.assign(diagnostics, await page.evaluate(modelControlSnapshot, input.model))
    await page.getByTestId('devin-client-model').selectOption(input.model)
    stage = 'client-tab'
    await page.locator(`button[data-client="${input.client}"]`).click()
    stage = 'environment-key'
    await page.getByTestId('devin-key-from-env').check()
    if (input.client !== 'omp') {
      await page.getByTestId('devin-project-mode').check()
      await page.getByTestId('devin-workspace-path').fill(input.workspace)
      await page.getByTestId('devin-test-command').fill(input.test_command)
    } else {
      await page.getByTestId('devin-omp-profile').selectOption(input.profile)
    }
    stage = 'config-generation'
    const instructions = await page.getByTestId('devin-instructions').innerText()
    const nodes = page.getByTestId('client-config-file')
    diagnostics.config_file_count = await nodes.count()
    if (await page.getByTestId('devin-omp-incompatible').count()) {
      if (input.client !== 'omp' || input.profile !== 'default' || !input.model.startsWith('fusion-') || await nodes.count() !== 0) throw new Error('Invalid product refusal')
      process.stdout.write(JSON.stringify({ passed: false, blocked: true, reason: 'omp_default_fusion_history_pruning',
        source: 'UseKeyModal browser refusal', client: input.client, profile: input.profile, selected_model: input.model,
        files: [], browser: browser.version(), ui_scope: uiScope, product_journey: journey }) + '\n')
      return
    }
    await nodes.first().waitFor()
    const files = []
    for (let index = 0; index < await nodes.count(); index++) {
      const node = nodes.nth(index)
      stage = 'clipboard'
      await node.locator('button').click()
      const copied = await page.evaluate(() => navigator.clipboard.readText())
      if (copied !== await node.locator('pre code').textContent()) throw new Error('Copied configuration differs from displayed configuration')
      const config = JSON.parse(copied)
      const advertised = await node.getAttribute('data-config-path')
      const name = advertised.replaceAll('\\', '/').split('/').at(-1)
      const targets = {
        'opencode.json': path.join(input.workspace, 'opencode.json'),
        'omo.jsonc': path.join(input.workspace, '.omo/omo.jsonc'),
        'models.yml': path.join(input.directory, 'omp-agent/models.yml'),
        'config.yml': path.join(input.directory, 'omp-agent/config.yml')
      }
      if (!(name in targets)) throw new Error('Unexpected product configuration file')
      const provider = name === 'opencode.json' ? config.provider?.devin?.options : name === 'models.yml' ? config.providers?.devin : null
      if (provider && !['{env:SUB2API_API_KEY}', 'SUB2API_API_KEY'].includes(provider.apiKey)) throw new Error('Generated configuration must reference the environment')
      await mkdir(path.dirname(targets[name]), { recursive: true })
      await writeFile(targets[name], copied, { mode: 0o600 })
      diagnostics.copied_files++
      const normalized = copied.replaceAll(origin, '<gateway>').replaceAll(input.workspace, '<workspace>')
      files.push({ name, sha256: createHash('sha256').update(normalized).digest('hex') })
    }
    if (input.catalog_recovery_probe && !catalogRecovery.manual_refresh) throw new Error('Catalog recovery was not exercised')
    process.stdout.write(JSON.stringify({
      passed: true, source: 'UseKeyModal browser copy buttons', ui_scope: uiScope, product_journey: journey,
      client: input.client, profile: input.profile, selected_model: input.model, files,
      instructions_sha256: createHash('sha256').update(instructions).digest('hex'), browser: browser.version(),
      catalog_recovery: input.catalog_recovery_probe ? catalogRecovery : undefined
    }) + '\n')
  } catch (error) {
    if (page) {
      try { Object.assign(diagnostics, await page.evaluate(modelControlSnapshot, input.model)) } catch { /* keep prior structural snapshot */ }
    }
    throw error
  } finally { await browser.close() }
}
main().catch(error => { process.stdout.write(JSON.stringify({ passed: false, stage, diagnostics, ui_scope: uiScope, product_journey: journey, error_kind: ['Error', 'TimeoutError', 'TypeError'].includes(error.name) ? error.name : 'other' }) + '\n'); process.exitCode = 1 })

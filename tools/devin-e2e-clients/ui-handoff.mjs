// Only the browser reads/generates configuration. Never log page contents,
// clipboard data, console text, tokens or screenshots.
import { chromium } from 'playwright-core'
import { writeFile, mkdir, access } from 'node:fs/promises'
import { constants } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'

let stage = 'input'
async function main() {
  let raw = ''
  for await (const chunk of process.stdin) { raw += chunk; if (raw.length > 65536) throw new Error('Oversized UI request') }
  const input = JSON.parse(raw)
  const origin = new URL(input.ui_url).origin
  if (!/^http:\/\/127\.0\.0\.1:[0-9]+$/.test(origin)) throw new Error('Only the disposable UI is supported')
  let executablePath = process.env.CHROMIUM_EXECUTABLE_PATH
  if (!executablePath) {
    for (const candidate of ['/usr/bin/chromium', '/usr/bin/google-chrome', '/usr/bin/chromium-browser']) {
      try { await access(candidate, constants.X_OK); executablePath = candidate; break } catch { /* try installed browser */ }
    }
  }
  if (!executablePath) throw new Error('Install a supported browser before running the journey')
  stage = 'browser-launch'
  const browser = await chromium.launch({ executablePath, headless: true })
  try {
    const context = await browser.newContext({ permissions: ['clipboard-read', 'clipboard-write'] })
    await context.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort())
    const page = await context.newPage()
    stage = 'navigate'
    await page.goto(`${origin}/test-support/devin-handoff.html`, { waitUntil: 'networkidle', timeout: 30000 })
    stage = 'model-discovery'
    await page.getByTestId('devin-client-model').selectOption(input.model)
    await page.locator(`button[data-client="${input.client}"]`).click()
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
      const normalized = copied.replaceAll(origin, '<gateway>').replaceAll(input.workspace, '<workspace>')
      files.push({ name, sha256: createHash('sha256').update(normalized).digest('hex') })
    }
    process.stdout.write(JSON.stringify({
      passed: true, source: 'UseKeyModal browser copy buttons', ui_scope: 'isolated product modal; signup/login not covered',
      client: input.client, profile: input.profile, selected_model: input.model, files,
      instructions_sha256: createHash('sha256').update(instructions).digest('hex'), browser: browser.version()
    }) + '\n')
  } finally { await browser.close() }
}
main().catch(error => { process.stdout.write(JSON.stringify({ passed: false, stage, error_kind: ['Error', 'TimeoutError', 'TypeError'].includes(error.name) ? error.name : 'other' }) + '\n'); process.exitCode = 1 })

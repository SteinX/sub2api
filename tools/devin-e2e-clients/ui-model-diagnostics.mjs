// Structural observations only: never retain URLs, headers, bodies or keys.
export function createCatalogProgress(observer, emit) {
  const ordinals = new WeakMap()
  const statuses = new WeakMap()
  return {
    request(request) {
      const before = observer.diagnostics.catalog_requests
      observer.request(request)
      if (observer.diagnostics.catalog_requests !== before) {
        const ordinal = observer.diagnostics.catalog_requests
        ordinals.set(request, ordinal); emit('request', ordinal, null)
      }
    },
    responseHeaders(response) {
      const request = response.request(), ordinal = ordinals.get(request)
      if (ordinal) { statuses.set(request, response.status()); emit('headers', ordinal, response.status()) }
    },
    requestFailed(request) {
      observer.requestFailed(request)
      const ordinal = ordinals.get(request)
      if (ordinal) emit('request-failed', ordinal, statuses.get(request) ?? null)
    },
    requestFinished(request) {
      const ordinal = ordinals.get(request)
      if (ordinal) emit('body-finished', ordinal, statuses.get(request) ?? null)
    }
  }
}

export function createModelDiagnostics(origin, selectedModel) {
  const diagnostics = {
    model_http_statuses: [], model_count: 0, selected_model_available: false,
    catalog_requests: 0, catalog_request_failures: 0, catalog_request_aborts: 0, cross_origin_catalog_requests: 0,
    model_response_shape: 'not_received', models_field_kind: 'not_received', data_field_kind: 'not_received', page_errors: 0,
    modal_present: false, modal_visible: false, devin_platform: false,
    select_present: false, select_disabled: null, option_count: 0, available_option_count: 0, target_option_present: false,
    config_file_count: 0, copied_files: 0
  }
  function catalogRequest(request) {
    const url = new URL(request.url())
    return request.method() === 'GET' && url.pathname === '/v1/models' && Boolean(url.searchParams.get('client_version'))
      ? url : null
  }
  return {
    diagnostics,
    request(request) {
      const url = catalogRequest(request)
      if (!url) return
      diagnostics.catalog_requests++
      if (url.origin !== origin) diagnostics.cross_origin_catalog_requests++
    },
    requestFailed(request) {
      if (!catalogRequest(request)) return
      diagnostics.catalog_request_failures++
      if (request.failure?.()?.errorText === 'net::ERR_ABORTED') diagnostics.catalog_request_aborts++
    },
    async response(response) {
      const url = catalogRequest(response.request())
      if (!url || url.origin !== origin) return
      diagnostics.model_http_statuses.push(response.status())
      try {
        const payload = await response.json()
        const kind = (object, key) => !object || !Object.hasOwn(object, key) ? 'missing'
          : Array.isArray(object[key]) ? 'array' : object[key] === null ? 'null' : typeof object[key]
        diagnostics.models_field_kind = kind(payload, 'models')
        diagnostics.data_field_kind = kind(payload, 'data')
        if (!payload || !Array.isArray(payload.models)) {
          diagnostics.model_response_shape = 'invalid_manifest'
          return
        }
        diagnostics.model_response_shape = 'models_array'
        diagnostics.model_count = payload.models.length
        diagnostics.selected_model_available = payload.models.some(model => model && model.slug === selectedModel)
      } catch {
        diagnostics.model_response_shape = 'invalid_json'
      }
    }
  }
}

// Playwright serializes this function into the page. Do not read option text.
export function modelControlSnapshot(model) {
  const modal = document.querySelector('[data-testid="use-key-modal"]')
  const select = document.querySelector('[data-testid="devin-client-model"]')
  return {
    modal_present: Boolean(modal), modal_visible: Boolean(modal?.getClientRects().length),
    devin_platform: modal?.dataset.platform === 'devin',
    select_present: Boolean(select), select_disabled: select ? select.disabled : null,
    option_count: select ? select.options.length : 0,
    available_option_count: select ? [...select.options].filter(option => option.value !== '').length : 0,
    target_option_present: select ? [...select.options].some(option => option.value === model) : false
  }
}

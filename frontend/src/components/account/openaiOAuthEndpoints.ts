export interface OpenAIOAuthEndpoints {
  responses_url?: string
  chatgpt_base_url?: string
  auth_base_url?: string
  platform_base_url?: string
}

export const OPENAI_OAUTH_ENDPOINT_DEFAULTS = {
  responses_url: 'https://chatgpt.com/backend-api/codex/responses',
  chatgpt_base_url: 'https://chatgpt.com',
  auth_base_url: 'https://auth.openai.com',
  platform_base_url: 'https://api.openai.com'
}

export function buildOpenAIOAuthEndpoints(value: OpenAIOAuthEndpoints): OpenAIOAuthEndpoints | undefined {
  const endpoints: OpenAIOAuthEndpoints = {}
  for (const key of Object.keys(OPENAI_OAUTH_ENDPOINT_DEFAULTS) as (keyof OpenAIOAuthEndpoints)[]) {
    const url = value[key]?.trim().replace(/\/+$/, '')
    if (url && url !== OPENAI_OAUTH_ENDPOINT_DEFAULTS[key]) endpoints[key] = url
  }
  return Object.keys(endpoints).length ? endpoints : undefined
}

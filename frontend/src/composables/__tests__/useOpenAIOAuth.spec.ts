import { describe, expect, it, vi } from 'vitest'

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn()
  })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const messages: Record<string, string> = {
        'admin.accounts.oauth.openai.failedToExchangeCode': 'OpenAI 授权码兑换失败',
        'admin.accounts.oauth.openai.errors.OPENAI_OAUTH_PROXY_REQUIRED':
          '未设置代理，当前服务器无法直连 OpenAI，导致 OpenAI OAuth 请求失败。请先选择可访问 OpenAI 的代理后重试；如果授权码已失效，请重新生成授权链接。'
      }
      return messages[key] ?? key
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      refreshOpenAIToken: vi.fn()
    }
  }
}))

import { useOpenAIOAuth } from '@/composables/useOpenAIOAuth'
import { adminAPI } from '@/api/admin'

describe('useOpenAIOAuth custom endpoints', () => {
  const endpoints = { auth_base_url: 'http://auth.example/relay', chatgpt_base_url: 'http://chat.example' }

  it('sends endpoints before authorization and refresh-token validation', async () => {
    vi.mocked(adminAPI.accounts.generateAuthUrl).mockResolvedValueOnce({ auth_url: 'http://auth.example/relay/oauth/authorize?state=abc', session_id: 'session' })
    vi.mocked(adminAPI.accounts.refreshOpenAIToken).mockResolvedValueOnce({ access_token: 'at', oauth_endpoints: endpoints })
    const oauth = useOpenAIOAuth()
    expect(await oauth.generateAuthUrl(undefined, undefined, endpoints)).toBe(true)
    expect(adminAPI.accounts.generateAuthUrl).toHaveBeenLastCalledWith('/admin/openai/generate-auth-url', { oauth_endpoints: endpoints })
    const token = await oauth.validateRefreshToken('rt', undefined, undefined, endpoints)
    expect(adminAPI.accounts.refreshOpenAIToken).toHaveBeenLastCalledWith('rt', undefined, '/admin/openai/refresh-token', undefined, endpoints)
    expect(oauth.buildCredentials(token!)).toMatchObject({ oauth_endpoints: endpoints })
  })
})

describe('useOpenAIOAuth.buildCredentials', () => {
  it('should keep client_id when token response contains it', () => {
    const oauth = useOpenAIOAuth()
    const creds = oauth.buildCredentials({
      access_token: 'at',
      refresh_token: 'rt',
      client_id: 'app_test_client',
      expires_at: 1700000000
    })

    expect(creds.client_id).toBe('app_test_client')
    expect(creds.access_token).toBe('at')
    expect(creds.refresh_token).toBe('rt')
  })

  it('should keep legacy behavior when client_id is missing', () => {
    const oauth = useOpenAIOAuth()
    const creds = oauth.buildCredentials({
      access_token: 'at',
      refresh_token: 'rt',
      expires_at: 1700000000
    })

    expect(Object.prototype.hasOwnProperty.call(creds, 'client_id')).toBe(false)
    expect(creds.access_token).toBe('at')
    expect(creds.refresh_token).toBe('rt')
  })

  it('should keep ChatGPT subscription expiration from token response', () => {
    const oauth = useOpenAIOAuth()
    const creds = oauth.buildCredentials({
      access_token: 'at',
      refresh_token: 'rt',
      expires_at: 1700000000,
      plan_type: 'team',
      subscription_expires_at: '2026-07-20T19:22:48+00:00'
    })

    expect(creds.plan_type).toBe('team')
    expect(creds.subscription_expires_at).toBe('2026-07-20T19:22:48+00:00')
  })
})

describe('useOpenAIOAuth.exchangeAuthCode', () => {
  it('shows a clear proxy hint when code exchange fails without a proxy', async () => {
    vi.mocked(adminAPI.accounts.exchangeCode).mockRejectedValueOnce({
      status: 502,
      reason: 'OPENAI_OAUTH_PROXY_REQUIRED',
      message: 'OpenAI OAuth token exchange failed: no proxy is configured.'
    })
    const oauth = useOpenAIOAuth()

    const tokenInfo = await oauth.exchangeAuthCode('code', 'session-id', 'state')

    expect(tokenInfo).toBeNull()
    expect(oauth.error.value).toBe(
      '未设置代理，当前服务器无法直连 OpenAI，导致 OpenAI OAuth 请求失败。请先选择可访问 OpenAI 的代理后重试；如果授权码已失效，请重新生成授权链接。'
    )
  })
})

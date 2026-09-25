import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account } from '@/types'

const { refreshToken, applyOAuthCredentials, generateAuthUrl, exchangeCode } = vi.hoisted(() => ({
  refreshToken: vi.fn(), applyOAuthCredentials: vi.fn(),
  generateAuthUrl: vi.fn(), exchangeCode: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() })
}))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('@/api/admin', () => ({
  adminAPI: { accounts: { refreshOpenAIToken: refreshToken, applyOAuthCredentials, generateAuthUrl, exchangeCode } }
}))

import ReAuthAccountModal from '../ReAuthAccountModal.vue'
import OAuthAuthorizationFlow from '@/components/account/OAuthAuthorizationFlow.vue'

const endpoints = { auth_base_url: 'https://auth.example/relay', chatgpt_base_url: 'https://chat.example/relay' }
const wrappers: ReturnType<typeof mount>[] = []
function mountModal(credentials: Record<string, unknown> = {}) {
  const wrapper = mount(ReAuthAccountModal, {
    props: {
      show: true,
      account: { id: 7, name: 'ChatGPT', platform: 'openai', type: 'oauth', proxy_id: 3, credentials } as Account
    },
    global: {
      stubs: {
        BaseDialog: defineComponent({ template: '<div><slot /><slot name="footer" /></div>' }),
        Icon: true
      }
    }
  })
  wrappers.push(wrapper)
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
  refreshToken.mockResolvedValue({ access_token: 'new-at', refresh_token: 'rotated-rt', expires_at: 123, oauth_endpoints: endpoints })
  applyOAuthCredentials.mockResolvedValue({ id: 7 })
})
afterEach(() => wrappers.splice(0).forEach(wrapper => wrapper.unmount()))

describe('ChatGPT reauthorization', () => {
  it('uses the account endpoints, proxy and client ID for manual RT reauthorization', async () => {
    const credentials = { oauth_endpoints: endpoints, client_id: 'existing-client' }
    const wrapper = mountModal(credentials)
    await wrapper.get('input[value="refresh_token"]').setValue(true)
    await wrapper.get('textarea').setValue(' new-rt ')
    expect(wrapper.findAll('button').some(button => button.text().includes('admin.accounts.oauth.completeAuth'))).toBe(false)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(refreshToken).toHaveBeenCalledWith('new-rt', 3, '/admin/openai/refresh-token', 'existing-client', endpoints)
    expect(applyOAuthCredentials).toHaveBeenCalledWith(7, expect.objectContaining({
      type: 'oauth', credentials: expect.objectContaining({ oauth_endpoints: endpoints, access_token: 'new-at', refresh_token: 'rotated-rt' })
    }))
    expect(wrapper.emitted('reauthorized')).toEqual([[{ id: 7 }]])
  })

  it('uses defaults when no endpoints are configured and keeps a non-rotated RT', async () => {
    refreshToken.mockResolvedValue({ access_token: 'new-at' })
    const wrapper = mountModal()
    await wrapper.get('input[value="refresh_token"]').setValue(true)
    await wrapper.get('textarea').setValue('new-rt')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(refreshToken).toHaveBeenCalledWith('new-rt', 3, '/admin/openai/refresh-token', undefined, undefined)
    expect(applyOAuthCredentials).toHaveBeenCalledWith(7, expect.objectContaining({
      credentials: expect.objectContaining({ refresh_token: 'new-rt' })
    }))
  })

  it('does not save or clear errors when RT validation fails', async () => {
    refreshToken.mockRejectedValueOnce(new Error('invalid RT'))
    const wrapper = mountModal({ oauth_endpoints: endpoints })
    await wrapper.get('input[value="refresh_token"]').setValue(true)
    await wrapper.get('textarea').setValue('bad-rt')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(applyOAuthCredentials).not.toHaveBeenCalled()
    expect(wrapper.emitted('reauthorized')).toBeUndefined()
  })

  it('preserves endpoints through authorization-code reauthorization', async () => {
    generateAuthUrl.mockResolvedValueOnce({ auth_url: 'https://auth.example/relay/oauth/authorize?state=abc', session_id: 'session' })
    exchangeCode.mockResolvedValueOnce({ access_token: 'new-at', oauth_endpoints: endpoints })
    const wrapper = mountModal({ oauth_endpoints: endpoints })
    const flow = wrapper.getComponent(OAuthAuthorizationFlow)
    flow.vm.$emit('generate-url')
    await flushPromises()
    expect(generateAuthUrl).toHaveBeenCalledWith('/admin/openai/generate-auth-url', { proxy_id: 3, oauth_endpoints: endpoints })
    await wrapper.get('input[value="manual"]').setValue(true)
    await wrapper.get('textarea[placeholder="admin.accounts.oauth.openai.authCodePlaceholder"]').setValue('code')
    const complete = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.oauth.completeAuth'))!
    await complete.trigger('click')
    await flushPromises()
    expect(applyOAuthCredentials).toHaveBeenCalledWith(7, expect.objectContaining({
      credentials: expect.objectContaining({ oauth_endpoints: endpoints, access_token: 'new-at' })
    }))
  })
})

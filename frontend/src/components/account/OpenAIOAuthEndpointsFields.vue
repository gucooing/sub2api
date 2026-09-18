<template>
  <div class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
    <label class="input-label">{{ t('admin.accounts.openai.oauthEndpoints.title') }}</label>
    <p class="input-hint">{{ t('admin.accounts.openai.oauthEndpoints.hint') }}</p>
    <div v-for="key in keys" :key="key">
      <label :for="`openai-oauth-${key}`" class="input-label">
        {{ t(`admin.accounts.openai.oauthEndpoints.${key}`) }}
      </label>
      <input
        :id="`openai-oauth-${key}`"
        :value="modelValue[key]"
        type="url"
        class="input font-mono"
        :placeholder="OPENAI_OAUTH_ENDPOINT_DEFAULTS[key]"
        @input="update(key, $event)"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { OPENAI_OAUTH_ENDPOINT_DEFAULTS, type OpenAIOAuthEndpoints } from './openaiOAuthEndpoints'

const props = defineProps<{ modelValue: OpenAIOAuthEndpoints }>()
const emit = defineEmits<{ 'update:modelValue': [value: OpenAIOAuthEndpoints] }>()
const { t } = useI18n()
const keys = Object.keys(OPENAI_OAUTH_ENDPOINT_DEFAULTS) as (keyof OpenAIOAuthEndpoints)[]
function update(key: keyof OpenAIOAuthEndpoints, event: Event) {
  emit('update:modelValue', { ...props.modelValue, [key]: (event.target as HTMLInputElement).value })
}
</script>

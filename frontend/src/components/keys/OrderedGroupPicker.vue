<template>
  <div class="space-y-3" data-tour="key-form-group">
    <!-- Selected ordered list -->
    <div v-if="selectedGroups.length" class="space-y-2">
      <VueDraggable
        v-model="selectedGroups"
        :animation="200"
        handle=".drag-handle"
        class="space-y-2"
        @end="emitIds"
      >
        <div
          v-for="(group, index) in selectedGroups"
          :key="group.id"
          class="flex items-center gap-2 rounded-lg border border-gray-200 bg-white px-2 py-2 dark:border-dark-600 dark:bg-dark-800"
        >
          <div
            class="drag-handle flex cursor-grab items-center text-gray-300 hover:text-gray-500 active:cursor-grabbing dark:text-dark-600 dark:hover:text-dark-400"
            :title="t('keys.dragToReorder')"
          >
            <svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor">
              <path
                d="M7 2a2 2 0 1 0 0 4 2 2 0 0 0 0-4zM13 2a2 2 0 1 0 0 4 2 2 0 0 0 0-4zM7 8a2 2 0 1 0 0 4 2 2 0 0 0 0-4zM13 8a2 2 0 1 0 0 4 2 2 0 0 0 0-4zM7 14a2 2 0 1 0 0 4 2 2 0 0 0 0-4zM13 14a2 2 0 1 0 0 4 2 2 0 0 0 0-4z"
              />
            </svg>
          </div>

          <span
            class="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary-50 text-xs font-semibold text-primary-700 dark:bg-primary-900/30 dark:text-primary-300"
          >
            {{ index + 1 }}
          </span>

          <GroupBadge
            :name="group.name"
            :platform="group.platform"
            :subscription-type="group.subscription_type"
            :rate-multiplier="group.rate_multiplier"
            :user-rate-multiplier="userGroupRates?.[group.id] ?? null"
            :peak-rate-enabled="group.peak_rate_enabled"
            :peak-start="group.peak_start"
            :peak-end="group.peak_end"
            :peak-rate-multiplier="group.peak_rate_multiplier"
            class="min-w-0 flex-1"
          />

          <span
            v-if="index === 0"
            class="shrink-0 rounded bg-primary-50 px-1.5 py-0.5 text-[10px] font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300"
          >
            {{ t('keys.primaryGroup') }}
          </span>
          <span
            v-else
            class="shrink-0 rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700 dark:text-dark-400"
          >
            {{ t('keys.fallbackGroup') }}
          </span>

          <button
            type="button"
            class="shrink-0 rounded-md p-1 text-gray-400 transition-colors hover:bg-gray-100 hover:text-red-500 dark:hover:bg-dark-700"
            :title="t('keys.removeGroup')"
            @click="removeGroup(group.id)"
          >
            <Icon name="x" size="sm" />
          </button>
        </div>
      </VueDraggable>
    </div>

    <div
      v-else
      class="rounded-lg border border-dashed border-gray-200 px-3 py-4 text-center text-sm text-gray-400 dark:border-dark-600 dark:text-dark-400"
    >
      {{ t('keys.noGroupsSelected') }}
    </div>

    <!-- Add group -->
    <div v-if="canAddMore" class="space-y-2">
      <label class="input-label mb-0">{{ t('keys.addFallbackGroup') }}</label>
      <Select
        :model-value="null"
        :options="addableOptions"
        :placeholder="t('keys.selectGroup')"
        :searchable="true"
        :search-placeholder="t('keys.searchGroup')"
        @update:model-value="onAdd"
      >
        <template #selected>
          <span class="text-gray-400">{{ t('keys.selectGroup') }}</span>
        </template>
        <template #option="{ option, selected }">
          <GroupOptionItem
            :name="(option as unknown as GroupOption).label"
            :platform="(option as unknown as GroupOption).platform"
            :subscription-type="(option as unknown as GroupOption).subscriptionType"
            :rate-multiplier="(option as unknown as GroupOption).rate"
            :user-rate-multiplier="(option as unknown as GroupOption).userRate"
            :peak-rate-enabled="(option as unknown as GroupOption).peakRateEnabled"
            :peak-start="(option as unknown as GroupOption).peakStart"
            :peak-end="(option as unknown as GroupOption).peakEnd"
            :peak-rate-multiplier="(option as unknown as GroupOption).peakRateMultiplier"
            :description="(option as unknown as GroupOption).description"
            :selected="selected"
          />
        </template>
      </Select>
      <p v-if="lockedPlatform" class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('keys.samePlatformOnly', { platform: lockedPlatform }) }}
      </p>
    </div>

    <p v-else-if="selectedGroups.length >= maxCount" class="text-xs text-gray-500 dark:text-gray-400">
      {{ t('keys.maxGroupsReached', { max: maxCount }) }}
    </p>

    <p class="input-hint">{{ t('keys.orderedGroupsHint') }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { VueDraggable } from 'vue-draggable-plus'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import GroupOptionItem from '@/components/common/GroupOptionItem.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Group, GroupPlatform, SubscriptionType } from '@/types'

interface GroupOption extends SelectOption {
  value: number
  label: string
  description: string | null
  rate: number
  userRate: number | null
  peakRateEnabled: boolean
  peakStart: string
  peakEnd: string
  peakRateMultiplier: number
  subscriptionType: SubscriptionType
  platform: GroupPlatform
}

const props = withDefaults(
  defineProps<{
    modelValue: number[]
    groups: Group[]
    maxCount?: number
    userGroupRates?: Record<number, number>
  }>(),
  {
    maxCount: 5,
    userGroupRates: () => ({})
  }
)

const emit = defineEmits<{
  'update:modelValue': [value: number[]]
}>()

const { t } = useI18n()

const selectedGroups = ref<Group[]>([])

const groupById = computed(() => {
  const map = new Map<number, Group>()
  for (const g of props.groups) {
    map.set(g.id, g)
  }
  return map
})

const syncFromModel = () => {
  selectedGroups.value = props.modelValue
    .map((id) => groupById.value.get(id))
    .filter((g): g is Group => !!g)
}

watch(
  () => [props.modelValue, props.groups] as const,
  () => syncFromModel(),
  { immediate: true, deep: true }
)

const lockedPlatform = computed<GroupPlatform | null>(() => {
  return selectedGroups.value[0]?.platform ?? null
})

const canAddMore = computed(() => selectedGroups.value.length < props.maxCount)

const addableOptions = computed<GroupOption[]>(() => {
  const selected = new Set(selectedGroups.value.map((g) => g.id))
  const platform = lockedPlatform.value
  return props.groups
    .filter((g) => !selected.has(g.id))
    .filter((g) => !platform || g.platform === platform)
    .map((group) => ({
      value: group.id,
      label: group.name,
      description: group.description,
      rate: group.rate_multiplier,
      userRate: props.userGroupRates?.[group.id] ?? null,
      peakRateEnabled: group.peak_rate_enabled,
      peakStart: group.peak_start,
      peakEnd: group.peak_end,
      peakRateMultiplier: group.peak_rate_multiplier,
      subscriptionType: group.subscription_type,
      platform: group.platform
    }))
})

const emitIds = () => {
  emit(
    'update:modelValue',
    selectedGroups.value.map((g) => g.id)
  )
}

const onAdd = (value: string | number | boolean | null) => {
  if (value === null || value === undefined || value === '') return
  const id = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(id) || selectedGroups.value.some((g) => g.id === id)) return
  if (selectedGroups.value.length >= props.maxCount) return

  const group = groupById.value.get(id)
  if (!group) return
  if (lockedPlatform.value && group.platform !== lockedPlatform.value) return

  selectedGroups.value = [...selectedGroups.value, group]
  emitIds()
}

const removeGroup = (id: number) => {
  selectedGroups.value = selectedGroups.value.filter((g) => g.id !== id)
  emitIds()
}
</script>

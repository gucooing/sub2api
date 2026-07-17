/**
 * Admin API Keys API endpoints
 * Handles API key management for administrators
 */

import { apiClient } from '../client'
import type { ApiKey } from '@/types'

export interface UpdateApiKeyGroupResult {
  api_key: ApiKey
  auto_granted_group_access: boolean
  granted_group_id?: number
  granted_group_name?: string
}

/**
 * Update an API key's group binding (admin).
 * Supports single group_id (legacy) and ordered group_ids when the admin API accepts it.
 * @param id - API Key ID
 * @param groupIdOrIds - Group ID (0/null to unbind), or ordered group_ids array
 * @returns Updated API key with auto-grant info
 */
export async function updateApiKeyGroup(
  id: number,
  groupIdOrIds: number | number[] | null
): Promise<UpdateApiKeyGroupResult> {
  const body: { group_id?: number; group_ids?: number[] } = {}

  if (Array.isArray(groupIdOrIds)) {
    body.group_ids = groupIdOrIds
    // Dual-write primary when non-empty; empty array unbinds via group_id: 0
    body.group_id = groupIdOrIds.length > 0 ? groupIdOrIds[0] : 0
  } else {
    body.group_id = groupIdOrIds === null ? 0 : groupIdOrIds
  }

  const { data } = await apiClient.put<UpdateApiKeyGroupResult>(`/admin/api-keys/${id}`, body)
  return data
}

export const apiKeysAPI = {
  updateApiKeyGroup
}

export default apiKeysAPI

import { nativeConfigMatchesOrigin, type NativeConfig } from './native'

export type PanelPairing = { id: string; device_name: string; expires_at: string; revoked_at?: string; token?: string }

/** Metadata must come from the signed-in account's pairing list, never storage. */
export function matchingPanelPairing(config: NativeConfig | undefined, pairings: PanelPairing[], origin: string, now = Date.now()): PanelPairing | undefined {
  if (!config?.has_token || !config.pairing_id || !nativeConfigMatchesOrigin(config, origin)) return undefined
  return pairings.find(pairing => pairing.id === config.pairing_id
    && pairing.device_name === config.device_name
    && !pairing.revoked_at && Date.parse(pairing.expires_at) > now)
}

import { api } from './client'

export type LiveAccount = { id: string; in_flight: number; max_inflight: number; enabled: boolean }

/** One account-pool slot: occupancy, whether it is resting, and since when. */
export type LiveWorker = {
  id: string
  name: string
  provider: string
  busy: boolean
  in_flight: number
  max_inflight: number
  enabled: boolean
  rate_limited: boolean
  progress: number
  started_at?: string
}

/** One active cooldown: account-wide when model is empty, else model-scoped. */
export type LiveRateLimit = {
  id: string
  label: string
  account_id: string
  model?: string
  until: string
  remaining_ms: number
}

/** A request in the live set: queued while it selects an account, occupied once bound. */
export type LiveJob = {
  id: string
  name: string
  status: string
  started_at?: string
  account_id?: string
}

export type LiveCompletion = { request_id?: string; account_id: string; status: string }

export type LiveSticky = {
  ttl_seconds: number
  capacity: number
  bindings: number
  hits: number
  misses: number
  escapes: number
  rebindings: number
  last_escape_at?: string
  last_escape_reason?: string
  last_miss_reason?: string
}

export type LiveSnapshot = {
  time: string
  in_flight: number
  capacity: number
  rpm: number
  accounts: LiveAccount[]
  workers?: LiveWorker[]
  rate_limits?: LiveRateLimit[]
  sticky?: LiveSticky
  recent_completions?: LiveCompletion[]
  queue?: LiveJob[]
  occupancy?: LiveJob[]
}

export function fetchLive() {
  return api<LiveSnapshot>('/api/live')
}

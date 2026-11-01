import { Card } from '@heroui/react'
import type { LiveJob, LiveSnapshot } from '@/api/live'

type T = (key: string, vars?: Record<string, string | number>) => string

/** How long a job or a slot has been running, in seconds. */
function elapsedSec(startedAt: string | undefined, now: number): number | null {
  if (!startedAt) return null
  const at = Date.parse(startedAt)
  if (Number.isNaN(at)) return null
  return Math.max(0, Math.round((now - at) / 1000))
}

function elapsedLabel(startedAt: string | undefined, now: number, t: T): string {
  const seconds = elapsedSec(startedAt, now)
  if (seconds == null) return t('liveElapsedUnknown')
  if (seconds < 60) return t('liveElapsedSec', { n: seconds })
  return t('liveElapsedMin', { n: Math.floor(seconds / 60), s: seconds % 60 })
}

function countdownLabel(remainingMs: number, t: T): string {
  const seconds = Math.max(0, Math.round(remainingMs / 1000))
  if (seconds >= 60) return t('liveElapsedMin', { n: Math.floor(seconds / 60), s: seconds % 60 })
  return t('liveElapsedSec', { n: seconds })
}

/**
 * What the gateway is doing right now: requests still selecting an account, the
 * ones holding a slot (with the model each is running), and the accounts
 * resting on a cooldown. Everything here is a projection of state the executor
 * and the pool already keep — the panel never infers it from request logs.
 */
export function InflightLiveCard({ live, now, t }: { live: LiveSnapshot | null; now: number; t: T }) {
  const queue: LiveJob[] = live?.queue ?? []
  const occupancy: LiveJob[] = live?.occupancy ?? []
  const rateLimits = live?.rate_limits ?? []
  const workers = (live?.workers ?? []).filter((w) => w.enabled && (w.busy || w.rate_limited))
  const sticky = live?.sticky
  const idle = !queue.length && !occupancy.length && !rateLimits.length

  // One row per account that holds a slot, with the models it is running.
  const byAccount = new Map<string, LiveJob[]>()
  for (const job of occupancy) {
    const key = job.account_id || '—'
    const list = byAccount.get(key) ?? []
    list.push(job)
    byAccount.set(key, list)
  }
  const workerName = new Map((live?.workers ?? []).map((w) => [w.id, w.name]))

  return (
    <Card data-testid="inflight-live-card" className="gap-0 p-3.5">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div className="text-body font-medium text-foreground">{t('liveCardTitle')}</div>
        <div className="flex flex-wrap items-center gap-3 text-caption text-muted">
          <span>
            {t('liveConcurrency')}{' '}
            <span className="mono tabular font-semibold text-foreground">{live?.in_flight ?? '—'}</span>
            <span className="mono tabular">/{live ? live.capacity : '—'}</span>
          </span>
          <span>
            {t('liveRpm')} <span className="mono tabular font-semibold text-foreground">{live?.rpm ?? '—'}</span>
          </span>
          {sticky ? (
            <span title={t('liveStickyHint')}>
              {t('liveSticky')}{' '}
              <span className="mono tabular text-foreground">
                {sticky.hits}/{sticky.hits + sticky.misses}
              </span>
            </span>
          ) : null}
        </div>
      </div>

      {idle ? (
        <div className="rounded-lg bg-surface-secondary px-3 py-2.5 text-caption text-muted">{t('liveIdle')}</div>
      ) : (
        <div className="grid gap-3 lg:grid-cols-2">
          <div className="rounded-lg bg-surface-secondary px-3 py-2.5">
            <div className="mb-2 flex items-center justify-between text-caption text-muted">
              <span>{t('liveQueue')}</span>
              <span className="mono tabular">{queue.length}</span>
            </div>
            {queue.length === 0 ? (
              <div className="text-caption text-muted">—</div>
            ) : (
              <ul className="flex flex-col gap-1.5">
                {queue.slice(0, 6).map((job) => (
                  <li key={job.id} className="flex items-center justify-between gap-2 text-caption">
                    <span className="truncate text-foreground">{job.name}</span>
                    <span className="mono tabular shrink-0 text-muted">{elapsedLabel(job.started_at, now, t)}</span>
                  </li>
                ))}
                {queue.length > 6 ? (
                  <li className="text-caption text-muted">{t('liveMore', { n: queue.length - 6 })}</li>
                ) : null}
              </ul>
            )}
          </div>

          <div className="rounded-lg bg-surface-secondary px-3 py-2.5">
            <div className="mb-2 flex items-center justify-between text-caption text-muted">
              <span>{t('liveOccupancy')}</span>
              <span className="mono tabular">{occupancy.length}</span>
            </div>
            {byAccount.size === 0 ? (
              <div className="text-caption text-muted">—</div>
            ) : (
              <ul className="flex flex-col gap-1.5">
                {[...byAccount.entries()].slice(0, 6).map(([accountID, jobs]) => (
                  <li key={accountID} className="flex items-center justify-between gap-2 text-caption">
                    <span className="truncate text-foreground">
                      {workerName.get(accountID) || accountID}
                      <span className="ml-1.5 text-muted">{jobs.map((job) => job.name).join(' · ')}</span>
                    </span>
                    <span className="mono tabular shrink-0 text-muted">
                      {elapsedLabel(jobs[0]?.started_at, now, t)}
                    </span>
                  </li>
                ))}
                {byAccount.size > 6 ? (
                  <li className="text-caption text-muted">{t('liveMore', { n: byAccount.size - 6 })}</li>
                ) : null}
              </ul>
            )}
          </div>
        </div>
      )}

      {rateLimits.length > 0 ? (
        <div className="mt-3 rounded-lg bg-surface-secondary px-3 py-2.5">
          <div className="mb-2 flex items-center justify-between text-caption text-muted">
            <span>{t('liveRateLimits')}</span>
            <span className="mono tabular">{rateLimits.length}</span>
          </div>
          <ul className="flex flex-wrap gap-x-4 gap-y-1.5">
            {rateLimits.slice(0, 8).map((limit) => (
              <li key={limit.id} className="flex items-center gap-1.5 text-caption">
                <span className="status-dot" data-state="warn" />
                <span className="truncate text-foreground">{limit.label}</span>
                <span className="mono tabular text-muted">{countdownLabel(limit.remaining_ms, t)}</span>
              </li>
            ))}
            {rateLimits.length > 8 ? (
              <li className="text-caption text-muted">{t('liveMore', { n: rateLimits.length - 8 })}</li>
            ) : null}
          </ul>
        </div>
      ) : null}

      {workers.some((w) => w.busy && w.max_inflight > 1) ? (
        <div className="mt-3 flex flex-wrap gap-x-4 gap-y-1.5 text-caption text-muted">
          {workers
            .filter((w) => w.busy)
            .slice(0, 8)
            .map((w) => (
              <span key={w.id} className="inline-flex items-center gap-1.5">
                <span className="truncate text-foreground">{w.name}</span>
                <span className="mono tabular">
                  {w.in_flight}/{w.max_inflight}
                </span>
              </span>
            ))}
        </div>
      ) : null}
    </Card>
  )
}

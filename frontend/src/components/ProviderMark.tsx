import { CommandMark } from '@/components/CommandMark'
import { CodexMark } from '@/components/CodexMark'
import { DevinMark } from '@/components/DevinMark'
import { KimiMark } from '@/components/KimiMark'
import { MuseMark } from '@/components/MuseMark'
import { QoderMark } from '@/components/QoderMark'
import { QwenworkMark } from '@/components/QwenworkMark'
import { TraeMark } from '@/components/TraeMark'
import { WorkBuddyMark } from '@/components/WorkBuddyMark'
import { ZCodeMark } from '@/components/ZCodeMark'
import { ZhipuMark } from '@/components/ZhipuMark'
import { RaccoonMark } from '@/components/RaccoonMark'
import { AntigravityMark } from '@/components/AntigravityMark'
import { AutoClawMark } from '@/components/AutoClawMark'
import { CodeArtsMark } from '@/components/CodeArtsMark'

type Props = {
  provider?: string
  size?: number
  className?: string
}

/** Brand mark for an account provider; unknown values fall back to Qoder. */
export function ProviderMark({ provider, size = 16, className = '' }: Props) {
  const id = String(provider || '').toLowerCase()
  // AutoClaw is two providers sharing one protocol: CN keeps the historical
  // id, the overseas build is autoclaw-intl. Both used to fall through to the
  // Qoder logo, so a 国内版 account was indistinguishable from a Qoder one.
  if (id === 'autoclaw') {
    return <AutoClawMark size={size} className={className} />
  }
  if (id === 'autoclaw-intl') {
    return <AutoClawMark size={size} className={className} intl />
  }
  if (id === 'codearts') {
    return <CodeArtsMark size={size} className={className} />
  }
  if (id === 'workbuddy') {
    return <WorkBuddyMark size={size} className={className} />
  }
  if (id === 'trae') {
    return <TraeMark size={size} className={className} />
  }
  if (id === 'devin') {
    return <DevinMark size={size} className={className} />
  }
  if (id === 'command') {
    return <CommandMark size={size} className={className} />
  }
  if (id === 'codex') {
    return <CodexMark size={size} className={className} />
  }
  if (id === 'zcode') {
    return <ZCodeMark size={size} className={className} />
  }
  if (id === 'zhipu') {
    return <ZhipuMark size={size} className={className} />
  }
  if (id === 'raccoon') {
    return <RaccoonMark size={size} className={className} />
  }
  if (id === 'antigravity') {
    return <AntigravityMark size={size} className={className} />
  }
  if (id === 'qwenwork') {
    return <QwenworkMark size={size} className={className} />
  }
  if (id === 'kimi') {
    return <KimiMark size={size} className={className} />
  }
  if (id === 'meta' || id === 'muse') {
    return <MuseMark size={size} className={className} />
  }
  return <QoderMark size={size} className={className} />
}

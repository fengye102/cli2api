import zCodeMark from '@/assets/zcode-mark.png'

type Props = {
  size?: number
  className?: string
}

// ZCode desktop client mark (the icon zcode.z.ai serves). Z.ai's own wordmark
// is a different, wider lockup, so the client icon is the one that matches the
// service an account signs in to.
export function ZCodeMark({ size = 16, className = '' }: Props) {
  return (
    <img
      src={zCodeMark}
      alt=""
      width={size}
      height={size}
      className={`block shrink-0 rounded-[22%] ${className}`.trim()}
      draggable={false}
    />
  )
}

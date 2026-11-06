type Props = {
  size?: number
  className?: string
}

// MiniMax Code mark: an "M" whose two strokes fold into each other, which is
// the shape the vendor uses. Unknown providers used to fall back to the Qoder
// logo, so a MiniMax account was indistinguishable from a Qoder one.
export function MiniMaxMark({ size = 16, className = '' }: Props) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={`block shrink-0 ${className}`.trim()}
      aria-hidden="true"
    >
      <rect x="1.5" y="1.5" width="21" height="21" rx="6" fill="#0F1014" />
      <path
        fill="none"
        stroke="#FFFFFF"
        strokeWidth="2.1"
        strokeLinecap="round"
        strokeLinejoin="round"
        d="M5.6 17V8.2l6.4 6.6 6.4-6.6V17"
      />
      <circle cx="12" cy="18.4" r="1.35" fill="#FF5A3C" />
    </svg>
  )
}

type Props = {
  size?: number
  className?: string
}

// Antigravity (Google AI IDE) mark: a stylized orbit / spark glyph.
export function AntigravityMark({ size = 16, className = '' }: Props) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={`block shrink-0 text-foreground ${className}`.trim()}
      aria-hidden="true"
    >
      <path
        fill="currentColor"
        fillRule="evenodd"
        d="M12 2.5a9.5 9.5 0 1 0 0 19a9.5 9.5 0 0 0 0-19Zm0 2a7.5 7.5 0 1 1 0 15a7.5 7.5 0 0 1 0-15Zm0 3a4.5 4.5 0 1 0 0 9a4.5 4.5 0 0 0 0-9Zm0 2a2.5 2.5 0 1 1 0 5a2.5 2.5 0 0 1 0-5ZM12 1.5c1 0 1.8.2 2.6.6a.9.9 0 0 1-1.4 1.1A4.4 4.4 0 0 0 12 3.3c-1 0-1.9.3-2.7.8a.9.9 0 1 1-1-1.5A6 6 0 0 1 12 1.5Z"
      />
    </svg>
  )
}

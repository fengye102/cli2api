type Props = {
  size?: number
  className?: string
}

// Raccoon (小浣熊 / SenseTime) mark: a simple raccoon-mask silhouette drawn
// from two rounded chevrons, rendered as a currentColor glyph.
export function RaccoonMark({ size = 16, className = '' }: Props) {
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
        d="M7.2 3.2a2 2 0 0 1 3.4 0l1.4 2.1a2 2 0 0 1 0 0l1.4-2.1a2 2 0 0 1 3.4 0l2.2 4.4c.2.4.2.9 0 1.3l-3.6 7.2a5 5 0 0 1-4.5 2.7a5 5 0 0 1-4.5-2.7L5 8.9a2 2 0 0 1 0-1.3l2.2-4.4Zm4.8 4.9a2.6 2.6 0 0 0-2.6 2.6v.6a2.6 2.6 0 0 0 5.2 0v-.6a2.6 2.6 0 0 0-2.6-2.6Zm-1.9 5.9a1.1 1.1 0 0 0 0 2.2a1.1 1.1 0 0 0 0-2.2Zm3.8 0a1.1 1.1 0 0 0 0 2.2a1.1 1.1 0 0 0 0-2.2Z"
      />
    </svg>
  )
}

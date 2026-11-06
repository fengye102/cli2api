type Props = {
  size?: number
  className?: string
}

// Runable mark: a runner's chevron pair inside a rounded tile. Unknown
// providers used to fall back to the Qoder logo, so a Runable account looked
// like a Qoder one.
export function RunableMark({ size = 16, className = '' }: Props) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={`block shrink-0 ${className}`.trim()}
      aria-hidden="true"
    >
      <rect x="1.5" y="1.5" width="21" height="21" rx="6" fill="#111827" />
      <path
        fill="none"
        stroke="#7DD3FC"
        strokeWidth="2.2"
        strokeLinecap="round"
        strokeLinejoin="round"
        d="m8.4 5.8 5.6 6.2-5.6 6.2"
      />
      <path
        fill="none"
        stroke="#F8FAFC"
        strokeWidth="2.2"
        strokeLinecap="round"
        strokeLinejoin="round"
        d="m13.6 5.8 3.4 6.2-3.4 6.2"
        opacity="0.75"
      />
    </svg>
  )
}

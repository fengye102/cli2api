type Props = {
  size?: number
  className?: string
}

// Custom relay mark: a hub node with two opposite arrows, i.e. "traffic goes
// out to whatever address you gave us". Every unknown provider used to fall
// back to the Qoder logo, which made a relay account look like a Qoder one.
export function CustomMark({ size = 16, className = '' }: Props) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={`block shrink-0 text-foreground ${className}`.trim()}
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="2.6" fill="currentColor" />
      <path
        fill="none"
        stroke="currentColor"
        strokeWidth="1.9"
        strokeLinecap="round"
        strokeLinejoin="round"
        d="M9.1 9.1 5.4 5.4M5.4 9.6V5.4h4.2"
      />
      <path
        fill="none"
        stroke="currentColor"
        strokeWidth="1.9"
        strokeLinecap="round"
        strokeLinejoin="round"
        d="m14.9 14.9 3.7 3.7M18.6 14.4v4.2h-4.2"
      />
    </svg>
  )
}

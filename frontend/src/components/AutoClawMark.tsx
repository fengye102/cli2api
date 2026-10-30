type Props = {
  size?: number
  className?: string
  /** Overseas build: same glyph, different accent so the two channels read
   *  apart in the account list at a glance. */
  intl?: boolean
}

// AutoClaw mark: a claw gripping a rounded core, which is the shape the
// desktop client uses for itself. Drawn inline rather than shipped as a PNG so
// the mark follows the theme's foreground colour like every other provider.
//
// The CN and Intl builds share one glyph on purpose: they are the same product
// on two hosts, and the console already labels them 国内版 / 国际版. Only the
// accent differs, so a mixed account list stays distinguishable without
// inventing a second logo the upstream does not have.
export function AutoClawMark({ size = 16, className = '', intl = false }: Props) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={`block shrink-0 text-foreground ${className}`.trim()}
      aria-hidden="true"
    >
      {/* Three talons sweeping in from the left, plus the gripped core. */}
      <path
        fill="currentColor"
        fillRule="evenodd"
        d="M3.1 6.9c-.5-.3-1.1-.1-1.4.4s-.1 1.1.4 1.4l4.3 2.4c.3.2.5.5.5.9s-.2.7-.5.9L2.1 15.3c-.5.3-.7.9-.4 1.4s.9.7 1.4.4l4.6-2.6c1.3-.7 2.1-2.1 2.1-3.6s-.8-2.9-2.1-3.6L3.1 6.9Z"
      />
      <path
        fill="currentColor"
        fillRule="evenodd"
        d="M10.2 4.6c-.6-.2-1.2.1-1.4.7s.1 1.2.7 1.4l3.6 1.2c.4.1.7.5.7.9s-.3.8-.7.9l-3.6 1.2c-.6.2-.9.8-.7 1.4s.8.9 1.4.7l3.9-1.3c1.5-.5 2.5-1.9 2.5-3.5s-1-3-2.5-3.5l-3.9-1.1Z"
      />
      <circle
        fill="currentColor"
        cx="18.6"
        cy="12"
        r={intl ? 4.1 : 3.6}
        opacity={intl ? 1 : 0.92}
      />
      {/* Intl gets a ring so the two marks differ by more than diameter. */}
      {intl ? (
        <circle
          fill="none"
          stroke="currentColor"
          strokeWidth="1.2"
          cx="18.6"
          cy="12"
          r="5.6"
          opacity="0.55"
        />
      ) : null}
    </svg>
  )
}

type Props = {
  size?: number
  className?: string
}

// CodeArts (华为云码道) mark: the CodeArts "code flow" glyph - a bracketed
// chevron pair around a rising stroke, which is the motif Huawei uses for the
// CodeArts line. Inline SVG so it follows the theme foreground like the other
// provider marks; the console previously fell back to the Qoder logo, which
// made a 码道 account look like a Qoder account.
export function CodeArtsMark({ size = 16, className = '' }: Props) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={`block shrink-0 text-foreground ${className}`.trim()}
      aria-hidden="true"
    >
      {/* Left chevron */}
      <path
        fill="none"
        stroke="currentColor"
        strokeWidth="2.1"
        strokeLinecap="round"
        strokeLinejoin="round"
        d="M8.4 6.2 3.9 12l4.5 5.8"
      />
      {/* Right chevron */}
      <path
        fill="none"
        stroke="currentColor"
        strokeWidth="2.1"
        strokeLinecap="round"
        strokeLinejoin="round"
        d="M15.6 6.2 20.1 12l-4.5 5.8"
      />
      {/* Rising stroke between the chevrons */}
      <path
        fill="none"
        stroke="currentColor"
        strokeWidth="1.9"
        strokeLinecap="round"
        d="M9.7 15.4 12 11.6l2.3 3.8"
        opacity="0.75"
      />
    </svg>
  )
}

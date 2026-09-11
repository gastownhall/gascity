interface PartialDataNoticeProps {
  label: string;
  title: string;
  show?: boolean;
  /**
   * Optional leading status glyph (aria-hidden), so the indicator reads as
   * glyph + word per DESIGN.md "status = glyph + word, never color alone".
   * gascity-dashboard-2j8e.2: the Runs partial indicator pairs ◐ with "partial"
   * so a partial fan-out is legible in greyscale, not carried by the warn tint.
   */
  glyph?: string;
}

export function PartialDataNotice({ label, title, show = true, glyph }: PartialDataNoticeProps) {
  if (!show) return null;

  return (
    <span className="normal-case text-body text-warn" role="status" title={title}>
      {glyph !== undefined && <span aria-hidden="true">{glyph} </span>}
      {label}
    </span>
  );
}

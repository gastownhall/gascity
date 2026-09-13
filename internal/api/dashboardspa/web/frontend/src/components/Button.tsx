import type { ButtonHTMLAttributes, ReactNode } from 'react';

// Buttons are typeset, not boxed. The default carries a hairline
// border and reads as a deliberate control. The 'quiet' variant has
// no border and reads as a text link — used for inline list actions
// (claim / close / nudge / peek). The 'accent' variant is reserved
// for the rare loud moment (destructive confirms, primary CTA).

export type ButtonTone = 'default' | 'accent' | 'quiet';
export type ButtonSize = 'sm' | 'md';

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  tone?: ButtonTone;
  size?: ButtonSize;
  children: ReactNode;
}

const TONE: Record<ButtonTone, string> = {
  default: 'border border-rule text-fg-muted hover:text-fg hover:bg-surface-tint',
  accent: 'border border-accent text-accent hover:bg-accent hover:text-surface',
  quiet: 'border border-transparent text-fg-muted hover:text-fg',
};

const SIZE: Record<ButtonSize, string> = {
  sm: 'px-2.5 py-1 text-label uppercase tracking-wider',
  md: 'px-3.5 py-1.5 text-body',
};

export function Button({
  tone = 'default',
  size = 'sm',
  className = '',
  children,
  ...rest
}: ButtonProps) {
  return (
    <button
      {...rest}
      className={`inline-flex items-center gap-1.5 rounded-sm transition-colors duration-150 ease-out-quart focus-mark disabled:opacity-40 disabled:cursor-not-allowed ${TONE[tone]} ${SIZE[size]} ${className}`}
    >
      {children}
    </button>
  );
}

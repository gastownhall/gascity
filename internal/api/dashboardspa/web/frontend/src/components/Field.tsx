import type { ReactNode } from 'react';

interface FieldProps {
  label: string;
  children: ReactNode;
  variant?: 'definition' | 'form';
}

export function Field({ label, children, variant = 'definition' }: FieldProps) {
  if (variant === 'form') {
    return (
      <label className="block space-y-1.5">
        <span className="text-label uppercase tracking-wider text-fg-muted">{label}</span>
        {children}
      </label>
    );
  }

  return (
    <div>
      <dt className="text-label uppercase tracking-wider text-fg-faint mb-1">{label}</dt>
      <dd className="text-body text-fg">{children}</dd>
    </div>
  );
}

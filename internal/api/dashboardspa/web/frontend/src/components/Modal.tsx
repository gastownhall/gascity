import { useEffect, type ReactNode } from 'react';

interface ModalProps {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  /** Optional caption next to the title. */
  caption?: ReactNode;
  children: ReactNode;
  /** Optional footer slot (e.g. action buttons). */
  footer?: ReactNode;
  /** When set, render the modal at the given max-width class instead of the default. */
  widthClass?: string;
}

// Modals are last-resort per DESIGN.md. When they appear (Session Peek
// is the only legitimate use here), they sit on a hairline-bounded
// panel, opaque against a tinted scrim. No glassmorphism.
export function Modal({
  open,
  onClose,
  title,
  caption,
  children,
  footer,
  widthClass = 'max-w-3xl',
}: ModalProps) {
  useEffect(() => {
    if (!open) return;
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', handleKey);
    return () => document.removeEventListener('keydown', handleKey);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div
      role="dialog"
      aria-modal="true"
      className="fixed inset-0 z-50 flex items-start sm:items-center justify-center bg-fg/30 p-3 sm:p-6"
      onClick={onClose}
    >
      <div
        className={`w-full ${widthClass} bg-surface border border-rule rounded-md flex flex-col max-h-[90vh]`}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-3 px-5 py-4 border-b border-rule">
          <div className="min-w-0">
            <h2 className="text-title font-semibold text-fg truncate">{title}</h2>
            {caption && (
              <p className="text-label uppercase tracking-wider text-fg-muted mt-1 truncate">
                {caption}
              </p>
            )}
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="text-fg-muted hover:text-fg transition-colors duration-150 ease-out-quart focus-mark text-lg leading-none px-1"
          >
            ×
          </button>
        </div>
        <div className="flex-1 overflow-auto p-5 text-body text-fg">{children}</div>
        {footer && (
          <div className="border-t border-rule px-5 py-3 flex items-center justify-end gap-3">
            {footer}
          </div>
        )}
      </div>
    </div>
  );
}

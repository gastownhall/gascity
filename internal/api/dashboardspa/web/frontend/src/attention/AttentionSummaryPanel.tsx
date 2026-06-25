import { Fragment } from 'react';
import { Link } from 'react-router-dom';
import { useAttentionModel } from './context';
import { attentionDomainHref, attentionDomainLabel } from './labels';
import type { AttentionItem, AttentionSeverity } from './compose';

export function AttentionSummaryPanel() {
  const attention = useAttentionModel();
  if (attention.items.length === 0) return null;

  return (
    <section aria-labelledby="attention-summary-title" className="space-y-3">
      <h2 id="attention-summary-title" className="text-headline font-semibold text-fg">
        Attention
      </h2>
      <ul className="space-y-2">
        {attention.topItems.map((item) => (
          <li
            key={`${item.domain}:${item.id}`}
            className="text-body text-fg flex items-baseline gap-3"
          >
            <AttentionTitle item={item} />
            <span className={`text-label uppercase tracking-wider ${severityClass(item.severity)}`}>
              {attentionDomainLabel(item.domain)}
            </span>
          </li>
        ))}
      </ul>
      {attention.overflowByDomain.length > 0 && (
        <p className="text-label uppercase tracking-wider text-fg-muted">
          {attention.overflowByDomain.map((group, index) => (
            <Fragment key={group.domain}>
              {index > 0 && ' · '}
              <Link to={attentionDomainHref(group.domain)} className="hover:text-fg focus-mark">
                {group.total} more in {attentionDomainLabel(group.domain)}
              </Link>
            </Fragment>
          ))}
        </p>
      )}
    </section>
  );
}

function AttentionTitle({ item }: { item: AttentionItem }) {
  if (item.href === undefined) {
    return <span className="font-medium">{item.title}</span>;
  }
  return (
    <Link to={item.href} className="font-medium hover:text-fg focus-mark">
      {item.title}
    </Link>
  );
}

function severityClass(severity: AttentionSeverity): string {
  switch (severity) {
    case 'attention':
      return 'text-accent';
    case 'watch':
      return 'text-warn';
    case 'unavailable':
      return 'text-fg-muted';
  }
}

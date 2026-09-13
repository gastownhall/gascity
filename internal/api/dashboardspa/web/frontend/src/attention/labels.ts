import type { AttentionDomain } from './compose';

const DOMAIN_LABELS: Record<AttentionDomain, string> = {
  agents: 'Agents',
  beads: 'Beads',
  runs: 'Runs',
  mail: 'Mail',
  activity: 'Activity',
  health: 'Health',
};

const DOMAIN_HREFS: Record<AttentionDomain, string> = {
  agents: '/agents',
  beads: '/beads',
  runs: '/runs',
  mail: '/mail',
  activity: '/activity',
  health: '/health',
};

export function attentionDomainLabel(domain: AttentionDomain): string {
  return DOMAIN_LABELS[domain];
}

export function attentionDomainHref(domain: AttentionDomain): string {
  return DOMAIN_HREFS[domain];
}

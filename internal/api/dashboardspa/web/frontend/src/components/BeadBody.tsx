import type { SupervisorBead } from '../supervisor/beadReads';
import { formatDateTime } from '../lib/format';
import { Field } from './Field';
import { beadStatusTone, StatusBadge } from './StatusBadge';

// The read-only body of a single bead: kind banner, status grid, template
// origin, labels, description. Extracted from BeadDetailModal so the Beads
// board detail rail (gascity-dashboard-6frc) renders identical detail
// without the Modal chrome. The modal and the rail are the two callers.

// Gc has three bead shapes the body needs to distinguish, all clarified
// by the mayor (and not by the wire shape, which calls every one "task"):
//
//   1. Formula template: metadata['gc.kind'] === 'run'. NOT actionable
//      work — it's the recipe every wisp is instantiated from.
//      `status=in_progress` is the gc-system convention for "this template
//      is live and available". `gc.source_bead_id` is the historical
//      authoring origin, NOT the work this bead tracks. `gc.run_target`
//      may be stale. Leave it alone. It's plumbing.
//
//   2. Wisp / molecule instance: issue_type === 'molecule'. A single run
//      of a formula. Description usually empty.
//
//   3. Regular work bead: everything else.
interface RunMeta {
  kind?: string;
  originBeadId?: string;
  formulaContract?: string;
  runTarget?: string;
}

function readRunMeta(bead: SupervisorBead): RunMeta {
  // Supervisor Bead metadata is Record<string, string> per OpenAPI, so values are
  // guaranteed strings. Truthy check on the key suffices.
  const md = bead.metadata;
  if (!md) return {};
  const runMeta: RunMeta = {};
  if (md['gc.kind']) runMeta.kind = md['gc.kind'];
  if (md['gc.source_bead_id']) runMeta.originBeadId = md['gc.source_bead_id'];
  if (md['gc.formula_contract']) {
    runMeta.formulaContract = md['gc.formula_contract'];
  }
  if (md['gc.run_target']) runMeta.runTarget = md['gc.run_target'];
  else if (md['gc.routed_to']) runMeta.runTarget = md['gc.routed_to'];
  return runMeta;
}

type BeadKind = 'template' | 'wisp' | 'work';

function classifyBead(bead: SupervisorBead, wf: RunMeta): BeadKind {
  if (wf.kind === 'run') return 'template';
  if (bead.issue_type === 'molecule') return 'wisp';
  return 'work';
}

export function BeadBody({ bead }: { bead: SupervisorBead }) {
  const wf = readRunMeta(bead);
  const kind = classifyBead(bead, wf);

  return (
    <div className="space-y-8">
      {kind === 'template' && (
        <section>
          <h3 className="text-label uppercase tracking-wider text-fg-faint mb-3">
            Formula template
          </h3>
          <p className="text-body text-fg-muted max-w-prose">
            This bead is a recipe, not actionable work. Every{' '}
            {bead.ref ? <code className="text-fg-muted">{bead.ref}</code> : 'wisp'} instance is
            instantiated from this template. The <span className="text-fg-muted">in_progress</span>{' '}
            status is the gc-system convention for {'"'}available for instantiation{'"'} — do not
            act on it, nudge it, or close it.
          </p>
        </section>
      )}

      {kind === 'wisp' && (
        <section>
          <h3 className="text-label uppercase tracking-wider text-fg-faint mb-3">
            Formula instance
          </h3>
          <p className="text-body text-fg-muted max-w-prose">
            One run of the{' '}
            {bead.title ? <code className="text-fg-muted">{bead.title}</code> : 'formula'} recipe.
          </p>
        </section>
      )}

      <dl className="grid grid-cols-2 sm:grid-cols-4 gap-x-8 gap-y-5">
        <Field label="Status">
          <StatusBadge tone={beadStatusTone(bead.status)} label={bead.status} />
        </Field>
        <Field label="Type">{bead.issue_type}</Field>
        <Field label="Assignee">{bead.assignee || '·'}</Field>
        <Field label="Created">
          <span className="tnum">{formatDateTime(bead.created_at)}</span>
        </Field>
      </dl>

      {kind === 'template' && (wf.formulaContract || wf.originBeadId || wf.runTarget) && (
        <section>
          <h3 className="text-label uppercase tracking-wider text-fg-faint mb-3">
            Template origin
          </h3>
          <p className="text-body text-fg-muted max-w-prose mb-4">
            Where this formula came from, kept for traceability. The origin bead and target may be
            stale; the formula itself is now used wherever the pool dispatches it.
          </p>
          <dl className="grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-3">
            {wf.formulaContract && (
              <Field label="Contract">
                <code className="text-fg-muted">{wf.formulaContract}</code>
              </Field>
            )}
            {bead.ref && (
              <Field label="Ref">
                <code className="text-fg-muted">{bead.ref}</code>
              </Field>
            )}
            {wf.originBeadId && (
              <Field label="Origin bead">
                <code className="text-fg-muted">{wf.originBeadId}</code>
              </Field>
            )}
            {wf.runTarget && (
              <Field label="Origin target">
                <span className="text-fg-muted truncate" title={wf.runTarget}>
                  {wf.runTarget}
                </span>
              </Field>
            )}
          </dl>
        </section>
      )}

      {Array.isArray(bead.labels) && bead.labels.length > 0 && (
        <section>
          <h3 className="text-label uppercase tracking-wider text-fg-faint mb-3">Labels</h3>
          <div className="flex flex-wrap gap-x-3 gap-y-1">
            {bead.labels.map((l) => (
              <span key={l} className="text-label uppercase tracking-wider text-fg-muted">
                {l}
              </span>
            ))}
          </div>
        </section>
      )}

      <section>
        <h3 className="text-label uppercase tracking-wider text-fg-faint mb-3">
          {kind === 'template' ? 'Recipe' : 'Description'}
        </h3>
        {bead.description && bead.description.length > 0 ? (
          <pre className="text-body whitespace-pre-wrap leading-relaxed text-fg font-sans">
            {bead.description}
          </pre>
        ) : (
          <p className="text-body text-fg-muted italic">No description.</p>
        )}
      </section>
    </div>
  );
}
